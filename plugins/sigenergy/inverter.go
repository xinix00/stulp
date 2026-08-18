package main

import (
	"fmt"
	"sync"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/sigen"
)

// De omvormer: vermogen, opbrengst, de spanning per PV-ingang en de spanning en
// stroom per fase.
//
// Hoeveel PV-ingangen er zijn en hoeveel fasen hij levert staat in de omvormer
// zelf, in twee registers die één keer gelezen worden. De tegels volgen dat:
// een omvormer met twee ingangen krijgt geen vier lege spanningen.
type inverterDriver struct{}

type inverterDevice struct {
	*meter

	// guard hoort bij wat de omvormer over zichzelf vertelde. Apart van de lock
	// in meter, want die gaat over de leesronden.
	guard      sync.Mutex
	mppt       int
	threePhase bool
	configured bool
}

func (inverterDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	inverter := &inverterDevice{}
	handle, err := newMeter(inverter, device, sigen.Inverter, inverter.apply, inverter.applyInfo)
	if err != nil {
		return nil, err
	}
	inverter.meter = handle
	return inverter, nil
}

// ListDevices zoekt de units die een serienummer aanbieden op het adres waar de
// omvormer het heeft staan. Dat is hetzelfde register waarmee de bron een
// omvormer herkende.
func (inverterDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return listBySerial(sigen.Inverter, sigen.Inverter.Serial, "Sigenergy-omvormer")
}

// applyInfo verwerkt wat de omvormer over zichzelf zegt.
func (i *inverterDevice) applyInfo(values sigen.Reading) {
	if serial, ok := values.Text(sigen.Inverter.Serial); ok && serial != "" {
		i.device.SetStore(map[string]any{"serial": serial})
	}

	mppt, mpptOK := values.Number(sigen.Inverter.MPPTCount)
	outputType, typeOK := values.Number(sigen.Inverter.OutputType)
	if !mpptOK || !typeOK {
		// Zonder deze twee is niet te zeggen welke tegels ergens over gaan. De
		// volgende verbinding probeert het opnieuw; tot die tijd blijft de
		// omvormer zijn vermogen en opbrengst melden en verder niets.
		i.device.Error("de omvormer meldt niet hoeveel PV-ingangen of fasen hij heeft")
		return
	}
	name := sigen.InverterOutputType(outputType)
	threePhase := sigen.ThreePhase(name)

	i.guard.Lock()
	i.mppt, i.threePhase, i.configured = int(mppt), threePhase, true
	i.guard.Unlock()

	i.device.SetStore(map[string]any{"outputType": name, "mpptCount": int(mppt)})
	i.configureCapabilities(int(mppt), threePhase)
}

// configureCapabilities laat de tegels overeenkomen met wat deze omvormer heeft.
// Een spanning van een PV-ingang die er niet is, is geen meting maar een lege
// tegel waar niemand iets aan heeft.
func (i *inverterDevice) configureCapabilities(mppt int, threePhase bool) {
	for index := range sigen.Inverter.PVVoltages() {
		name := fmt.Sprintf("measure_voltage.pv%d", index+1)
		if index < mppt {
			i.ensureCapability(name)
		} else {
			i.dropCapability(name)
		}
	}
	for _, phase := range []string{"phaseB", "phaseC"} {
		if threePhase {
			i.ensureCapability("measure_voltage." + phase)
			i.ensureCapability("measure_current." + phase)
		} else {
			i.dropCapability("measure_voltage." + phase)
			i.dropCapability("measure_current." + phase)
		}
	}
}

func (i *inverterDevice) apply(values sigen.Reading) map[string]any {
	out := map[string]any{}
	putNumber(out, values, "measure_power", sigen.Inverter.Power)
	putNumber(out, values, "meter_power.daily", sigen.Inverter.DailyYield)
	putNumber(out, values, "meter_power", sigen.Inverter.TotalYield)

	i.guard.Lock()
	mppt, threePhase, configured := i.mppt, i.threePhase, i.configured
	i.guard.Unlock()

	// Zolang de omvormer nog niet verteld heeft wat hij is worden alleen de
	// tegels gevuld die hoe dan ook kloppen. Anders zou een enkelfasige omvormer
	// even drie fasen melden -- en die twee lezen dan de spanning van een fase
	// die er niet is.
	if !configured {
		return out
	}

	for index, reg := range sigen.Inverter.PVVoltages() {
		if index >= mppt {
			break
		}
		putNumber(out, values, fmt.Sprintf("measure_voltage.pv%d", index+1), reg)
	}

	putNumber(out, values, "measure_voltage.phaseA", sigen.Inverter.PhaseAVoltage)
	putNumber(out, values, "measure_current.phaseA", sigen.Inverter.PhaseACurrent)
	if !threePhase {
		return out
	}
	putNumber(out, values, "measure_voltage.phaseB", sigen.Inverter.PhaseBVoltage)
	putNumber(out, values, "measure_current.phaseB", sigen.Inverter.PhaseBCurrent)
	putNumber(out, values, "measure_voltage.phaseC", sigen.Inverter.PhaseCVoltage)
	putNumber(out, values, "measure_current.phaseC", sigen.Inverter.PhaseCCurrent)
	return out
}
