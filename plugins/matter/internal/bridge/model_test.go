package bridge

import (
	"testing"

	"github.com/xinix00/stulp/internal/appsdk"
)

func TestWindowCoveringAdapterKeepsStableEndpointAndInvertsPosition(t *testing.T) {
	device := appsdk.HomeDevice{ID: "somfy-1", AppID: "com.stulp.somfy", Name: "Gordijn",
		Capabilities: []string{"windowcoverings_state", "windowcoverings_set"}}
	var saved Record
	var capability string
	var value any
	manager, err := NewManager(Record{}, []appsdk.HomeDevice{device}, func(record Record) error { saved = record; return nil },
		func(_ string, selected string, sent any) error { capability, value = selected, sent; return nil })
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.SetExported(device.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Endpoint != FirstDeviceEndpoint || first.Kind != KindWindowCovering || saved.NextEndpoint != FirstDeviceEndpoint+1 {
		t.Fatalf("first endpoint = %#v, saved=%#v", first, saved)
	}
	if _, err := manager.SetExported(device.ID, false); err != nil {
		t.Fatal(err)
	}
	second, err := manager.SetExported(device.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Endpoint != first.Endpoint {
		t.Fatalf("endpoint changed from %d to %d", first.Endpoint, second.Endpoint)
	}
	if err := manager.Invoke(first.Endpoint, "closed_fraction", 0.25); err != nil {
		t.Fatal(err)
	}
	if capability != "windowcoverings_set" || value != 0.75 {
		t.Fatalf("translated command = %s %#v, want open position 0.75", capability, value)
	}
}
