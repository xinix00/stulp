package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/spotify/internal/spotify"
)

// Eén Spotify Connect-apparaat als tegel.
//
// De tegel van een speler die niets afspeelt is leeg en dat klopt: Spotify
// speelt op één plek tegelijk, en wat er speelt hoort bij het account. Een
// speler die Spotify nu niet ziet -- uitgezet, app dicht -- gaat op
// onbereikbaar in plaats van te blijven staan met wat hij een half uur geleden
// deed.

type playerDriver struct{}

type player struct {
	device *appsdk.Device

	// mu bewaakt id: dat kan veranderen. Een Connect-apparaat dat opnieuw
	// opstart meldt zich soms onder een nieuw id met dezelfde naam.
	mu sync.Mutex
	id string
}

func (playerDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen Spotify-id; koppel de speler opnieuw")
	}
	// Het id waarmee gekoppeld is ligt vast in Data; het id waarmee Spotify dit
	// apparaat nú kent staat in Store, want dat kan veranderen.
	if current, ok := device.StoreValue("spotifyId"); ok {
		if text, _ := current.(string); text != "" {
			id = text
		}
	}
	return &player{device: device, id: id}, nil
}

// ListDevices vraagt Spotify welke Connect-apparaten dit account nu ziet.
//
// "Nu" is letterlijk: een telefoon staat er alleen in zolang de Spotify-app
// open is. Wat hier niet staat, staat er over een minuut misschien wel.
func (playerDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	devices, err := cloud.Devices(ctx)
	if err != nil {
		return nil, err
	}
	found := []appsdk.PairedDevice{}
	for _, device := range devices {
		if device.ID == "" || device.IsRestricted {
			// Een apparaat zonder id of met restricties neemt geen opdrachten
			// van buiten aan. Aanbieden zou een tegel opleveren die altijd
			// faalt.
			continue
		}
		found = append(found, appsdk.PairedDevice{
			Name:  device.Name,
			Data:  map[string]any{"id": device.ID},
			Store: map[string]any{"type": device.Type},
		})
	}
	return found, nil
}

func (p *player) OnInit() error {
	instance.watch(p.device.ID(), p)
	instance.refreshAll()
	return nil
}

func (p *player) OnDeleted() { instance.forget(p.device.ID()) }

func (p *player) spotifyID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.id
}

// rename neemt een nieuw Spotify-id over en legt het vast, zodat het een
// herstart overleeft.
func (p *player) rename(id string) {
	p.mu.Lock()
	changed := p.id != id
	p.id = id
	p.mu.Unlock()
	if !changed {
		return
	}
	if err := p.device.SetStore(map[string]any{"spotifyId": id}); err != nil {
		p.device.Error("kon het nieuwe Spotify-id niet vastleggen: " + err.Error())
	}
}

// apply zet neer wat er van deze speler bekend is. playing zegt of wat er
// speelt op dít apparaat speelt.
func (p *player) apply(device spotify.Device, state spotify.Playback, playing bool) {
	if err := p.device.SetAvailable(); err != nil {
		p.device.Error(err.Error())
	}
	values := map[string]any{"speaker_playing": playing && state.IsPlaying}
	if device.VolumePercent != nil {
		values["volume_set"] = float64(*device.VolumePercent) / 100
	}

	if !playing || state.Item == nil {
		// Niets op dít apparaat. De titelvelden leegmaken, want anders blijft er
		// een nummer staan dat allang ergens anders speelt.
		values["speaker_track"] = ""
		values["speaker_artist"] = ""
		values["speaker_album"] = ""
	} else {
		values["speaker_track"] = state.Item.Name
		values["speaker_artist"] = state.Item.By()
		values["speaker_album"] = state.Item.Album.Name
	}
	if err := p.device.SetCapabilityValues(values); err != nil {
		p.device.Error(err.Error())
	}
}

// gone: Spotify ziet dit apparaat niet meer.
func (p *player) gone() {
	p.device.SetUnavailable("Spotify ziet deze speler nu niet. Zet hem aan of open de app erop.")
	if err := p.device.SetCapabilityValue("speaker_playing", false); err != nil {
		p.device.Error(err.Error())
	}
}

// unreachable: de aanroep zelf mislukte, dus we weten niets.
func (p *player) unreachable(err error) {
	p.device.SetUnavailable("Spotify antwoordt niet: " + err.Error())
}

func (p *player) OnCapability(name string, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	id := p.spotifyID()

	var err error
	switch name {
	case "speaker_playing":
		on, ok := value.(bool)
		if !ok {
			return fmt.Errorf("speaker_playing verwacht aan of uit, kreeg %v", value)
		}
		if on {
			// Hervatten en niet iets nieuws kiezen. Speelt er niets, dan zegt
			// Spotify dat -- en dat is eerlijker dan hier een nummer verzinnen.
			err = cloud.Resume(ctx, id)
		} else {
			err = cloud.Pause(ctx, id)
		}
	case "speaker_next":
		err = cloud.Next(ctx, id)
	case "speaker_prev":
		err = cloud.Previous(ctx, id)
	case "volume_set":
		level, ok := asNumber(value)
		if !ok {
			return fmt.Errorf("volume_set verwacht een getal tussen 0 en 1, kreeg %v", value)
		}
		err = cloud.SetVolume(ctx, id, int(math.Round(level*100)))
	default:
		return fmt.Errorf("deze speler kent %q niet", name)
	}
	if err != nil {
		return err
	}

	// Hier wordt de gevraagde waarde met opzet níet neergezet. Spotify heeft de
	// opdracht aangenomen, niet uitgevoerd: /me/player meldt seconden later pas
	// de nieuwe stand. Hem hier alvast omzetten gaf precies wat je zag -- de
	// knop sprong om, de eerstvolgende ronde zette hem terug, en daarna sprong
	// hij nog eens. Wat de speler doet komt van de speler.
	//
	// Wel de ronde naar voren halen, met een tel bedenktijd: meteen vragen
	// levert het oude antwoord op en dus dezelfde sprong.
	instance.refreshSoon()
	return nil
}

// asNumber neemt wat er over het protocol binnenkomt. JSON levert een float64,
// een Flow-kaart of een test soms een int.
func asNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	}
	return 0, false
}

var (
	_ appsdk.Driver            = playerDriver{}
	_ appsdk.Pairer            = playerDriver{}
	_ appsdk.DeviceHandler     = (*player)(nil)
	_ appsdk.CapabilityHandler = (*player)(nil)
	_ appsdk.Deleter           = (*player)(nil)
	_                          = errors.Is
)
