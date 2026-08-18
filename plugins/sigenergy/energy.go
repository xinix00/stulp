package main

import (
	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// De netmeter: wat er over de aansluiting gaat, per fase en in totaal, plus de
// twee meterstanden. Hij staat in de systeemregisters, dus op unit 247.
type energyDriver struct{}

type energyDevice struct{ *meter }

func (energyDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	energy := &energyDevice{}
	handle, err := newMeter(energy, device, sigen.Energy, energy.apply, nil)
	if err != nil {
		return nil, err
	}
	energy.meter = handle
	return energy, nil
}

func (energyDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return listByUnit(sigen.Energy, "Sigenergy-netmeter")
}

func (e *energyDevice) apply(values sigen.Reading) map[string]any {
	out := map[string]any{}
	putNumber(out, values, "measure_power", sigen.Energy.Power)
	putNumber(out, values, "measure_power.L1", sigen.Energy.PowerL1)
	putNumber(out, values, "measure_power.L2", sigen.Energy.PowerL2)
	putNumber(out, values, "measure_power.L3", sigen.Energy.PowerL3)
	putNumber(out, values, "meter_power.imported", sigen.Energy.TotalImport)
	putNumber(out, values, "meter_power.exported", sigen.Energy.TotalExport)

	if status, ok := values.Number(sigen.Energy.GridStatus); ok {
		out["grid_status"] = sigen.GridStatus(status)
	}
	if control, ok := values.Number(sigen.Energy.PhaseControl); ok {
		out["phase_control"] = sigen.PhaseControl(control)
	}
	return out
}
