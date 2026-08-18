package main

import (
	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// Het systeem als geheel: wat er van het net komt, wat de zon geeft, wat het
// huis gebruikt en wat de batterij doet. Eén tegel voor het hele plaatje.
type plantDriver struct{}

type plantDevice struct{ *meter }

func (plantDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	plant := &plantDevice{}
	handle, err := newMeter(plant, device, sigen.Plant, plant.apply, nil)
	if err != nil {
		return nil, err
	}
	plant.meter = handle
	return plant, nil
}

// ListDevices tast af welke unit het netvermogen aanbiedt. Dat is hetzelfde
// register waarmee de bron zijn systeem herkende, en in de praktijk is dat
// unit 247 -- maar het wordt gevraagd en niet aangenomen.
func (plantDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return listByUnit(sigen.Plant, "Sigenergy-systeem")
}

func (p *plantDevice) apply(values sigen.Reading) map[string]any {
	out := map[string]any{}
	putNumber(out, values, "measure_power.grid", sigen.Plant.GridPower)
	putNumber(out, values, "measure_power.battery", sigen.Plant.BatteryPower)
	putNumber(out, values, "measure_power.load", sigen.Plant.GeneralLoadPower)
	putNumber(out, values, "measure_battery", sigen.Plant.BatterySoC)

	// Zonvermogen is dat van de Sigenergy-omvormers plus dat van een omvormer
	// van een ander merk die aan hetzelfde systeem hangt. De bron telt ze net zo
	// bij elkaar op: op de tegel staat wat de zon geeft, niet wie het omzette.
	own, ownOK := values.Number(sigen.Plant.SolarPower)
	third, thirdOK := values.Number(sigen.Plant.ThirdPartyInverterPower)
	if ownOK || thirdOK {
		out["measure_power.solar"] = own + third
	}

	// Het systeem meldt het laadpaalvermogen niet als eigen register; het komt
	// van de laadpalen die in deze app gekoppeld zijn. Zonder gekoppelde lader
	// staat er nul, en dat is precies wat het systeem er dan van weet.
	out["measure_power.evcharger"] = instance.chargerPower()

	if status, ok := values.Number(sigen.Plant.GridStatus); ok {
		out["grid_status"] = sigen.GridStatus(status)
	}
	return out
}
