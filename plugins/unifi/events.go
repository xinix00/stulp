package main

import (
	"fmt"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// Wat er gebeurt, tegenover wat iets is.
//
// subscribe/events meldt het eerste: er wordt aangebeld, er is beweging, er is
// iets herkend. Die berichten dragen een device-id, geen model, dus ze worden
// hier tegen de gekoppelde camera's en deurbellen gelegd.

func (a *app) event(message protect.EventMessage) {
	a.mu.RLock()
	target, _ := a.devices[message.Item.Device].(*cameraDevice)
	a.mu.RUnlock()
	if target == nil {
		return
	}
	switch message.Item.Type {
	case "ring":
		// Alleen bij add: een update op dezelfde bel is dezelfde bel, en twee
		// keer een Flow starten voor één druk op de knop is één te veel.
		if message.Type == "add" {
			a.stulp.TriggerDeviceFlow("doorbell_pressed", target.device, nil, nil)
		}
	case "motion":
		switch {
		case message.Type == "add":
			target.motion(true)
		case message.Item.End != 0:
			target.motion(false)
		}
	case "smartDetectZone":
		for _, kind := range message.Item.SmartDetectTypes {
			a.stulp.TriggerDeviceFlow("smart_detection", target.device,
				map[string]any{"kind": kind}, map[string]any{"kind": kind})
		}
	case "smartAudioDetect":
		// Geluid is een eigen soort herkenning en geen bijzonder geval van de
		// vorige: een camera die een rookmelder hoort ziet niets, en de
		// woordenschat is een andere. Vandaar een eigen kaart.
		for _, kind := range message.Item.SmartDetectTypes {
			readable := audioKind(kind)
			a.stulp.TriggerDeviceFlow("audio_detection", target.device,
				map[string]any{"kind": readable},
				map[string]any{"kind": readable, "raw": kind})
		}
	}
}

// audioKind maakt van wat de console stuurt het woord dat op de kaart staat.
//
// De console gebruikt afkortingen als alrmCmonx; die op een keuzelijst zetten
// zou de gebruiker laten raden. Wat hier niet in staat gaat ongewijzigd door --
// een nieuw soort geluid hoort een Flow te kunnen starten voordat deze tabel
// bijgewerkt is, in plaats van stil te verdwijnen.
func audioKind(apiType string) string {
	switch apiType {
	case "alrmSmoke":
		return "smoke"
	case "alrmCmonx":
		return "co"
	case "alrmSiren":
		return "siren"
	case "alrmBabyCry":
		return "baby_cry"
	case "alrmSpeak":
		return "speaking"
	case "alrmBark":
		return "bark"
	case "alrmBurglar":
		return "burglar_alarm"
	case "alrmCarHorn":
		return "car_horn"
	case "alrmGlassBreak":
		return "glass_break"
	}
	return apiType
}

// registerFlow hangt de kaarten op die deze app aanbiedt.
func (a *app) registerFlow(stulp *appsdk.Stulp) {
	// De herkenningskaart draait alleen als het gekozen soort past. "any" past
	// altijd -- dat is waar iemand voor kiest die alles wil weten.
	stulp.OnFlowTrigger("smart_detection", func(args, state map[string]any) (bool, error) {
		wanted, _ := args["kind"].(string)
		if wanted == "" || wanted == "any" {
			return true, nil
		}
		got, _ := state["kind"].(string)
		return got == wanted, nil
	})

	// Dezelfde regel voor geluid: "any" past altijd.
	stulp.OnFlowTrigger("audio_detection", func(args, state map[string]any) (bool, error) {
		wanted, _ := args["kind"].(string)
		if wanted == "" || wanted == "any" {
			return true, nil
		}
		got, _ := state["kind"].(string)
		return got == wanted, nil
	})

	stulp.OnFlowAction("pulse_relay", func(args, _ map[string]any) (any, error) {
		relay, ok := a.deviceFor(appsdk.DeviceArg(args, "device")).(*relayDevice)
		if !ok {
			return nil, fmt.Errorf("dit is geen relaisuitgang van deze app")
		}
		milliseconds := 1000
		if value, ok := args["milliseconds"].(float64); ok && value > 0 {
			milliseconds = int(value)
		}
		return nil, relay.pulse(milliseconds)
	})
}

// deviceFor zoekt het gekoppelde apparaat op zijn Stulp-id.
func (a *app) deviceFor(stulpID string) handler {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, target := range a.devices {
		switch typed := target.(type) {
		case *cameraDevice:
			if typed.device.ID() == stulpID {
				return typed
			}
		case *relayDevice:
			if typed.device.ID() == stulpID {
				return typed
			}
		}
	}
	return nil
}
