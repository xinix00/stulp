package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// De schijnwerper: aan/uit, helderheid, en zijn eigen bewegingsmelder.
type lightDriver struct{}

type lightDevice struct {
	device  *appsdk.Device
	protect string
}

func (lightDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen UniFi-id")
	}
	return &lightDevice{device: device, protect: id}, nil
}

func (lightDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	lights, err := client.Lights(context.Background())
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(lights))
	for _, light := range lights {
		found = append(found, appsdk.PairedDevice{
			Name:  light.Name,
			Data:  map[string]any{"id": light.ID},
			Store: map[string]any{"model": light.Model, "host": light.Host},
		})
	}
	return found, nil
}

func (l *lightDevice) OnInit() error {
	instance.watch(l.protect, l)
	l.refresh()
	return nil
}

func (l *lightDevice) OnDeleted() {
	instance.forget(l.protect)
}

func (l *lightDevice) refresh() {
	client, err := instance.api()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	light, err := client.Light(ctx, l.protect)
	if err != nil {
		l.device.SetUnavailable("De console antwoordt niet: " + err.Error())
		return
	}
	if light.IsConnected {
		l.device.SetAvailable()
	} else {
		l.device.SetUnavailable("De schijnwerper is niet verbonden met de console.")
	}
	report(l.device, "onoff", light.IsLightOn)
	report(l.device, "dim", protect.LightBrightness(light.LightDeviceSettings.LedLevel))
	report(l.device, "alarm_motion", light.IsPirMotionDetected)
}

// apply verwerkt een deelbericht van de websocket.
//
// Elk veld is een pointer, en dat is de kern: de console stuurt alleen wat er
// veranderd is. Zonder pointer valt een ontbrekend veld niet te onderscheiden
// van false of nul, en dan zou elk bericht over de helderheid de schijnwerper
// ook "uit" en "geen beweging" melden.
func (l *lightDevice) apply(message protect.DeviceMessage) {
	var patch struct {
		IsConnected         *bool `json:"isConnected"`
		IsLightOn           *bool `json:"isLightOn"`
		IsPirMotionDetected *bool `json:"isPirMotionDetected"`
		LightDeviceSettings *struct {
			LedLevel *int `json:"ledLevel"`
		} `json:"lightDeviceSettings"`
	}
	if json.Unmarshal(message.Item, &patch) != nil {
		return
	}
	if patch.IsConnected != nil {
		if *patch.IsConnected {
			l.device.SetAvailable()
		} else {
			l.device.SetUnavailable("De schijnwerper is niet verbonden met de console.")
		}
	}
	if patch.IsLightOn != nil {
		report(l.device, "onoff", *patch.IsLightOn)
	}
	if patch.IsPirMotionDetected != nil {
		report(l.device, "alarm_motion", *patch.IsPirMotionDetected)
	}
	if patch.LightDeviceSettings != nil && patch.LightDeviceSettings.LedLevel != nil {
		report(l.device, "dim", protect.LightBrightness(*patch.LightDeviceSettings.LedLevel))
	}
}

func (l *lightDevice) OnCapability(name string, value any) error {
	client, err := instance.api()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch name {
	case "onoff":
		on, _ := value.(bool)
		if err := client.SetLightOn(ctx, l.protect, on); err != nil {
			return err
		}
	case "dim":
		level, _ := value.(float64)
		if err := client.SetLightLevel(ctx, l.protect, level); err != nil {
			return err
		}
	default:
		return fmt.Errorf("deze schijnwerper kent %q niet", name)
	}
	// De gevraagde waarde wordt hier niet neergezet: de console meldt over de
	// websocket wat er werkelijk gebeurde, en dat is binnen een tel binnen.
	// Vooruitlopen zou de tegel laten springen zodra het echte bericht komt.
	return nil
}
