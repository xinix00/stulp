package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// Camera's, deurbellen inbegrepen.
//
// Eén driver, want de integratie-API geeft geen manier om het onderscheid te
// maken: getoetst tegen een echte console draagt featureFlags geen hasChime, en
// er is geen type-veld. Twee drivers zouden betekenen dat de gebruiker moet
// raden waar zijn deurbel in staat.
//
// De belkaart hangt daarom aan elke camera. Hij vuurt alleen bij het apparaat
// dat werkelijk een knop heeft, want alleen die stuurt een ring-gebeurtenis.
type cameraDriver struct{}

type cameraDevice struct {
	device  *appsdk.Device
	protect string
	// motionOff stopt de beweging weer als er geen eindbericht komt. De console
	// stuurt dat meestal wel, maar niet altijd, en een tegel die voor altijd
	// "beweging" blijft melden is erger dan een die iets te vroeg opgeeft.
	motionOff *time.Timer
}

const motionHold = 30 * time.Second

func (d cameraDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen UniFi-id")
	}
	return &cameraDevice{device: device, protect: id}, nil
}

// ListDevices vraagt de console wat er is en laat alleen zien wat bij deze
// driver hoort.
func (d cameraDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.api()
	if err != nil {
		return nil, err
	}
	cameras, err := client.Cameras(context.Background())
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(cameras))
	for _, camera := range cameras {
		found = append(found, appsdk.PairedDevice{
			Name:  camera.Name,
			Data:  map[string]any{"id": camera.ID},
			Store: map[string]any{"model": camera.Model, "mac": camera.MAC},
		})
	}
	return found, nil
}

func (c *cameraDevice) OnInit() error {
	instance.watch(c.protect, c)
	if err := c.registerMedia(); err != nil {
		return err
	}
	c.refresh()
	return nil
}

func (c *cameraDevice) OnDeleted() {
	instance.forget(c.protect)
}

// registerMedia meldt het beeld aan. De stream wordt pas bij de console
// opgevraagd als er echt iemand kijkt: het adres draagt een sessie die verloopt,
// dus het nu vastleggen levert een adres op dat straks niet meer werkt.
func (c *cameraDevice) registerMedia() error {
	if err := c.device.SetCameraImage("snapshot", c.device.Name(), func() (appsdk.ImageSource, error) {
		address, err := streamer.snapshot(c.protect)
		if err != nil {
			return appsdk.ImageSource{}, err
		}
		return appsdk.ImageSource{URL: address, ContentType: "image/jpeg"}, nil
	}); err != nil {
		return err
	}
	return c.device.SetCameraVideo("live", c.device.Name(), func() (appsdk.VideoStream, error) {
		client, err := instance.api()
		if err != nil {
			return appsdk.VideoStream{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		streams, err := client.EnableStream(ctx, c.protect, "high")
		if err != nil {
			return appsdk.VideoStream{}, err
		}
		address, ok := streams.Best()
		if !ok {
			return appsdk.VideoStream{}, fmt.Errorf("de console heeft geen stream voor deze camera")
		}
		// Zonder enableSrtp. De console biedt de stream met SRTP aan als je erom
		// vraagt, en dan is elk pakket versleuteld met een sleutel uit de
		// beschrijving. Die laag is hier niet nodig: het verkeer blijft binnen
		// het eigen netwerk en gaat daarna over de verbinding die Stulp al
		// beveiligt. Zie PORTED.md als dat ooit anders moet.
		address = strings.TrimSuffix(address, "?enableSrtp")
		// De console spreekt RTSPS met H.264 en een browser niet. Deze app pakt
		// het om naar fMP4 en bedient dat op zijn eigen luisteraar; Stulp haalt
		// daar op en geeft de bytes door.
		served, mime, err := streamer.open(c.protect, address)
		if err != nil {
			return appsdk.VideoStream{}, err
		}
		// Het volledige type, met codec erin. Een browser bouwt zijn buffer op
		// die string en kan met "video/mp4" alleen niet weten of hij AV1 aankan.
		return appsdk.VideoStream{URL: served, ContentType: mime}, nil
	})
}

// refresh haalt de stand op bij de console.
func (c *cameraDevice) refresh() {
	client, err := instance.api()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	camera, err := client.Camera(ctx, c.protect)
	if err != nil {
		c.device.SetUnavailable("De console antwoordt niet: " + err.Error())
		return
	}
	c.applyCamera(camera)
}

func (c *cameraDevice) applyCamera(camera protect.Camera) {
	if camera.Connected() {
		c.device.SetAvailable()
		return
	}
	c.device.SetUnavailable("De console ziet deze camera niet (" + camera.State + ").")
}

// apply verwerkt een bericht van subscribe/devices.
//
// De console stuurt alleen de velden die veranderd zijn, dus alles wat ontbreekt
// blijft staan. Daarom een aparte struct met pointers: een ontbrekend veld en
// een veld dat op false staat zijn twee verschillende dingen.
func (c *cameraDevice) apply(message protect.DeviceMessage) {
	var patch struct {
		State *string `json:"state"`
	}
	if json.Unmarshal(message.Item, &patch) != nil || patch.State == nil {
		return
	}
	if *patch.State == "CONNECTED" {
		c.device.SetAvailable()
		return
	}
	c.device.SetUnavailable("De console ziet deze camera niet (" + *patch.State + ").")
}

// motion meldt beweging en zet hem na afloop weer uit.
func (c *cameraDevice) motion(active bool) {
	report(c.device, "alarm_motion", active)
	if c.motionOff != nil {
		c.motionOff.Stop()
		c.motionOff = nil
	}
	if active {
		c.motionOff = time.AfterFunc(motionHold, func() {
			report(c.device, "alarm_motion", false)
		})
	}
}

// OnSettings stuurt door wat de gebruiker in de instellingen wijzigde.
func (c *cameraDevice) OnSettings(changed map[string]any) error {
	client, err := instance.api()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if volume, ok := changed["mic_volume"].(float64); ok {
		if err := client.SetCameraMicVolume(ctx, c.protect, int(volume)); err != nil {
			return err
		}
	}
	return nil
}
