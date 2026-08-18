package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// De gong. De console kent alleen een volume van 0 tot 100; uit is volume nul.
//
// Daarom onthoudt dit apparaat het laatste volume dat niet nul was: wie op de
// tegel uit en weer aan drukt wil zijn eigen volume terug, niet honderd.
type chimeDriver struct{}

type chimeDevice struct {
	device  *appsdk.Device
	protect string
}

const lastVolumeKey = "lastVolume"

func (chimeDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen UniFi-id")
	}
	return &chimeDevice{device: device, protect: id}, nil
}

func (chimeDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	chimes, err := client.Chimes(context.Background())
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(chimes))
	for _, chime := range chimes {
		found = append(found, appsdk.PairedDevice{
			Name:  chime.Name,
			Data:  map[string]any{"id": chime.ID},
			Store: map[string]any{"model": chime.Model},
		})
	}
	return found, nil
}

func (c *chimeDevice) OnInit() error {
	instance.watch(c.protect, c)
	c.refresh()
	return nil
}

func (c *chimeDevice) OnDeleted() {
	instance.forget(c.protect)
}

func (c *chimeDevice) refresh() {
	client, err := instance.api()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	chime, err := client.Chime(ctx, c.protect)
	if err != nil {
		c.device.SetUnavailable("De console antwoordt niet: " + err.Error())
		return
	}
	if chime.IsConnected {
		c.device.SetAvailable()
	} else {
		c.device.SetUnavailable("De gong is niet verbonden met de console.")
	}
	c.applyVolume(chime.Volume)
}

func (c *chimeDevice) applyVolume(volume int) {
	report(c.device, "onoff", volume > 0)
	report(c.device, "volume_set", float64(volume)/100)
	if volume > 0 {
		c.device.SetStore(map[string]any{lastVolumeKey: volume})
	}
}

// apply verwerkt een deelbericht van de websocket. De pointers houden een
// ontbrekend veld uit elkaar van een echte nul -- volume 0 is een geldige stand.
func (c *chimeDevice) apply(message protect.DeviceMessage) {
	var patch struct {
		IsConnected *bool `json:"isConnected"`
		Volume      *int  `json:"volume"`
	}
	if json.Unmarshal(message.Item, &patch) != nil {
		return
	}
	if patch.IsConnected != nil {
		if *patch.IsConnected {
			c.device.SetAvailable()
		} else {
			c.device.SetUnavailable("De gong is niet verbonden met de console.")
		}
	}
	if patch.Volume != nil {
		c.applyVolume(*patch.Volume)
	}
}

func (c *chimeDevice) OnCapability(name string, value any) error {
	client, err := instance.api()
	if err != nil {
		return err
	}
	volume := 0
	switch name {
	case "onoff":
		if on, _ := value.(bool); on {
			volume = c.lastVolume()
		}
	case "volume_set":
		level, _ := value.(float64)
		volume = int(level * 100)
	default:
		return fmt.Errorf("deze gong kent %q niet", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.SetChimeVolume(ctx, c.protect, volume); err != nil {
		return err
	}
	c.applyVolume(volume)
	return nil
}

// lastVolume levert waar de gong op stond voordat hij uitging, of een redelijk
// begin als hij nog nooit aan is geweest.
func (c *chimeDevice) lastVolume() int {
	if stored, ok := c.device.StoreValue(lastVolumeKey); ok {
		if volume, ok := stored.(float64); ok && volume > 0 {
			return int(volume)
		}
	}
	return 100
}
