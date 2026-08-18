package appsdk

import (
	"fmt"
	"sync"
)

// Device is de greep die Stulp aan een plugin geeft voor één gekoppeld apparaat.
//
// Lezen komt uit de lokale kopie en kost niets; schrijven is een request naar
// Stulp en geeft dus een fout terug. Alle methoden mogen vanaf elke goroutine.
type Device struct {
	id       string
	driverID string
	host     *Host
	stulp    *Stulp
	handler  DeviceHandler

	mu    sync.Mutex
	media map[string]mediaSlot
}

func (d *Device) ID() string       { return d.id }
func (d *Device) DriverID() string { return d.driverID }

// Stulp geeft toegang tot wat niet bij dit ene apparaat hoort: instellingen van
// de app, Flow-kaarten, notificaties.
func (d *Device) Stulp() *Stulp { return d.stulp }

func (d *Device) Name() string {
	value, _ := d.host.state.DeviceField(d.id, "name")
	name, _ := value.(string)
	return name
}

// Data is de identiteit die bij het koppelen is vastgelegd en daarna niet meer
// verandert. Hier hoort te staan hoe je het apparaat terugvindt, niet waar het
// nu is -- een adres hoort in de settings of de store.
func (d *Device) Data() map[string]any {
	data, _ := d.host.state.DeviceMap(d.id, "data")
	return data
}

func (d *Device) Settings() map[string]any {
	settings, _ := d.host.state.DeviceMap(d.id, "settings")
	return settings
}

func (d *Device) Setting(key string) (any, bool) {
	settings, ok := d.host.state.DeviceMap(d.id, "settings")
	if !ok {
		return nil, false
	}
	value, ok := settings[key]
	return value, ok
}

// SetSettings schrijft de instellingen die de gebruiker ook zelf kan aanpassen.
// Alleen de sleutels in patch veranderen.
func (d *Device) SetSettings(patch map[string]any) error {
	return d.host.MergeDeviceMap(d.id, "settings", patch)
}

// Store is de plek voor wat de app zelf bijhoudt en de gebruiker niet ziet: een
// token, een sessie-id, een laatst geziene waarde.
func (d *Device) Store() map[string]any {
	store, _ := d.host.state.DeviceMap(d.id, "store")
	return store
}

// StoreNumber levert een bewaard getal, of nul als het er niet staat. Gebruik
// HasStore om die twee uit elkaar te houden: nul is bij een meterstand een
// geldige waarde, en "nog nooit geijkt" is iets anders dan "stond op nul".
func (d *Device) StoreNumber(key string) float64 {
	value, _ := d.StoreValue(key)
	number, _ := value.(float64)
	return number
}

func (d *Device) HasStore(key string) bool {
	_, ok := d.StoreValue(key)
	return ok
}

func (d *Device) StoreValue(key string) (any, bool) {
	store, ok := d.host.state.DeviceMap(d.id, "store")
	if !ok {
		return nil, false
	}
	value, ok := store[key]
	return value, ok
}

func (d *Device) SetStore(patch map[string]any) error {
	return d.host.MergeDeviceMap(d.id, "store", patch)
}

func (d *Device) Capabilities() []string {
	value, _ := d.host.state.DeviceField(d.id, "capabilities")
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func (d *Device) HasCapability(name string) bool {
	for _, have := range d.Capabilities() {
		if have == name {
			return true
		}
	}
	return false
}

// CapabilityValue leest de laatst gemelde waarde.
func (d *Device) CapabilityValue(name string) (any, bool) {
	state, ok := d.host.state.DeviceMap(d.id, "state")
	if !ok {
		return nil, false
	}
	value, ok := state[name]
	return value, ok
}

// SetCapabilityValue meldt een nieuwe waarde. Dit is wat een sensor doet zodra
// hij iets weet, en wat een schakelaar doet nadat hij bevestigd heeft.
//
// Een capability die dit apparaat niet heeft is een fout en geen stille no-op:
// een tikfout in een capability-naam hoort meteen op te vallen en niet pas als
// iemand zich afvraagt waarom de tegel leeg blijft.
func (d *Device) SetCapabilityValue(name string, value any) error {
	if !d.HasCapability(name) {
		return fmt.Errorf("device %s has no capability %q", d.id, name)
	}
	return d.host.MergeDeviceMap(d.id, "state", map[string]any{name: value})
}

func (d *Device) AddCapability(name string) error {
	return d.host.AddCapability(d.id, name)
}

func (d *Device) RemoveCapability(name string) error {
	return d.host.RemoveCapability(d.id, name)
}

// Available meldt of Stulp dit apparaat als bereikbaar toont.
func (d *Device) Available() bool {
	value, _ := d.host.state.DeviceField(d.id, "available")
	available, _ := value.(bool)
	return available
}

func (d *Device) SetAvailable() error {
	return d.host.SetDeviceField(d.id, "available", true)
}

// SetUnavailable zet het apparaat op onbereikbaar, met een reden die de
// gebruiker te zien krijgt. Een lege reden is toegestaan maar zelden nuttig.
func (d *Device) SetUnavailable(reason string) error {
	if err := d.host.SetDeviceField(d.id, "unavailableMessage", reason); err != nil {
		return err
	}
	return d.host.SetDeviceField(d.id, "available", false)
}

func (d *Device) Log(message string)   { d.host.Log("info", d.prefix(message)) }
func (d *Device) Error(message string) { d.host.Log("error", d.prefix(message)) }

func (d *Device) prefix(message string) string { return d.Name() + ": " + message }
