package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// fakeBacking is wat Stulp voor de tests is: een la met apparaten en de eigen
// state van deze app.
//
// Bewust geen echte store. Deze app draait in zijn eigen proces en komt niet
// bij Stulps document; een test die dat wél doet toetst een pad dat in het echt
// niet bestaat -- en houdt de afhankelijkheid in stand die het procesmodel juist
// wil uitsluiten.
//
// Wat er met een vervanging in de Flows van de gebruiker gebeurt wordt hier niet
// getoetst maar alleen opgeschreven: dat is Stulps werk, en het staat onder toets
// in internal/store. Hier gaat het erom dát de controller het meldt, en met wat.
type fakeBacking struct {
	mu       sync.Mutex
	devices  map[string]Device
	order    []string
	fabric   *FabricRecord
	next     int
	events   []flowEvent
	replaced []map[string]DeviceReplacement
}

type flowEvent struct {
	cardType, cardID string
	tokens, state    any
}

func newBacking() *fakeBacking {
	return &fakeBacking{devices: map[string]Device{}}
}

func (b *fakeBacking) AddDevice(_ context.Context, device Device) (Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if device.ID == "" {
		b.next++
		device.ID = fmt.Sprintf("device-%d", b.next)
	}
	if _, exists := b.devices[device.ID]; !exists {
		b.order = append(b.order, device.ID)
	}
	b.devices[device.ID] = device
	return device, nil
}

func (b *fakeBacking) Device(_ context.Context, id string) (Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	device, ok := b.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device %s is not known to this app", id)
	}
	return device, nil
}

// Devices levert ze in de volgorde waarin ze zijn toegevoegd. Een map-volgorde
// is willekeurig, en dan zou een test die over endpoints samenvoegt de ene keer
// slagen en de andere keer niet.
func (b *fakeBacking) Devices(context.Context) ([]Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Device, 0, len(b.order))
	for _, id := range b.order {
		if device, ok := b.devices[id]; ok {
			out = append(out, device)
		}
	}
	return out, nil
}

func (b *fakeBacking) UpdateDevice(_ context.Context, device Device) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.devices[device.ID]; !ok {
		return fmt.Errorf("device %s is not known to this app", device.ID)
	}
	b.devices[device.ID] = device
	return nil
}

func (b *fakeBacking) DeleteDevice(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.devices[id]; !ok {
		return fmt.Errorf("device %s is not known to this app", id)
	}
	delete(b.devices, id)
	for index, current := range b.order {
		if current == id {
			b.order = append(b.order[:index], b.order[index+1:]...)
			break
		}
	}
	return nil
}

func (b *fakeBacking) Fabric(context.Context) (FabricRecord, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fabric == nil {
		return FabricRecord{}, false, nil
	}
	return *b.fabric, true, nil
}

func (b *fakeBacking) SaveFabric(_ context.Context, record FabricRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fabric = &record
	return nil
}

func (b *fakeBacking) AllocateNodeID(context.Context) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fabric == nil {
		return 0, errors.New("no Matter fabric exists yet")
	}
	allocated := b.fabric.NextNodeID
	if allocated == 0 {
		return 0, errors.New("stored Matter fabric has no next node ID")
	}
	b.fabric.NextNodeID = allocated + 1
	return allocated, nil
}

func (b *fakeBacking) RecordSystemFlowEvent(_ context.Context, cardType, cardID string, tokens, state any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, flowEvent{cardType: cardType, cardID: cardID, tokens: tokens, state: state})
	return nil
}

func (b *fakeBacking) ReplaceDeviceReferences(_ context.Context, replacements map[string]DeviceReplacement) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replaced = append(b.replaced, replacements)
	return nil
}

// replacements levert alles wat er gemeld is, samengevoegd.
func (b *fakeBacking) replacements() map[string]DeviceReplacement {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]DeviceReplacement{}
	for _, batch := range b.replaced {
		for oldID, replacement := range batch {
			out[oldID] = replacement
		}
	}
	return out
}

func (b *fakeBacking) flowEvents() []flowEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]flowEvent(nil), b.events...)
}
