package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/matter/internal/bridge"
	"github.com/xinix00/stulp/plugins/matter/internal/controller"
)

// backing laat de Matter-controller praten met Stulp.
//
// Lezen komt uit de lokale kopie die de SDK bijhoudt: dat is dezelfde stand die
// Stulp heeft, dus een opzoeking kost niets. Schrijven gaat als request, en
// alleen wat echt veranderd is: de controller geeft een heel device terug, maar
// er hoeft niet elke keer een naam en een klasse overheen.
type backing struct {
	stulp *appsdk.Stulp
	state pluginState
	mu    sync.Mutex
}

// pluginState is de eigen state van deze plugin: zijn identiteit in de fabric.
// Het staat in Stulps document zodat een backup het meeneemt, maar het is van de
// plugin en verlaat hem via geen enkele API-route.
//
// De teller voor node-id's zit in de fabric zelf en niet hiernaast. Twee tellers
// die hetzelfde bijhouden lopen uit elkaar, en een node-id dat twee keer wordt
// uitgegeven is een apparaat dat het andere overschrijft.
type pluginState struct {
	Fabric         *controller.FabricRecord            `json:"fabric,omitempty"`
	SharingWindows map[string]controller.SharingWindow `json:"sharingWindows,omitempty"`
	Bridge         *bridge.Record                      `json:"bridge,omitempty"`
}

func newBacking(stulp *appsdk.Stulp) (*backing, error) {
	b := &backing{stulp: stulp}
	if raw := stulp.State(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &b.state); err != nil {
			// Onleesbare state overschrijven zou de fabric weggooien, en dan is
			// elk apparaat opnieuw delen vanuit de andere controller. Stoppen is
			// het enige antwoord dat die keuze bij de mens laat.
			return nil, fmt.Errorf("stored Matter state is unreadable: %w", err)
		}
	}
	return b, nil
}

func (b *backing) save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveLocked()
}

func (b *backing) saveLocked() error {
	raw, err := json.Marshal(b.state)
	if err != nil {
		return err
	}
	return b.stulp.SetState(raw)
}

// ---------------------------------------------------------------------------
// Apparaten
// ---------------------------------------------------------------------------

// AddDevice bestaat niet voor een plugin. Stulp maakt apparaten aan, na het
// koppelen; een app die het zelf zou doen omzeilt de plek waar de gebruiker
// erover gaat.
func (b *backing) AddDevice(context.Context, controller.Device) (controller.Device, error) {
	return controller.Device{}, fmt.Errorf("a plugin cannot create devices; commissioning goes through pairing")
}

// DeleteDevice net zomin: verwijderen doet de gebruiker in Manage.
func (b *backing) DeleteDevice(context.Context, string) error {
	return fmt.Errorf("a plugin cannot delete devices; that is done in Stulp")
}

func (b *backing) Device(_ context.Context, id string) (controller.Device, error) {
	device, ok := b.read(id)
	if !ok {
		return controller.Device{}, fmt.Errorf("device %s is not known to this app", id)
	}
	return device, nil
}

func (b *backing) Devices(context.Context) ([]controller.Device, error) {
	ids := b.stulp.DeviceIDs()
	out := make([]controller.Device, 0, len(ids))
	for _, id := range ids {
		if device, ok := b.read(id); ok {
			out = append(out, device)
		}
	}
	return out, nil
}

// read bouwt het device op uit de lokale kopie. De controller denkt in hele
// apparaten, de SDK bewaart losse velden.
func (b *backing) read(id string) (controller.Device, bool) {
	device := b.stulp.Device(id)
	if device == nil {
		return controller.Device{}, false
	}
	// De reden betekent alleen iets zolang het apparaat onbereikbaar is. Het
	// veld blijft in Stulp staan als het apparaat weer bereikbaar wordt, en
	// meelezen zou dan een oude fout als actueel presenteren.
	message := ""
	if !device.Available() {
		message = stringField(b.stulp, id, "unavailableMessage")
	}
	return controller.Device{
		ID: id, DriverID: device.DriverID(),
		Name: device.Name(), Class: stringField(b.stulp, id, "class"),
		Data: device.Data(), Settings: device.Settings(), Store: device.Store(),
		Capabilities: device.Capabilities(), State: deviceState(device),
		Available: device.Available(), Message: message,
	}, true
}

// UpdateDevice schrijft alleen wat anders is dan wat er staat.
func (b *backing) UpdateDevice(_ context.Context, updated controller.Device) error {
	device := b.stulp.Device(updated.ID)
	if device == nil {
		return fmt.Errorf("device %s is not known to this app", updated.ID)
	}
	current, _ := b.read(updated.ID)

	for _, capability := range updated.Capabilities {
		if !device.HasCapability(capability) {
			if err := device.AddCapability(capability); err != nil {
				return err
			}
		}
	}
	for _, capability := range current.Capabilities {
		if !contains(updated.Capabilities, capability) {
			if err := device.RemoveCapability(capability); err != nil {
				return err
			}
		}
	}
	if changed := changedEntries(current.State, updated.State); len(changed) > 0 {
		// Eén RPC voor het hele rapport i.p.v. één per capability — zie
		// appsdk.SetCapabilityValues (CPU-ronde 19-08).
		if err := device.SetCapabilityValues(changed); err != nil {
			return err
		}
	}
	if changed := changedEntries(current.Settings, updated.Settings); len(changed) > 0 {
		if err := device.SetSettings(changed); err != nil {
			return err
		}
	}
	if changed := changedEntries(current.Store, updated.Store); len(changed) > 0 {
		if err := device.SetStore(changed); err != nil {
			return err
		}
	}
	// Ook een nieuwe reden bij een apparaat dat al onbereikbaar wás moet door:
	// wie de eerste reden laat staan terwijl de echte fout inmiddels bekend
	// is, laat de gebruiker naar een verouderde melding kijken.
	if current.Available != updated.Available || current.Message != updated.Message {
		if updated.Available {
			return device.SetAvailable()
		}
		return device.SetUnavailable(updated.Message)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Eigen state
// ---------------------------------------------------------------------------

func (b *backing) Fabric(context.Context) (controller.FabricRecord, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Fabric == nil {
		return controller.FabricRecord{}, false, nil
	}
	return *b.state.Fabric, true, nil
}

func (b *backing) SaveFabric(_ context.Context, record controller.FabricRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Fabric = &record
	return b.saveLocked()
}

// AllocateNodeID geeft het volgende vrije node-id uit en schuift de teller op.
//
// Zonder fabric bestaat er niets om in uit te delen. Dat is een fout en geen
// nul: een node-id 0 zou pas veel later opvallen, als apparaat.
func (b *backing) AllocateNodeID(context.Context) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Fabric == nil {
		return 0, fmt.Errorf("no Matter fabric exists yet")
	}
	allocated := b.state.Fabric.NextNodeID
	if allocated == 0 {
		return 0, fmt.Errorf("stored Matter fabric has no next node ID")
	}
	b.state.Fabric.NextNodeID = allocated + 1
	return allocated, b.saveLocked()
}

func (b *backing) saveSharingWindow(window controller.SharingWindow) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.SharingWindows == nil {
		b.state.SharingWindows = map[string]controller.SharingWindow{}
	}
	b.state.SharingWindows[window.NodeID] = window
	return b.saveLocked()
}

func (b *backing) deleteSharingWindow(nodeID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state.SharingWindows, nodeID)
	return b.saveLocked()
}

func (b *backing) sharingWindows() []controller.SharingWindow {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	result := make([]controller.SharingWindow, 0, len(b.state.SharingWindows))
	changed := false
	for nodeID, window := range b.state.SharingWindows {
		if !window.ExpiresAt.After(now) {
			delete(b.state.SharingWindows, nodeID)
			changed = true
			continue
		}
		result = append(result, window)
	}
	if changed {
		_ = b.saveLocked()
	}
	return result
}

func (b *backing) bridgeRecord() bridge.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Bridge == nil {
		return bridge.Record{}
	}
	return *b.state.Bridge
}

func (b *backing) saveBridgeRecord(record bridge.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Bridge = &record
	return b.saveLocked()
}

// ---------------------------------------------------------------------------
// Flow
// ---------------------------------------------------------------------------

func (b *backing) RecordSystemFlowEvent(_ context.Context, cardType, cardID string, tokens, state any) error {
	return b.stulp.TriggerSystemFlow(cardType, cardID, tokens, state)
}

func (b *backing) ReplaceDeviceReferences(_ context.Context, replacements map[string]controller.DeviceReplacement) error {
	out := make(map[string]appsdk.DeviceReplacement, len(replacements))
	for oldID, replacement := range replacements {
		out[oldID] = appsdk.DeviceReplacement{
			DeviceID: replacement.DeviceID, Capabilities: replacement.Capabilities,
		}
	}
	return b.stulp.ReplaceDeviceReferences(out)
}

// ---------------------------------------------------------------------------

func stringField(stulp *appsdk.Stulp, id, field string) string {
	value, _ := stulp.DeviceField(id, field)
	text, _ := value.(string)
	return text
}

func deviceState(device *appsdk.Device) map[string]any {
	out := map[string]any{}
	for _, capability := range device.Capabilities() {
		if value, ok := device.CapabilityValue(capability); ok {
			out[capability] = value
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// changedEntries levert wat er in updated anders is dan in current.
func changedEntries(current, updated map[string]any) map[string]any {
	changed := map[string]any{}
	for key, value := range updated {
		if !sameJSON(current[key], value) {
			changed[key] = value
		}
	}
	return changed
}

func sameJSON(left, right any) bool {
	a, leftErr := json.Marshal(left)
	b, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(a) == string(b)
}
