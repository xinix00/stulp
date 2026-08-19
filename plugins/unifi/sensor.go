package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// De UP-Sense: contact, beweging, temperatuur, vocht, licht en een batterij.
type sensorDriver struct{}

type sensorDevice struct {
	device  *appsdk.Device
	protect string
}

func (sensorDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen UniFi-id")
	}
	return &sensorDevice{device: device, protect: id}, nil
}

func (sensorDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	sensors, err := client.Sensors(context.Background())
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(sensors))
	for _, sensor := range sensors {
		found = append(found, appsdk.PairedDevice{
			Name:  sensor.Name,
			Data:  map[string]any{"id": sensor.ID},
			Store: map[string]any{"model": sensor.Model},
		})
	}
	return found, nil
}

func (s *sensorDevice) OnInit() error {
	instance.watch(s.protect, s)
	s.refresh()
	return nil
}

func (s *sensorDevice) OnDeleted() {
	instance.forget(s.protect)
}

func (s *sensorDevice) refresh() {
	client, err := instance.api()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sensor, err := client.Sensor(ctx, s.protect)
	if err != nil {
		s.device.SetUnavailable("De console antwoordt niet: " + err.Error())
		return
	}
	if sensor.IsConnected {
		s.device.SetAvailable()
	} else {
		s.device.SetUnavailable("De sensor is niet verbonden met de console.")
	}
	values := map[string]any{
		"alarm_contact":   sensor.IsOpened,
		"alarm_motion":    sensor.IsMotionDetected,
		"alarm_tamper":    sensor.TamperingDetectedAt != nil,
		"measure_battery": float64(sensor.BatteryStatus.Percentage),
		"alarm_battery":   sensor.BatteryStatus.IsLow,
	}
	// Een meting die de sensor niet doet blijft leeg. Er staat null in het
	// antwoord, en nul graden is een echte temperatuur.
	if value := sensor.Stats.Temperature.Value; value != nil {
		values["measure_temperature"] = *value
	}
	if value := sensor.Stats.Humidity.Value; value != nil {
		values["measure_humidity"] = *value
	}
	if value := sensor.Stats.Light.Value; value != nil {
		values["measure_luminance"] = *value
	}
	report(s.device, values)
}

// apply verwerkt een deelbericht van de websocket. Elk veld is een pointer
// omdat de console alleen stuurt wat er veranderd is: een ontbrekend veld is
// iets anders dan false of nul, en die twee door elkaar halen laat een sensor
// bij elk bericht "dicht" en "geen beweging" melden.
func (s *sensorDevice) apply(message protect.DeviceMessage) {
	var patch struct {
		IsConnected         *bool  `json:"isConnected"`
		IsOpened            *bool  `json:"isOpened"`
		IsMotionDetected    *bool  `json:"isMotionDetected"`
		TamperingDetectedAt *int64 `json:"tamperingDetectedAt"`
		BatteryStatus       *struct {
			Percentage *int  `json:"percentage"`
			IsLow      *bool `json:"isLow"`
		} `json:"batteryStatus"`
		Stats *struct {
			Temperature *struct{ Value *float64 } `json:"temperature"`
			Humidity    *struct{ Value *float64 } `json:"humidity"`
			Light       *struct{ Value *float64 } `json:"light"`
		} `json:"stats"`
	}
	if json.Unmarshal(message.Item, &patch) != nil {
		return
	}
	if patch.IsConnected != nil {
		if *patch.IsConnected {
			s.device.SetAvailable()
		} else {
			s.device.SetUnavailable("De sensor is niet verbonden met de console.")
		}
	}
	values := map[string]any{}
	if patch.IsOpened != nil {
		values["alarm_contact"] = *patch.IsOpened
	}
	if patch.IsMotionDetected != nil {
		values["alarm_motion"] = *patch.IsMotionDetected
	}
	if patch.TamperingDetectedAt != nil {
		values["alarm_tamper"] = true
	}
	if patch.BatteryStatus != nil {
		if patch.BatteryStatus.Percentage != nil {
			values["measure_battery"] = float64(*patch.BatteryStatus.Percentage)
		}
		if patch.BatteryStatus.IsLow != nil {
			values["alarm_battery"] = *patch.BatteryStatus.IsLow
		}
	}
	if patch.Stats != nil {
		if patch.Stats.Temperature != nil && patch.Stats.Temperature.Value != nil {
			values["measure_temperature"] = *patch.Stats.Temperature.Value
		}
		if patch.Stats.Humidity != nil && patch.Stats.Humidity.Value != nil {
			values["measure_humidity"] = *patch.Stats.Humidity.Value
		}
		if patch.Stats.Light != nil && patch.Stats.Light.Value != nil {
			values["measure_luminance"] = *patch.Stats.Light.Value
		}
	}
	report(s.device, values)
}
