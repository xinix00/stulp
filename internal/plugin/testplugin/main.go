// Command testplugin is de plugin die de tests van Stulp starten.
//
// Hij hoort bij geen enkel apparaat: hij bestaat om de weg te beproeven die
// elke plugin aflegt -- starten, zich melden, een instelling zetten, weer
// stoppen. Wat hij precies doet komt uit STULP_TEST_PLUGIN, zodat één binary
// alle testgevallen dekt.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
)

// pairDriver doet wat elke echte driver doet: de identiteit uit Data() halen en
// weigeren als die er niet is. Daarmee is dit meteen de toets of Stulp een net
// gekoppeld apparaat bij de app aflevert vóórdat hij het laat starten.
type pairDriver struct{}

type pairDevice struct{ device *appsdk.Device }

func (pairDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	id, _ := device.Data()["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("dit apparaat heeft geen id")
	}
	return &pairDevice{device: device}, nil
}

func (pairDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return []appsdk.PairedDevice{{Name: "Ding", Data: map[string]any{"id": "ding-1"}}}, nil
}

// OnInit zet een spoor in zijn eigen store. Zo kan een test zien dát dit
// apparaat bij de app is aangekomen -- device.init is anders van buiten niet te
// onderscheiden van een apparaat dat de app nooit gekregen heeft.
func (d *pairDevice) OnInit() error {
	return d.device.SetStore(map[string]any{"testplugin.initialised": true})
}

func main() {
	appsdk.Run(appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"thing": pairDriver{}},
		OnInit: func(h *appsdk.Stulp) error {
			// Een app die er na het starten mee ophoudt. Dat is wat een
			// crash in het echt doet, en het is het geval waarvan Stulp moet
			// merken dat het gebeurd is.
			if os.Getenv("STULP_TEST_PLUGIN") == "exit" {
				go func() {
					time.Sleep(50 * time.Millisecond)
					os.Exit(3)
				}()
				return nil
			}
			if os.Getenv("STULP_TEST_PLUGIN") != "slow" {
				return nil
			}
			// Melden dát we begonnen zijn en daarna blijven staan tot de test
			// ons loslaat. De supervisor hoort ondertussen gewoon aanspreekbaar
			// te blijven.
			//
			// Wachten op een teken en niet op de klok: met een vaste slaap is
			// het een race wie er eerder is, en op een drukke machine wint de
			// slaap. Dan meet de test niets meer.
			if err := h.SetSetting("entered", true); err != nil {
				return err
			}
			// Het teken komt langs het protocol heen, via een bestand. Alles wat
			// Stulp naar ons stuurt wacht achter deze OnInit op dezelfde
			// worker, dus een instelling zetten zou de test laten vastlopen op
			// de app die op die instelling wacht.
			release := os.Getenv("STULP_TEST_RELEASE")
			for release != "" {
				if _, err := os.Stat(release); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			return nil
		},
	})
}
