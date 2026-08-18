package sigen

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/sigenergy/internal/modbus"
)

// Tegen een echt systeem.
//
// Draait alleen met STULP_SIGEN_HOST. Wat hier getoetst wordt is wat een
// nagebouwde server niet kan bewijzen: dat de registers staan waar de bron zegt,
// dat de schalen kloppen, en dat het aftasten van unit-ids werkt.
func TestAgainstARealSystem(t *testing.T) {
	host := os.Getenv("STULP_SIGEN_HOST")
	if host == "" {
		t.Skip("geen STULP_SIGEN_HOST; deze toets vraagt een echt systeem")
	}
	port := 502
	if text := os.Getenv("STULP_SIGEN_PORT"); text != "" {
		port, _ = strconv.Atoi(text)
	}
	client := modbus.New(host, port, 10*time.Second)
	defer client.Close()

	// Eerst het systeem: unit 247 draagt de plantregisters.
	t.Log("  === unit 247, het systeem ===")
	report(t, client, 247, Plant.All())

	// Dan aftasten welke unit-ids er verder antwoorden.
	t.Log("  === welke units antwoorden ===")
	for unit := uint8(1); unit <= 8; unit++ {
		start := time.Now()
		_, err := client.ReadHolding(unit, 30500, 1)
		switch {
		case err == nil:
			t.Logf("    unit %d: antwoordt (%v)", unit, time.Since(start).Round(time.Millisecond))
			report(t, client, unit, Inverter.All())
			report(t, client, unit, Battery.All())
		default:
			t.Logf("    unit %d: %v (%v)", unit, err, time.Since(start).Round(time.Millisecond))
		}
	}
}

// report leest een set en toont wat eruit komt.
func report(t *testing.T, client *modbus.Client, unit uint8, set Set) {
	t.Helper()
	reading, err := NewPoller(unit, set).Read(client)
	if err != nil {
		t.Logf("    lezen mislukte: %v", err)
		return
	}
	shown := 0
	for _, reg := range set {
		if value, ok := reading.Number(reg); ok {
			t.Logf("      %-38s %v", reg.What, value)
			shown++
		} else if text, ok := reading.Text(reg); ok && text != "" {
			t.Logf("      %-38s %q", reg.What, text)
			shown++
		}
		if shown >= 30 {
			t.Log("      (rest ingekort)")
			return
		}
	}
	if shown == 0 {
		t.Log("      niets bruikbaars teruggekregen")
	}
}

// Wat unit 1 werkelijk aanbiedt. Het herkenningsregister bleek er niet te zijn,
// dus de vraag is welke registers dan wel.
func TestWhatUnitOneOffers(t *testing.T) {
	host := os.Getenv("STULP_SIGEN_HOST")
	if host == "" {
		t.Skip("geen STULP_SIGEN_HOST")
	}
	client := modbus.New(host, 502, 4*time.Second)
	defer client.Close()

	t.Log("  losse registers aftasten:")
	for _, probe := range []struct {
		addr uint16
		what string
	}{
		{30500, "omvormer serienummer (bron)"},
		{30501, "omvormer serie +1"},
		{30515, "serienummer volgens de bron"},
		{31000, "omvormer meting"},
		{30578, "batterij"},
		{31025, "mppt"},
		{31026, "aantal mppt"},
		{32000, "ev-lader"},
		{40000, "schrijfbereik"},
	} {
		words, err := client.ReadHolding(1, probe.addr, 1)
		if err != nil {
			t.Logf("    %-6d %-30s %v", probe.addr, probe.what, shorten(err.Error()))
			continue
		}
		t.Logf("    %-6d %-30s = %v", probe.addr, probe.what, words)
	}

	t.Log("  de omvormerset:")
	report(t, client, 1, Inverter.All())
	t.Log("  de batterijset:")
	report(t, client, 1, Battery.All())
}

func shorten(text string) string {
	if len(text) > 70 {
		return text[len(text)-70:]
	}
	return text
}

// Het aftasten zoals de plugin het doet: met het probe-register per soort, niet
// met een zelfgekozen adres. Dit is wat de koppelpagina zou tonen.
func TestScanLikeThePluginDoes(t *testing.T) {
	host := os.Getenv("STULP_SIGEN_HOST")
	if host == "" {
		t.Skip("geen STULP_SIGEN_HOST")
	}
	// Korte wachttijd: op een LAN antwoordt een unit die er is binnen enkele
	// milliseconden. Wie er niet is zwijgt, en daar wachten we niet tien
	// seconden op -- zie de bevinding in PORTED.md.
	client := modbus.New(host, 502, 1500*time.Millisecond)
	defer client.Close()

	for _, card := range []struct {
		name string
		reg  Reg
	}{
		{"systeem", Plant.Probe()},
		{"omvormer", Inverter.Probe()},
		{"batterij", Battery.Probe()},
		{"meter", Energy.Probe()},
		{"ev-lader", EvACCharger.Probe()},
	} {
		found := []uint8{}
		start := time.Now()
		for _, unit := range []uint8{1, 2, 3, 4, 247} {
			if _, err := client.ReadHolding(unit, card.reg.Addr, card.reg.Count); err == nil {
				found = append(found, unit)
			}
		}
		t.Logf("  %-9s probe %-5d op units %v  (%v)",
			card.name, card.reg.Addr, found, time.Since(start).Round(time.Millisecond))
	}
}
