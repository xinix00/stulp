package tahoma

import (
	"context"
	"sync"
	"time"
)

// De poll is waar standen vandaan komen.
//
// Dit is niet de opzet die je zou kiezen. Je zou je één keer aanmelden voor
// gebeurtenissen en daarna alleen nog horen wat er veranderde. De bron doet dat
// niet: lib/sync.js haalt op een vaste tik de hele /setup op en laat elk
// apparaat zichzelf eruit zoeken. Een gebeurtenis-eindpunt komt in de hele bron
// niet voor -- niet in lib/, niet in de drivers, niet in app.json. Zie
// PORTED.md; verzinnen is geen optie, want een eindpunt dat je niet kunt lezen
// kun je ook niet goed raden.
//
// Wat hier wél anders is dan in de bron: er wordt vergeleken. Elke ronde levert
// alle apparaten, maar alleen wie iets anders meldt dan de vorige keer wordt
// doorgegeven. Een rolluik dat stilstaat hoort niet elke tien seconden opnieuw
// dezelfde stand in de geschiedenis te zetten.

// DefaultInterval is hoe vaak er gepolld wordt als niemand iets anders zegt.
// Tien seconden komt uit app.js van de bron (INITIAL_SYNC_INTERVAL).
const DefaultInterval = 10 * time.Second

// MinInterval is de ondergrens.
//
// De bron laat de gebruiker elk getal invullen, tot en met nul. Dat is een
// account dat door Somfy geblokkeerd wordt, en het is niet te zien waaraan het
// lag. Eén seconde is al veel voor een cloud die aan de andere kant van het land
// staat; korter is nooit een oplossing voor iets.
const MinInterval = time.Second

// Handlers is wat de plugin met een ronde doet.
type Handlers struct {
	// OnDevice krijgt elk apparaat waarvan minstens één stand veranderd is
	// sinds de vorige ronde. De eerste ronde meldt ze allemaal, want dan is er
	// nog niets om mee te vergelijken.
	OnDevice func(Device)
	// OnRound draait na elke geslaagde ronde, met alles wat TaHoma nu kent. Dit
	// is waar bereikbaarheid vandaan komt: een apparaat dat niet meer in de
	// setup staat is uit de doos gehaald, en dat is hoe de bron het ook ziet.
	OnRound func([]Device)
	// OnError meldt een ronde die niet gelukt is. Een cloud die er even niet is
	// hoort geen storing te heten; dit is voor wie wil weten waarom.
	OnError func(error)
}

// Poller houdt de standen bij.
type Poller struct {
	client   *Client
	handlers Handlers

	// nudge vraagt om een extra ronde nu. Buffer één: tien apparaten die
	// tegelijk om een ronde vragen krijgen er samen één, en dat is genoeg.
	nudge chan struct{}

	mu       sync.Mutex
	interval time.Duration
	seen     map[string]string // deviceURL -> handtekening van de standen
}

// NewPoller bouwt een poller. Een interval onder MinInterval wordt opgetrokken.
func NewPoller(client *Client, interval time.Duration, handlers Handlers) *Poller {
	if interval < MinInterval {
		interval = DefaultInterval
	}
	return &Poller{
		client:   client,
		handlers: handlers,
		nudge:    make(chan struct{}, 1),
		interval: interval,
		seen:     map[string]string{},
	}
}

// Nudge vraagt om een ronde zodra het kan.
//
// Bedoeld voor vlak na een commando: de opdracht is aangenomen maar het rolluik
// staat er nog niet, en wachten op de gewone tik betekent tien seconden een
// tegel die niets doet. Dit versnelt het terugmelden; het bevestigt niets.
func (p *Poller) Nudge() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

// Interval levert de tik die nu geldt.
func (p *Poller) Interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interval
}

// Run pollt tot ctx eindigt. Blokkeert, dus hoort in een eigen goroutine.
func (p *Poller) Run(ctx context.Context) {
	// Meteen een eerste ronde: wachten op de eerste tik zou betekenen dat elke
	// tegel na een herstart tien seconden leeg blijft.
	p.round(ctx)

	timer := time.NewTimer(p.Interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-p.nudge:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		p.round(ctx)
		timer.Reset(p.Interval())
	}
}

// round haalt de setup op en meldt wat er veranderd is.
func (p *Poller) round(ctx context.Context) {
	setup, err := p.client.Setup(ctx)
	if err != nil {
		if ctx.Err() == nil && p.handlers.OnError != nil {
			p.handlers.OnError(err)
		}
		return
	}

	changed := p.diff(setup.Devices)
	if p.handlers.OnDevice != nil {
		for _, device := range changed {
			p.handlers.OnDevice(device)
		}
	}
	if p.handlers.OnRound != nil {
		p.handlers.OnRound(setup.Devices)
	}
}

// diff levert de apparaten waarvan de standen anders zijn dan de vorige ronde,
// en onthoudt de nieuwe standen.
func (p *Poller) diff(devices []Device) []Device {
	p.mu.Lock()
	defer p.mu.Unlock()

	fresh := make(map[string]string, len(devices))
	changed := make([]Device, 0, len(devices))
	for _, device := range devices {
		if device.DeviceURL == "" {
			continue
		}
		signature := device.Signature()
		fresh[device.DeviceURL] = signature
		if previous, known := p.seen[device.DeviceURL]; !known || previous != signature {
			changed = append(changed, device)
		}
	}
	// Vervangen en niet samenvoegen: een apparaat dat uit de doos verdwijnt en
	// later terugkomt hoort weer als nieuw te tellen, anders blijft zijn tegel
	// leeg tot hij toevallig een keer beweegt.
	p.seen = fresh
	return changed
}
