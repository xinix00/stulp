package main

import (
	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// De batterij: laadtoestand, vermogen, de meterstanden en de temperaturen van
// de koudste en de warmste cel.
//
// Het vermogen komt niet van de batterij zelf maar uit de systeemregisters op
// unit 247. De batterij-unit biedt het niet aan; de bron doet hetzelfde.
type batteryDriver struct{}

type batteryDevice struct{ *meter }

func (batteryDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	battery := &batteryDevice{}
	handle, err := newMeter(battery, device, sigen.Battery, battery.apply, battery.applyInfo)
	if err != nil {
		return nil, err
	}
	battery.meter = handle
	return battery, nil
}

func (batteryDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return listBySerial(sigen.Battery, sigen.Battery.Serial, "Sigenergy-batterij")
}

func (b *batteryDevice) applyInfo(values sigen.Reading) {
	store := map[string]any{}
	if serial, ok := values.Text(sigen.Battery.Serial); ok && serial != "" {
		store["serial"] = serial
	}
	if capacity, ok := values.Number(sigen.Battery.Capacity); ok {
		store["capacity"] = round(capacity)
	}
	if len(store) > 0 {
		b.device.SetStore(store)
	}
}

func (b *batteryDevice) apply(values sigen.Reading) map[string]any {
	out := map[string]any{}
	putNumber(out, values, "measure_battery", sigen.Battery.SoC)
	putNumber(out, values, "measure_power", sigen.Battery.Power)
	putNumber(out, values, "meter_power.charged", sigen.Battery.TotalCharged)
	putNumber(out, values, "meter_power.discharged", sigen.Battery.TotalDischarged)
	putNumber(out, values, "measure_temperature.minCell", sigen.Battery.MinCellTemp)
	putNumber(out, values, "measure_temperature.maxCell", sigen.Battery.MaxCellTemp)
	putNumber(out, values, "measure_temperature.pcs", sigen.Battery.PCSTemp)
	putText(out, values, "firmware", sigen.Battery.Firmware)

	// Laden of ontladen volgt uit de bedrijfsstand plus het teken van het
	// vermogen. Zonder het vermogen valt er niets over te zeggen, en dan blijft
	// de tegel liever staan dan dat er "stil" komt te staan terwijl hij laadt.
	power, powerOK := values.Number(sigen.Battery.Power)
	if status, ok := values.Number(sigen.Battery.Status); ok && powerOK {
		out["battery_charging_state"] = sigen.BatteryChargingState(status, power)
	}
	return out
}
