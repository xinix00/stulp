package main

import (
	"fmt"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// De AC-laadpaal: laadstand, vermogen, meterstand, en starten en stoppen.
//
// Dit is het enige apparaat in deze app dat ook schrijft. Dat gaat naar één
// register: 1 start de laadbeurt, 0 stopt hem.
type chargerDriver struct{}

type chargerDevice struct {
	*meter

	// guard hoort bij het laatst gemeten vermogen. Het systeem meldt geen
	// laadpaalvermogen als eigen register, dus telt de systeemtegel op wat de
	// laders hier melden.
	guard sync.Mutex
	power float64
}

func (chargerDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	charger := &chargerDevice{}
	handle, err := newMeter(charger, device, sigen.EvACCharger, charger.apply, nil)
	if err != nil {
		return nil, err
	}
	charger.meter = handle
	return charger, nil
}

// ListDevices zoekt de units die een laadstand aanbieden. De AC-lader heeft geen
// serienummer in zijn registers -- de bron leest het ook niet -- dus het unit-id
// is waaraan hij te herkennen is.
func (chargerDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return listByUnit(sigen.EvACCharger, "Sigenergy AC-laadpaal")
}

func (c *chargerDevice) apply(values sigen.Reading) map[string]any {
	out := map[string]any{}
	putNumber(out, values, "meter_power.charged", sigen.EvACCharger.TotalCharged)

	// Het register staat in kW en measure_power gaat in watt; de bron zet het
	// ongewijzigd door en meldt daarmee 7 W voor een lader die 7 kW levert.
	kilowatt, powerOK := values.Number(sigen.EvACCharger.Power)
	if !powerOK {
		return out
	}
	watt := sigen.WattFromKilowatt(kilowatt)
	out["measure_power"] = watt

	c.guard.Lock()
	c.power = watt
	c.guard.Unlock()

	if status, ok := values.Number(sigen.EvACCharger.Status); ok {
		out["evcharger_charging"] = watt > 0
		out["evcharger_charging_state"] = sigen.ACChargerChargingState(status, watt)
	}
	return out
}

// lastPower is wat deze lader het laatst trok, voor de systeemtegel.
func (c *chargerDevice) lastPower() float64 {
	c.guard.Lock()
	defer c.guard.Unlock()
	return c.power
}

// OnCapability start en stopt de laadbeurt.
//
// De bevestiging komt van de volgende ronde: het register zegt of het gelukt is,
// en meteen terugmelden dat er geladen wordt terwijl de auto nog niet reageert
// zou een tegel opleveren die iets anders zegt dan de lader doet. Wat hier wel
// meteen terugkomt, is dat de opdracht is aangenomen.
func (c *chargerDevice) OnCapability(name string, value any) error {
	if name != "evcharger_charging" {
		return fmt.Errorf("deze laadpaal kent %q niet", name)
	}
	client, err := instance.api()
	if err != nil {
		return err
	}
	command := sigen.EvACChargerStop
	on, _ := value.(bool)
	if on {
		command = sigen.EvACChargerStart
	}
	if err := client.WriteHolding(c.Unit(), sigen.EvACChargerControl, []uint16{command}); err != nil {
		return err
	}
	return nil
}
