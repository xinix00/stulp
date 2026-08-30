// Command spotify bedient Spotify Connect: welke apparaten er zijn, wat erop
// speelt, en welk nummer of welke playlist erop moet gaan spelen.
//
// Wat deze app níet is: een speler. Spotify speelt zelf af, op een apparaat dat
// al met je account verbonden is -- een speaker, een telefoon, een computer.
// Deze app zegt alleen wat er moet gebeuren. Er gaat dus geen audio door Stulp
// heen, en er is geen stream om om te pakken.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/spotify/internal/spotify"
)

// pollInterval is hoe vaak er gekeken wordt wat er speelt.
//
// Tien seconden. Spotify hanteert een limiet per app en niet per gebruiker, dus
// vaker vragen kost meer dan het oplevert -- en wat er verandert is een titel,
// geen alarm.
const pollInterval = 10 * time.Second

// callTimeout begrenst één aanroep naar Spotify.
const callTimeout = 20 * time.Second

// cloud is de API-client. Eén voor de hele app: het token is van het account en
// niet van een apparaat.
var cloud = &spotify.Client{}

var instance = &app{devices: map[string]*player{}}

type app struct {
	mu      sync.RWMutex
	stulp   *appsdk.Stulp
	session *session
	devices map[string]*player
	order   []string
	cancel  context.CancelFunc

	// pending is een begonnen autorisatie die nog ingewisseld moet worden.
	pending *spotify.Authorization
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: instance.start,
		OnStop: instance.stop,
		Drivers: map[string]appsdk.Driver{
			"player": playerDriver{},
		},
	}
}

func (a *app) start(stulp *appsdk.Stulp) error {
	a.mu.Lock()
	a.stulp = stulp
	a.session = newSession(a.saveTokens, func(message string) { stulp.Notify(message) })
	a.mu.Unlock()

	tokens, err := readStored(stulp.State())
	if err != nil {
		// Onleesbare state is geen reden om niet te starten: de gebruiker kan
		// opnieuw koppelen, en de melding zegt dat.
		stulp.Error(err.Error())
	}
	a.readConfig()
	if tokens != nil {
		if err := a.session.setTokens(tokens); err != nil {
			stulp.Error(err.Error())
		}
	}
	cloud.Token = a.session.token
	cloud.HTTP = a.session.http

	a.registerAPI(stulp)
	a.registerFlow(stulp)
	stulp.OnSettingsChanged(func(map[string]any) { a.readConfig() })

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	go a.session.maintain(ctx)
	go a.poll(ctx)
	return nil
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// readConfig haalt de registratie uit de instellingen.
func (a *app) readConfig() {
	a.mu.RLock()
	stulp, current := a.stulp, a.session
	a.mu.RUnlock()
	if current == nil {
		return
	}
	current.setConfig(spotify.Config{
		ClientID:    stulp.SettingText("clientId"),
		RedirectURI: stulp.SettingText("redirectUri"),
	})
}

func (a *app) saveTokens(tokens *spotify.Tokens) error {
	state, err := writeStored(tokens)
	if err != nil {
		return err
	}
	a.mu.RLock()
	stulp := a.stulp
	a.mu.RUnlock()
	return stulp.SetState(state)
}

// poll haalt één keer per ronde op wat er te weten valt, voor alle apparaten
// tegelijk.
//
// Eén ronde voor allemaal en niet één per apparaat: de twee aanroepen die het
// kost -- welke apparaten er zijn, en wat er speelt -- gelden voor het hele
// account. Per apparaat vragen zou hetzelfde antwoord vijf keer ophalen.
func (a *app) poll(ctx context.Context) {
	a.sweep(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

func (a *app) sweep(ctx context.Context) {
	targets := a.targets()
	if len(targets) == 0 {
		return
	}

	call, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	devices, err := cloud.Devices(call)
	if err != nil {
		for _, target := range targets {
			target.unreachable(err)
		}
		return
	}

	// Wat er speelt hoort bij het account, niet bij een apparaat: Spotify speelt
	// op één plek tegelijk. Er kan dus niets spelen, en dat is geen fout.
	state, playing, err := cloud.Playback(call)
	if err != nil {
		a.stulp.Error("kon niet opvragen wat er speelt: " + err.Error())
	}

	for _, target := range targets {
		found, ok := find(devices, target)
		if !ok {
			target.gone()
			continue
		}
		target.apply(found, state, playing && state.Device.ID == found.ID)
	}
}

// find zoekt een gekoppeld apparaat terug in wat Spotify nu ziet.
//
// Op id én op naam, want een Connect-apparaat krijgt niet altijd hetzelfde id
// terug: een speaker die opnieuw opstart meldt zich soms onder een nieuw id met
// dezelfde naam. Alleen op id zoeken zou die tegel voorgoed grijs laten.
func find(devices []spotify.Device, target *player) (spotify.Device, bool) {
	for _, device := range devices {
		if device.ID == target.spotifyID() {
			return device, true
		}
	}
	for _, device := range devices {
		if device.Name != "" && device.Name == target.device.Name() {
			target.rename(device.ID)
			return device, true
		}
	}
	return spotify.Device{}, false
}

func (a *app) targets() []*player {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*player, 0, len(a.order))
	for _, id := range a.order {
		if target, ok := a.devices[id]; ok {
			out = append(out, target)
		}
	}
	return out
}

func (a *app) watch(deviceID string, target *player) {
	a.mu.Lock()
	if _, known := a.devices[deviceID]; !known {
		a.order = append(a.order, deviceID)
	}
	a.devices[deviceID] = target
	a.mu.Unlock()
}

func (a *app) forget(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.devices, deviceID)
	for i, id := range a.order {
		if id == deviceID {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

// refreshAll haalt buiten de ronde om alles op. Na het koppelen, zodat de
// apparaten niet een hele ronde grijs blijven.
func (a *app) refreshAll() {
	go a.sweep(context.Background())
}

// settleTime is hoe lang een opdracht mag nazinderen voordat er gevraagd wordt
// wat ervan terechtkwam.
//
// Spotify neemt een opdracht aan en voert hem daarna uit; meteen vragen levert
// het antwoord van vóór de opdracht. Eén seconde is genoeg gebleken en kost
// niets: gaat het toch langer duren, dan haalt de gewone ronde het op.
const settleTime = time.Second

// refreshSoon haalt de ronde naar voren, maar niet meteen.
func (a *app) refreshSoon() {
	go func() {
		time.Sleep(settleTime)
		a.sweep(context.Background())
	}()
}

// registerFlow hangs the two Spotify-specific playback cards on this app.
//
// Each has two arguments: what should play, and on which Spotify Connect
// player. Both choice lists search Spotify live; WiiM has its own local cards
// and never enters this process.
func (a *app) registerFlow(stulp *appsdk.Stulp) {
	stulp.OnFlowAction("play_track", func(args, state map[string]any) (any, error) {
		target, err := a.playerFor(args)
		if err != nil {
			return nil, err
		}
		uri, name := trackArgument(args["track"])
		if uri == "" {
			if name != "" {
				return nil, fmt.Errorf("%q is geen nummer maar een zoekterm; kies er een uit de lijst", name)
			}
			return nil, fmt.Errorf("kies eerst een nummer")
		}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		if err := cloud.Play(ctx, target.spotifyID(), uri); err != nil {
			return nil, err
		}
		a.refreshSoon()
		return map[string]any{"track": name}, nil
	})

	stulp.OnFlowAction("play_playlist", func(args, _ map[string]any) (any, error) {
		target, err := a.playerFor(args)
		if err != nil {
			return nil, err
		}
		uri, name := playlistArgument(args["playlist"])
		if uri == "" {
			if name != "" {
				return nil, fmt.Errorf("%q is geen playlist maar een zoekterm; kies er een uit de lijst", name)
			}
			return nil, fmt.Errorf("kies eerst een playlist")
		}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		if err := cloud.PlayContext(ctx, target.spotifyID(), uri); err != nil {
			return nil, err
		}
		a.refreshSoon()
		return map[string]any{"playlist": name}, nil
	})

	stulp.OnFlowAutocomplete("action", "play_track", "track", func(query string, args map[string]any) ([]appsdk.AutocompleteItem, error) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		tracks, err := cloud.Search(ctx, query, 0) // 0 = zoveel als Spotify toestaat
		if err != nil {
			return nil, err
		}
		items := make([]appsdk.AutocompleteItem, 0, len(tracks))
		for _, track := range tracks {
			items = append(items, appsdk.AutocompleteItem{
				ID:          track.URI,
				Name:        track.Name,
				Description: track.By(),
				Image:       track.Cover(),
			})
		}
		return items, nil
	})

	stulp.OnFlowAutocomplete("action", "play_playlist", "playlist", func(query string, args map[string]any) ([]appsdk.AutocompleteItem, error) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		playlists, err := cloud.SearchPlaylists(ctx, query, 0)
		if err != nil {
			return nil, err
		}
		items := make([]appsdk.AutocompleteItem, 0, len(playlists))
		for _, playlist := range playlists {
			items = append(items, appsdk.AutocompleteItem{
				ID:          playlist.URI,
				Name:        playlist.Name,
				Description: playlist.By(),
				Image:       playlist.Cover(),
			})
		}
		return items, nil
	})
}

// playerFor zoekt het apparaat op dat een Flow-kaart aanwijst. Een device is
// geen autocomplete-keuze met "id", maar een blijvende {"$device": id}
// verwijzing; DeviceArg leest precies dat Flow-formaat.
func (a *app) playerFor(args map[string]any) (*player, error) {
	id := appsdk.DeviceArg(args, "device")
	if id == "" {
		return nil, fmt.Errorf("kies eerst een speler")
	}
	a.mu.RLock()
	target, ok := a.devices[id]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("die speler hoort niet bij deze app")
	}
	return target, nil
}

// trackArgument haalt uit wat de kaart bewaarde de uri en de naam.
//
// Een autocomplete-argument komt terug als het hele item en niet als het id
// alleen, dus beide vormen aannemen scheelt een kaart die stilletjes niets doet
// na een wijziging in de interface.
//
// Wat er níet doorheen komt is losse tekst. Wie wel typt maar niet kiest, laat
// "blue monday" in het argument staan; dat als uri doorsturen levert een fout
// van Spotify op waar niets uit valt op te maken. Beter hier tegenhouden, met
// een zin die zegt wat er moet gebeuren.
func trackArgument(value any) (uri, name string) {
	switch typed := value.(type) {
	case string:
		return trackURI(typed), typed
	case map[string]any:
		id, _ := typed["id"].(string)
		name, _ = typed["name"].(string)
		if name == "" {
			name = id
		}
		return trackURI(id), name
	}
	return "", ""
}

// trackURI neemt aan wat Spotify als nummer herkent: een volledige uri, of het
// kale id van 22 tekens dat in zo'n uri staat.
func trackURI(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, "spotify:track:"):
		return value
	case len(value) == 22 && isBase62(value):
		return "spotify:track:" + value
	}
	return ""
}

func playlistArgument(value any) (uri, name string) {
	switch typed := value.(type) {
	case string:
		return playlistURI(typed), typed
	case map[string]any:
		id, _ := typed["id"].(string)
		name, _ = typed["name"].(string)
		if name == "" {
			name = id
		}
		return playlistURI(id), name
	}
	return "", ""
}

func playlistURI(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, "spotify:playlist:"):
		return value
	case strings.HasPrefix(value, "https://open.spotify.com/playlist/"):
		id := strings.TrimPrefix(value, "https://open.spotify.com/playlist/")
		if before, _, found := strings.Cut(id, "?"); found {
			id = before
		}
		if len(id) == 22 && isBase62(id) {
			return "spotify:playlist:" + id
		}
	case len(value) == 22 && isBase62(value):
		return "spotify:playlist:" + value
	}
	return ""
}

func isBase62(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
