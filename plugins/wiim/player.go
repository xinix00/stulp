package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/wiim/internal/wiim"
)

// De speler: afspelen, volume, wat er klinkt, en de vier voorkeurtoetsen.

// pollInterval is hoe vaak er naar een speler gekeken wordt.
//
// Vijf seconden komt uit de bron (`device.js`, `setInterval(…, 5000)`) en is
// hier overgenomen omdat het de goede afweging is. Eén ronde is één klein
// SOAP-verzoek dat de speler in milliseconden beantwoordt, dus de speler merkt
// er niets van. Trager en de tegel loopt zichtbaar achter: wie in de WiiM-app
// het volume verzet of een nummer verder springt wil dat niet een halve minuut
// later terugzien. Sneller levert niets op — de tijd staat in hele seconden en
// er is niets fijnkorreliger om te tonen.
//
// Er is geen weg waarop de speler zichzelf meldt. UPnP kent daar abonnementen
// voor, en de bron heeft ze geprobeerd en weer uitgezet ("FIXME: Workaround
// until subscribe > renew is fixed", `device.js`). Wat kapot was in de bron
// wordt hier niet nagebouwd; zie PORTED.md.
const pollInterval = 5 * time.Second

// pollTimeout houdt één ronde korter dan het ritme, zodat rondes elkaar nooit
// inhalen op een speler die traag antwoordt.
const pollTimeout = 4 * time.Second

// failuresBeforeUnavailable is hoeveel mislukte rondes er nodig zijn voordat de
// tegel grijs wordt.
//
// Eén gemiste ronde zegt niets: wifi laat pakketten vallen en een speler die
// van bron wisselt is even weg. Drie op rij is vijftien seconden stilte, en dan
// is er echt iets aan de hand.
const failuresBeforeUnavailable = 3

type playerDriver struct{}

func (playerDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	uuid, _ := device.Data()["id"].(string)
	if uuid == "" {
		return nil, fmt.Errorf("dit apparaat draagt geen WiiM-uuid; koppel de speler opnieuw")
	}
	return &player{device: device, uuid: uuid}, nil
}

type player struct {
	device *appsdk.Device
	uuid   string

	mu       sync.Mutex
	client   *wiim.Client
	cancel   context.CancelFunc
	failures int
	lastErr  string
	lastOK   time.Time
	// loopComplaint onthoudt over welke loopmode al geklaagd is. Zonder dat
	// staat er elke vijf seconden dezelfde regel in de log.
	loopComplaint string
}

// OnInit start de ronde en wacht nergens op: een speler die uit staat zou
// anders elk ander apparaat van deze app ophouden.
func (p *player) OnInit() error {
	instance.watch(p.device.ID(), p)
	p.restart()
	return nil
}

func (p *player) OnDeleted() {
	instance.forget(p.device.ID())
	p.halt()
}

// OnSettings: het adres is het enige dat iemand hier kan verzetten, en het is
// precies wat verandert als de router een ander adres uitdeelt.
func (p *player) OnSettings(changed map[string]any) error {
	if _, touched := changed["address"]; !touched {
		return nil
	}
	address, _ := changed["address"].(string)
	if err := wiim.CheckAddress(address); err != nil {
		// Weigeren en niet stilzwijgend bewaren: anders staat er een adres in
		// de instellingen waar nooit iets op zal antwoorden.
		return err
	}
	p.restart()
	return nil
}

// address is waar deze speler staat.
func (p *player) address() string {
	value, _ := p.device.Setting("address")
	address, _ := value.(string)
	return address
}

// restart zet de ronde opnieuw op met wat er nu in de instellingen staat.
func (p *player) restart() {
	p.halt()

	address := p.address()
	if err := wiim.CheckAddress(address); err != nil {
		p.device.SetUnavailable("Er is geen bruikbaar adres ingesteld: " + err.Error())
		return
	}
	client := wiim.New(address)
	// De poort waar de beschrijving stond komt van het koppelen. Bijna altijd
	// 49152, maar wat de speler zelf zei wint van wat wij zouden aannemen.
	if value, ok := p.device.StoreValue("port"); ok {
		if port, ok := value.(float64); ok && port > 0 {
			client.Port = int(port)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.client, p.cancel, p.failures = client, cancel, 0
	p.mu.Unlock()

	go p.run(ctx)
}

func (p *player) halt() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *player) run(ctx context.Context) {
	p.round(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.round(ctx)
		}
	}
}

func (p *player) round(parent context.Context) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, pollTimeout)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		if parent.Err() != nil {
			return // deze speler is gestopt of kreeg een ander adres
		}
		p.failed(err)
		return
	}
	p.succeeded()
	p.apply(status)
}

// failed telt een mislukte ronde. De waarden blijven staan op wat ze het laatst
// waren: een speler die even niet antwoordt zegt niets over wat er speelde.
func (p *player) failed(err error) {
	p.mu.Lock()
	p.failures++
	failures := p.failures
	p.lastErr = err.Error()
	p.mu.Unlock()

	// Alleen op de drempel melden. Elke ronde dezelfde regel in de log zetten
	// maakt de log onleesbaar op precies het moment dat je hem nodig hebt.
	if failures == failuresBeforeUnavailable {
		p.device.SetUnavailable("De speler op " + p.address() + " antwoordt niet: " + err.Error())
		p.device.Error(err.Error())
	}
}

func (p *player) succeeded() {
	p.mu.Lock()
	first := p.failures >= failuresBeforeUnavailable
	p.failures = 0
	p.lastErr = ""
	p.lastOK = time.Now()
	p.mu.Unlock()

	if first || !p.device.Available() {
		p.device.SetAvailable()
	}
}

// apply zet de stand van de speler op de tegel.
func (p *player) apply(status wiim.Status) {
	values := map[string]any{"speaker_playing": status.Playing()}

	if status.Loop.Known {
		values["speaker_shuffle"] = status.Loop.Shuffle
		values["speaker_repeat"] = status.Loop.Repeat
		p.mu.Lock()
		p.loopComplaint = ""
		p.mu.Unlock()
	} else {
		p.complainOnce(status.Loop.Raw, "de speler meldt loopmode "+status.Loop.Raw+
			"; die stand kent deze app niet, dus shuffle en herhalen blijven staan")
	}

	// speaker_position komt alleen van de speler, en telt tussen twee rondes
	// niet door. Dat is een keuze: doortellen is een waarde verzinnen. Hij
	// klopt niet meer zodra iemand in de WiiM-app spoelt, bij een gapless
	// overgang, bij bufferen of bij een stream die stilvalt — en dat is precies
	// het soort waarde dat er goed uitziet en niet waar is. De tegel loopt
	// daardoor in stappen van vijf seconden; wat er staat is wat de speler zei.
	if status.Position.Known {
		values["speaker_position"] = status.Position.Value
	}
	if status.Duration.Known {
		values["speaker_duration"] = status.Duration.Value
	}

	values["volume_set"] = status.Volume
	values["volume_mute"] = status.Muted

	// Zonder metadata hoort er niets te staan in plaats van wat er tien minuten
	// geleden speelde.
	values["speaker_artist"] = status.Track.Artist
	values["speaker_album"] = status.Track.Album
	values["speaker_track"] = status.Track.Title
	p.commit(values)
}

// commit schrijft alle gewijzigde velden uit één spelerantwoord tegelijk.
//
// Elke commit is een verzoek aan Stulp en een regel in de geschiedenis.
// Een speler die stilstaat hoeft niet elke vijf seconden opnieuw te melden dat
// hij hetzelfde nummer niet speelt.
func (p *player) commit(values map[string]any) {
	changed := make(map[string]any, len(values))
	for name, value := range values {
		if old, known := p.device.CapabilityValue(name); !known || old != value {
			changed[name] = value
		}
	}
	if err := p.device.SetCapabilityValues(changed); err != nil {
		p.device.Error(err.Error())
	}
}

func (p *player) complainOnce(key, message string) {
	p.mu.Lock()
	already := p.loopComplaint == key
	p.loopComplaint = key
	p.mu.Unlock()
	if !already {
		p.device.Error(message)
	}
}

// ---------------------------------------------------------------------------
// Opdrachten
// ---------------------------------------------------------------------------

// command voert iets uit op de speler, of zegt waarom dat niet kan.
func (p *player) command(ctx context.Context, run func(*wiim.Client) error) error {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return fmt.Errorf("deze speler heeft geen bruikbaar adres; vul het in bij de instellingen van het apparaat")
	}
	return run(client)
}

func (p *player) OnCapability(name string, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	switch name {
	case "speaker_playing":
		playing, _ := value.(bool)
		if err := p.command(ctx, func(client *wiim.Client) error {
			if playing {
				return client.Play(ctx)
			}
			return client.Pause(ctx)
		}); err != nil {
			return err
		}
		// Niet vooruitlopen: de volgende ronde meldt wat de speler werkelijk
		// doet. Hier alvast omzetten laat de tegel springen zodra dat binnenkomt.
		return nil

	case "speaker_next":
		return p.command(ctx, func(client *wiim.Client) error { return client.Next(ctx) })

	case "speaker_prev":
		return p.command(ctx, func(client *wiim.Client) error { return client.Previous(ctx) })

	case "speaker_shuffle":
		shuffle, _ := value.(bool)
		repeat, err := p.text("speaker_repeat")
		if err != nil {
			return err
		}
		return p.setLoop(ctx, shuffle, repeat, name, shuffle)

	case "speaker_repeat":
		repeat, _ := value.(string)
		shuffle, err := p.flag("speaker_shuffle")
		if err != nil {
			return err
		}
		return p.setLoop(ctx, shuffle, repeat, name, repeat)

	case "volume_set":
		level, _ := value.(float64)
		if err := p.command(ctx, func(client *wiim.Client) error { return client.SetVolume(ctx, level) }); err != nil {
			return err
		}
		// Ook hier niets terugmelden. Dat de speler alleen hele procenten kent
		// blijft waar -- maar hij meldt zijn eigen volume in de volgende ronde,
		// en dat is nauwkeuriger dan wat wij dachten te versturen.
		return nil

	case "volume_mute":
		muted, _ := value.(bool)
		if err := p.command(ctx, func(client *wiim.Client) error { return client.SetMute(ctx, muted) }); err != nil {
			return err
		}
		return nil

	case "button.off":
		return p.command(ctx, func(client *wiim.Client) error { return client.Stop(ctx) })

	case "button.preset1", "button.preset2", "button.preset3", "button.preset4":
		number := int(name[len(name)-1] - '0')
		return p.command(ctx, func(client *wiim.Client) error { return client.Preset(ctx, number) })
	}
	return fmt.Errorf("deze speler kent %q niet", name)
}

// setLoop stuurt shuffle en herhalen samen, want bij WiiM zijn dat één getal.
func (p *player) setLoop(ctx context.Context, shuffle bool, repeat, name string, value any) error {
	mode, err := wiim.LoopMode(shuffle, repeat)
	if err != nil {
		return err
	}
	if err := p.command(ctx, func(client *wiim.Client) error { return client.SetLoopMode(ctx, mode) }); err != nil {
		return err
	}
	// Niet vooruitlopen op de speler: de volgende ronde meldt wat hij werkelijk
	// doet. Hier alvast omzetten laat de tegel springen zodra dat binnenkomt.
	return nil
}

// text en flag lezen de andere helft van de loopmode.
//
// Is die er nog niet, dan is dit een speler die net gestart is en nog geen
// ronde heeft gehad. Een stand aannemen zou hier betekenen dat een druk op
// shuffle stilletjes het herhalen uitzet; dus zegt de app wat er aan de hand is
// en probeert de gebruiker het vijf seconden later opnieuw.
func (p *player) text(capability string) (string, error) {
	value, known := p.device.CapabilityValue(capability)
	text, ok := value.(string)
	if !known || !ok {
		return "", fmt.Errorf("de speler heeft nog niet gemeld hoe hij herhaalt; probeer het zo nog eens")
	}
	return text, nil
}

func (p *player) flag(capability string) (bool, error) {
	value, known := p.device.CapabilityValue(capability)
	flag, ok := value.(bool)
	if !known || !ok {
		return false, fmt.Errorf("de speler heeft nog niet gemeld of hij shuffelt; probeer het zo nog eens")
	}
	return flag, nil
}
