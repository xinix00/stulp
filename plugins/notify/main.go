// Command com.stulp.notify maakt van een telefoon of browser een apparaat.
//
// Waarom dit een plugin is en geen stuk kern: dan is een telefoon een device als
// elk ander. Hij komt via "Device toevoegen" binnen, staat in Manage, hoort bij
// een groep, is te hernoemen, en de Flow-kaart wijst hem aan met het gewone
// device-argument. Er is geen tweede soort doel dat overal apart behandeld moet
// worden -- dat is de hele opzet van Stulp, en meldingen zijn daarop geen
// uitzondering.
//
// Wat de plugin bezit: de VAPID-identiteit (in zijn eigen instellingen), het
// abonnement van elk toestel (in de data van dat device), en het versturen zelf.
// Wat de kern bezit: de service worker en het webmanifest, want die horen bij de
// pagina en niet bij een app, en het delen van een afbeelding, want die haalt hij
// op bij de plugin die de camera kent.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/internal/webpush"
)

// pushTTL is hoe lang een pushdienst een bericht mag vasthouden voor een toestel
// dat uit staat. Niet oneindig: een melding die een uur later binnenkomt is geen
// melding meer maar een raadsel.
const pushTTL = 10 * time.Minute

// vapidSetting is de sleutel waaronder de identiteit in de app-instellingen
// staat. In de instellingen en niet in een device: hij hoort bij de app, en een
// nieuwe sleutel maakt elk bestaand abonnement in huis in één keer ongeldig.
const vapidSetting = "vapidPrivateKey"

type app struct {
	stulp  *appsdk.Stulp
	sender webpush.Sender

	mu      sync.Mutex
	phones  map[string]*phone
	private []byte
}

var instance = &app{phones: map[string]*phone{}}

// key geeft de VAPID-identiteit en maakt hem bij het eerste gebruik aan.
//
// Bij het eerste gebruik: een huis dat nooit een melding stuurt hoeft geen
// sleutel in zijn document te hebben staan.
func (a *app) key() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.private != nil {
		return a.private, nil
	}
	if stored, ok := a.stulp.Setting(vapidSetting); ok {
		if text, _ := stored.(string); text != "" {
			decoded, err := base64.RawURLEncoding.DecodeString(text)
			if err != nil {
				return nil, fmt.Errorf("de bewaarde VAPID-sleutel is onleesbaar: %w", err)
			}
			a.private = decoded
			return decoded, nil
		}
	}
	created, err := webpush.GenerateKey()
	if err != nil {
		return nil, err
	}
	// Eerst bewaren, dan gebruiken. Een sleutel die alleen in het geheugen staat
	// levert abonnementen op die na een herstart niet meer werken.
	if err := a.stulp.SetSetting(vapidSetting, base64.RawURLEncoding.EncodeToString(created)); err != nil {
		return nil, fmt.Errorf("VAPID-sleutel bewaren: %w", err)
	}
	a.private = created
	return created, nil
}

// publicKey is wat de browser nodig heeft om zich aan te melden.
func (a *app) publicKey() (string, error) {
	private, err := a.key()
	if err != nil {
		return "", err
	}
	public, err := webpush.PublicKey(private)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(public), nil
}

func (a *app) remember(device *phone) {
	a.mu.Lock()
	a.phones[device.device.ID()] = device
	a.mu.Unlock()
}
func (a *app) forget(id string)       { a.mu.Lock(); delete(a.phones, id); a.mu.Unlock() }
func (a *app) phone(id string) *phone { a.mu.Lock(); defer a.mu.Unlock(); return a.phones[id] }

type phoneDriver struct{}

// Pair is de kant van de plugin in het koppelgesprek.
//
// De browser vraagt de publieke sleutel op en meldt zich daarmee aan bij zijn
// eigen pushdienst. Dat aanmelden kan alleen de bovenste pagina doen -- een
// koppelpagina van een app staat in een sandbox zonder eigen herkomst en mag geen
// service worker registreren -- dus tekent Manage die stap zelf, op aanwijzing van
// het pair-template in app.json.
func (phoneDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"publicKey": func(any) (any, error) {
			key, err := instance.publicKey()
			if err != nil {
				return nil, err
			}
			return map[string]any{"publicKey": key}, nil
		},
	}
}

// ListDevices hoort bij deze driver niet te bestaan: er valt niets te zoeken. Wat
// er te koppelen is, is de browser waarin je nu kijkt, en die geeft zichzelf af.
func (phoneDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return nil, errors.New("een telefoon meldt zichzelf aan; er valt niets te zoeken")
}

func (phoneDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	if _, err := subscriptionOf(device); err != nil {
		return nil, err
	}
	return &phone{device: device}, nil
}

type phone struct {
	device *appsdk.Device
}

func (p *phone) OnInit() error {
	instance.remember(p)
	// Meldingen staan aan tenzij iemand ze uitzette. Zonder deze regel staat de
	// tegel op "uit" terwijl er wel berichten aankomen.
	if _, ok := p.device.CapabilityValue("onoff"); !ok {
		return p.device.SetCapabilityValue("onoff", true)
	}
	return nil
}

func (p *phone) OnDeleted() { instance.forget(p.device.ID()) }

// OnCapability is de schakelaar op de tegel: meldingen naar dit toestel aan of
// uit, zonder het abonnement weg te gooien.
func (p *phone) OnCapability(name string, value any) error {
	return p.device.SetCapabilityValue(name, value)
}

// muted zegt of dit toestel nu niets wil ontvangen.
func (p *phone) muted() bool {
	value, ok := p.device.CapabilityValue("onoff")
	if !ok {
		return false
	}
	enabled, _ := value.(bool)
	return !enabled
}

// subscriptionOf leest het abonnement uit de data van het device.
//
// In de data en niet in een aparte lijst: dit ís de identiteit van dit apparaat,
// net zoals een serienummer dat voor een lamp is. Daarmee overleeft het een
// herstart, gaat het mee in een backup, en verdwijnt het als het device verdwijnt.
func subscriptionOf(device *appsdk.Device) (webpush.Subscription, error) {
	data := device.Data()
	endpoint, _ := data["endpoint"].(string)
	publicKey, _ := data["p256dh"].(string)
	auth, _ := data["auth"].(string)
	if endpoint == "" || publicKey == "" || auth == "" {
		return webpush.Subscription{}, errors.New("dit toestel heeft geen pushabonnement")
	}
	decodedKey, err := decodeBase64(publicKey)
	if err != nil {
		return webpush.Subscription{}, fmt.Errorf("de publieke sleutel van de browser lezen: %w", err)
	}
	decodedAuth, err := decodeBase64(auth)
	if err != nil {
		return webpush.Subscription{}, fmt.Errorf("het gedeelde geheim lezen: %w", err)
	}
	subscription := webpush.Subscription{Endpoint: endpoint, P256dh: decodedKey, Auth: decodedAuth}
	if err := subscription.Validate(); err != nil {
		return webpush.Subscription{}, err
	}
	return subscription, nil
}

// decodeBase64 accepteert beide base64-alfabetten met en zonder opvulling. Een
// browser levert base64url; code die het met btoa in elkaar zet levert de gewone
// variant. Dat zijn dezelfde bytes.
func decodeBase64(value string) ([]byte, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "=")
	value = strings.NewReplacer("+", "-", "/", "_").Replace(value)
	return base64.RawURLEncoding.DecodeString(value)
}

// send bezorgt één bericht bij één toestel.
func (a *app) send(target *phone, message webpush.Message) error {
	subscription, err := subscriptionOf(target.device)
	if err != nil {
		return err
	}
	private, err := a.key()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = a.sender.Send(ctx, private, subscription, message, pushTTL)
	switch {
	case err == nil:
		// Een geslaagde aflevering betekent dat de pushdienst het pakketje heeft
		// aangenomen, niet dat de telefoon het gezien heeft. Verder komt niemand,
		// en dat is inherent aan push: het toestel kan uit staan.
		return target.device.SetAvailable()
	case errors.Is(err, webpush.ErrGone):
		// De pushdienst kent dit abonnement niet meer: de browser is gewist of de
		// app is van het beginscherm gehaald. Het device blijft staan met de reden
		// erbij -- weggooien zou een Flow die hierop richt stil laten sneuvelen,
		// en opnieuw koppelen is één klik.
		_ = target.device.SetUnavailable("Deze browser is afgemeld. Koppel dit toestel opnieuw.")
		return err
	default:
		return err
	}
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go (zelfde naad als examples/virtual).
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: func(h *appsdk.Stulp) error {
			instance.stulp = h
			// Het contactadres dat in elk ondertekend verzoek komt. Apple en Google
			// eisen dat een afzender te bereiken is; ze controleren de vorm, niet of
			// er iemand opneemt.
			contact := "mailto:stulp@localhost"
			if value, ok := h.Setting("contact"); ok {
				if text, _ := value.(string); strings.TrimSpace(text) != "" {
					contact = strings.TrimSpace(text)
				}
			}
			instance.sender = webpush.Sender{Subject: contact}

			h.OnFlowAction("send", func(args, state map[string]any) (any, error) {
				target := instance.phone(appsdk.DeviceArg(args, "device"))
				if target == nil {
					return nil, errors.New("kies eerst een toestel")
				}
				body := strings.TrimSpace(textOf(args["message"]))
				if body == "" {
					return nil, errors.New("een melding heeft tekst nodig")
				}
				if target.muted() {
					// Niet stil overslaan: wie de schakelaar omzette weet het, maar
					// wie een Flow test hoort te zien waarom er niets aankwam.
					return nil, fmt.Errorf("meldingen staan uit voor %s", target.device.Name())
				}
				title := strings.TrimSpace(textOf(args["title"]))
				if title == "" {
					title = "Stulp"
				}
				message := webpush.Message{Title: title, Body: body, URL: "/"}
				result := map[string]any{"device": target.device.Name()}
				// De foto komt uit het afbeeldingsregister van Stulp en wordt nú
				// opgehaald: bij "iemand belt aan" wil je zien wie er stond toen het
				// gebeurde. Lukt dat niet, dan gaat het bericht alsnog -- een deurbel
				// die stil blijft omdat er geen plaatje bij kon is erger.
				if source := autocompleteID(args["image"]); source != "" {
					address, err := h.ImageURL(source, "")
					if err != nil {
						result["image"] = "zonder foto: " + err.Error()
					} else {
						message.Image = address
					}
				}
				if err := instance.send(target, message); err != nil {
					return result, err
				}
				return result, nil
			})

			// De keuzelijst van foto's is het register dat Stulp bijhoudt: elk
			// apparaat dat een stilstaand beeld aanmeldt, van welke app dan ook.
			// Deze plugin praat niet met een cameraplugin -- die weg bestaat niet --
			// hij vraagt het aan Stulp, die media toch al doorgeeft.
			h.OnFlowAutocomplete("action", "send", "image", func(query string, _ map[string]any) ([]appsdk.AutocompleteItem, error) {
				sources, err := h.ImageSources()
				if err != nil {
					return nil, err
				}
				items := make([]appsdk.AutocompleteItem, 0, len(sources))
				for _, source := range sources {
					if query != "" && !strings.Contains(strings.ToLower(source.DeviceName+" "+source.Title), strings.ToLower(query)) {
						continue
					}
					items = append(items, appsdk.AutocompleteItem{
						ID: source.DeviceID, Name: source.DeviceName, Description: source.Title,
					})
				}
				return items, nil
			})
			return nil
		},
		Drivers: map[string]appsdk.Driver{"phone": phoneDriver{}},
	}
}

// autocompleteID haalt het id uit een keuze uit een keuzelijst. Die komt als
// losse tekst terug, of als het hele item met zijn id erin, afhankelijk van hoe de
// kaart bewaard is. Een device-argument is iets anders: dat draagt $device, en
// daar is appsdk.DeviceArg voor.
func autocompleteID(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case map[string]any:
		id, _ := current["id"].(string)
		return id
	}
	return ""
}

func textOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
