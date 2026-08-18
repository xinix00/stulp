package sigen

import "fmt"

// De betekenis achter de standregisters, uit lib/enums.js van de bron.
//
// Alleen wat de geporte drivers gebruiken staat hier. De standen van de
// DC-lader zijn niet overgenomen omdat die driver niet mee is; zie PORTED.md.

// unknown is wat een stand krijgt die niet in de lijst staat. Met het getal
// erbij, want dan is er tenminste iets om op te zoeken in de protocolbeschrijving
// -- een lege tegel zegt niets en "onbekend" ook niet.
func unknown(value float64) string { return fmt.Sprintf("onbekend (%.0f)", value) }

// BatteryChargingState vertaalt bedrijfsstand plus vermogen naar de drie
// waarden die battery_charging_state kent.
//
// Alleen een draaiende batterij laadt of ontlaadt; alle andere standen -- stil,
// storing, uit, afwijkend -- zijn stilstand. Het teken van het vermogen bepaalt
// welke kant het op gaat.
func BatteryChargingState(status, power float64) string {
	switch int(status) {
	case 1: // draait
		if power > 0 {
			return "charging"
		}
		return "discharging"
	default: // 0 stil, 2 storing, 3 uit, 7 afwijkend, en wat er verder komt
		return "idle"
	}
}

// GridStatus is de netstand van het systeem.
func GridStatus(value float64) string {
	switch int(value) {
	case 0:
		return "Op het net"
	case 1:
		return "Van het net"
	case 2:
		return "Van het net (handmatig)"
	}
	return unknown(value)
}

// PhaseControl zegt of het systeem de fasen los van elkaar stuurt.
func PhaseControl(value float64) string {
	switch int(value) {
	case 0:
		return "Uit"
	case 1:
		return "Aan"
	}
	return unknown(value)
}

// InverterOutputType is hoe de omvormer zijn wisselspanning levert. Bepaalt of
// er één fase of drie te melden zijn.
func InverterOutputType(value float64) string {
	switch int(value) {
	case 0:
		return "L/N"
	case 1:
		return "L1/L2/L3"
	case 2:
		return "L1/L2/L3/N"
	case 3:
		return "L1/L2/N"
	}
	return unknown(value)
}

// ThreePhase zegt of een uitgangstype drie fasen levert.
func ThreePhase(outputType string) bool {
	return outputType == "L1/L2/L3" || outputType == "L1/L2/L3/N"
}

// ACChargerChargingState vertaalt de IEC-stand van de AC-lader plus het
// vermogen naar evcharger_charging_state.
//
// De IEC-standen komen uit bijlage 14 van de protocolbeschrijving zoals de bron
// ze opschrijft: 0 opstarten, 1 niets aangesloten (A), 2 en 3 aangesloten maar
// niet gereed (B1/B2), 4 en 5 laden (C1/C2), 6 storing (F), 7 geen voeding (E).
func ACChargerChargingState(status, power float64) string {
	switch int(status) {
	case 2, 3:
		return "plugged_in"
	case 4, 5:
		if power > 0 {
			return "plugged_in_charging"
		}
		return "plugged_in"
	default: // 0 opstarten, 1 niets aangesloten, 6 storing, 7 geen voeding
		return "plugged_out"
	}
}
