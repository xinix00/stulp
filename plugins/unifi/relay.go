package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// Eén uitgang van een relaismodule.
//
// Eén apparaat per uitgang en niet per module: een module met twee uitgangen
// zit meestal aan twee verschillende dingen vast, en die wil je apart kunnen
// schakelen en apart een naam geven.
type relayDriver struct{}

type relayDevice struct {
	device  *appsdk.Device
	protect string // het relais
	output  string // de uitgang erop
}

func (relayDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	data := device.Data()
	relayID, _ := data["id"].(string)
	outputID, _ := data["output"].(string)
	if relayID == "" || outputID == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen relais- en uitgang-id")
	}
	return &relayDevice{device: device, protect: relayID, output: outputID}, nil
}

func (relayDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	relays, err := client.Relays(context.Background())
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0)
	for _, relay := range relays {
		for _, output := range relay.Outputs {
			name := output.Name
			if name == "" {
				name = relay.Name + " " + output.ID
			}
			found = append(found, appsdk.PairedDevice{
				Name:  name,
				Data:  map[string]any{"id": relay.ID, "output": output.ID},
				Store: map[string]any{"model": relay.Model, "relay": relay.Name},
			})
		}
	}
	return found, nil
}

func (r *relayDevice) OnInit() error {
	instance.watch(r.protect, r)
	r.refresh()
	return nil
}

func (r *relayDevice) OnDeleted() {
	instance.forget(r.protect)
}

func (r *relayDevice) refresh() {
	client, err := instance.api()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	relay, err := client.Relay(ctx, r.protect)
	if err != nil {
		r.device.SetUnavailable("De console antwoordt niet: " + err.Error())
		return
	}
	if relay.IsConnected {
		r.device.SetAvailable()
	} else {
		r.device.SetUnavailable("Het relais is niet verbonden met de console.")
	}
	for _, output := range relay.Outputs {
		if output.ID == r.output {
			report(r.device, map[string]any{"onoff": output.On()})
			return
		}
	}
	r.device.SetUnavailable("Deze uitgang bestaat niet meer op het relais.")
}

// apply verwerkt een deelbericht van de websocket. IsConnected is een pointer
// omdat een ontbrekend veld niet hetzelfde is als "niet verbonden"; de uitgangen
// komen als hele lijst mee of helemaal niet.
func (r *relayDevice) apply(message protect.DeviceMessage) {
	var patch struct {
		IsConnected *bool `json:"isConnected"`
		Outputs     []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"outputs"`
	}
	if json.Unmarshal(message.Item, &patch) != nil {
		return
	}
	if patch.IsConnected != nil {
		if *patch.IsConnected {
			r.device.SetAvailable()
		} else {
			r.device.SetUnavailable("Het relais is niet verbonden met de console.")
		}
	}
	for _, output := range patch.Outputs {
		if output.ID == r.output {
			report(r.device, map[string]any{"onoff": output.State == "on"})
		}
	}
}

func (r *relayDevice) OnCapability(name string, value any) error {
	if name != "onoff" {
		return fmt.Errorf("deze uitgang kent %q niet", name)
	}
	client, err := instance.api()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Als puls aan staat is de tegel een knop en geen schakelaar: hij springt
	// vanzelf terug, want de deur staat niet "aan".
	if pulse, _ := r.device.Setting("pulse"); pulse == true {
		if err := client.PulseRelayOutput(ctx, r.protect, r.output, r.pulseLength()); err != nil {
			return err
		}
		// De console meldt over de websocket dat de uitgang weer uit staat.
		return nil
	}
	on, _ := value.(bool)
	if err := client.SetRelayOutput(ctx, r.protect, r.output, on); err != nil {
		return err
	}
	// Niet vooruitlopen: de console meldt over de websocket wat de uitgang
	// werkelijk doet, en dat is binnen een tel binnen.
	return nil
}

func (r *relayDevice) pulseLength() int {
	if value, ok := r.device.Setting("pulse_ms"); ok {
		if milliseconds, ok := value.(float64); ok && milliseconds > 0 {
			return int(milliseconds)
		}
	}
	return 1000
}

// pulse is wat de Flow-kaart doet.
func (r *relayDevice) pulse(milliseconds int) error {
	client, err := instance.api()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return client.PulseRelayOutput(ctx, r.protect, r.output, milliseconds)
}
