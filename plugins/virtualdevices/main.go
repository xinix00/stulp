// Command com.stulp.virtualdevices maakt schakelaars zonder hardware.
//
// Zo'n schakelaar is gewone Stulp-state: hij staat tussen de apparaten, kan in
// een groep, en de standaard onoff-capability levert vanzelf de ALS-, EN- en
// DAN-kaarten voor Flows en dezelfde schrijfactie voor Scenes. De enige
// bijzondere eigenschap is dat de stand ook in de device-store staat. Live
// capabilitywaarden zijn normaal gesproken vluchtige waarnemingen; voor een
// virtuele schakelaar IS de laatst gekozen waarde juist de waarheid die een
// herstart moet overleven.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/xinix00/stulp/internal/appsdk"
)

const (
	virtualStateKey      = "onoff"
	maxVirtualNameLength = 160
)

// switchDevice is het kleine deel van appsdk.Device dat de schakelaar nodig
// heeft. De interface houdt de persistentieregel rechtstreeks testbaar, zonder
// een tweede uitvoering naast de echte handler te bouwen.
type switchDevice interface {
	StoreValue(string) (any, bool)
	SetStore(map[string]any) error
	SetCapabilityValue(string, any) error
	SetAvailable() error
}

type switchDriver struct {
	// newID is alleen een testnaad. Een lege functie kiest de cryptografisch
	// willekeurige productie-id hieronder.
	newID func() (string, error)
}

func (switchDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	return &virtualSwitch{device: device}, nil
}

// Pair maakt per open koppelvenster één onafhankelijke kandidaat. Daardoor kan
// iemand zoveel schakelaars met dezelfde of verschillende namen toevoegen als
// nodig is, terwijl een retry van de uiteindelijke add_devices-stap dezelfde
// identiteit houdt en dus idempotent blijft.
func (d switchDriver) Pair() map[string]appsdk.PairHandler {
	var candidate *appsdk.PairedDevice
	return map[string]appsdk.PairHandler{
		"create": func(data any) (any, error) {
			request, _ := data.(map[string]any)
			name, _ := request["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, errors.New("geef de virtuele schakelaar een naam")
			}
			if utf8.RuneCountInString(name) > maxVirtualNameLength {
				return nil, fmt.Errorf("de naam mag hoogstens %d tekens lang zijn", maxVirtualNameLength)
			}
			id, err := d.virtualID()
			if err != nil {
				return nil, fmt.Errorf("identiteit voor de virtuele schakelaar maken: %w", err)
			}
			created := appsdk.PairedDevice{
				Name: name,
				Data: map[string]any{"id": id},
				// De eerste stand ligt al vast voordat Stulp het device start. Zo
				// is zelfs een herstart tussen koppelen en OnInit eenduidig: uit.
				Store: map[string]any{virtualStateKey: false},
			}
			candidate = &created
			return created, nil
		},
		"list_devices": func(any) (any, error) {
			if candidate == nil {
				return nil, errors.New("geef de virtuele schakelaar eerst een naam")
			}
			return []appsdk.PairedDevice{*candidate}, nil
		},
	}
}

func (d switchDriver) virtualID() (string, error) {
	if d.newID != nil {
		return d.newID()
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "virtual-" + hex.EncodeToString(value[:]), nil
}

type virtualSwitch struct{ device switchDevice }

func (s *virtualSwitch) OnInit() error {
	stored, found := s.device.StoreValue(virtualStateKey)
	on, valid := stored.(bool)
	if !found || !valid {
		on = false
		// Ook een apparaat uit een vroege versie of een handmatig herstelde
		// backup krijgt vanaf nu dezelfde duurzame waarheid.
		if err := s.device.SetStore(map[string]any{virtualStateKey: on}); err != nil {
			return fmt.Errorf("beginstand bewaren: %w", err)
		}
	}
	if err := s.device.SetCapabilityValue("onoff", on); err != nil {
		return fmt.Errorf("bewaarde stand herstellen: %w", err)
	}
	// Er zit geen verbinding achter die weg kan vallen; zodra de handler draait
	// is het apparaat per definitie bereikbaar.
	return s.device.SetAvailable()
}

func (s *virtualSwitch) OnCapability(name string, value any) error {
	if name != "onoff" {
		return fmt.Errorf("virtuele schakelaar kent capability %q niet", name)
	}
	on, ok := value.(bool)
	if !ok {
		return errors.New("de aan/uit-stand moet true of false zijn")
	}
	// Eerst de duurzame bron bijwerken, daarna de vluchtige projectie. Na een
	// bevestigde schrijfopdracht kan een herstart daardoor nooit terugvallen op
	// de vorige stand.
	if err := s.device.SetStore(map[string]any{virtualStateKey: on}); err != nil {
		return fmt.Errorf("stand bewaren: %w", err)
	}
	if err := s.device.SetCapabilityValue("onoff", on); err != nil {
		return fmt.Errorf("stand melden: %w", err)
	}
	return nil
}

func plugin() appsdk.Plugin {
	return appsdk.Plugin{Drivers: map[string]appsdk.Driver{"switch": switchDriver{}}}
}

func main() { start(plugin()) }
