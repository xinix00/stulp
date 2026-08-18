package tahoma

import (
	"context"
	"os"
	"testing"
	"time"
)

// Tegen het echte TaHoma.
//
// Draait alleen als STULP_SOMFY_USER en STULP_SOMFY_PASSWORD gezet zijn. Wat een
// nagebouwde server niet kan bewijzen staat hier: dat Somfy het inlogformulier
// aanneemt zoals wij het sturen, en dat /setup de apparaten teruggeeft in de
// vorm die de drivers lezen.
//
// Eén login per run. Somfy knijpt af op herhaald inloggen, en een test die dat
// uitlokt is een test die de gebruiker zijn account kost.
func TestAgainstRealTahoma(t *testing.T) {
	user, password := os.Getenv("STULP_SOMFY_USER"), os.Getenv("STULP_SOMFY_PASSWORD")
	if user == "" || password == "" {
		t.Skip("geen STULP_SOMFY_USER/PASSWORD; deze toets vraagt een echt account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := New(user, password)
	setup, err := client.Setup(ctx)
	if err != nil {
		t.Fatalf("inloggen en /setup ophalen: %v", err)
	}
	devices := setup.Devices
	t.Logf("  %d apparaten", len(devices))

	kinds := map[string]int{}
	states := map[string]int{}
	for _, device := range devices {
		kinds[device.ControllableName]++
		for _, state := range device.States {
			states[state.Name]++
		}
	}
	for name, count := range kinds {
		t.Logf("    %-52s %d", name, count)
	}
	t.Logf("  standen die voorkomen:")
	for name, count := range states {
		t.Logf("    %-52s %d", name, count)
	}
	if len(devices) == 0 {
		t.Fatal("het account meldt geen enkel apparaat")
	}
}

// Wat de apparaten van dit account kunnen. Dit is de bron voor de tabel in
// covering.go: welke commando's een soort aanbiedt bepaalt of hij te bedienen is.
func TestWhatTheRealDevicesOffer(t *testing.T) {
	user, password := os.Getenv("STULP_SOMFY_USER"), os.Getenv("STULP_SOMFY_PASSWORD")
	if user == "" || password == "" {
		t.Skip("geen account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	setup, err := New(user, password).Setup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, device := range setup.Devices {
		if seen[device.ControllableName] {
			continue
		}
		seen[device.ControllableName] = true
		t.Logf("  %s (%s)", device.ControllableName, device.Label)
		for _, state := range device.States {
			t.Logf("    %s = %v", state.Name, state.Value)
		}
	}
}

// De namen die dit account meldt, zodat de tabel in covering.go erop getoetst
// kan worden zonder een hele Stulp te starten.
func TestRealControllableNames(t *testing.T) {
	user, password := os.Getenv("STULP_SOMFY_USER"), os.Getenv("STULP_SOMFY_PASSWORD")
	if user == "" || password == "" {
		t.Skip("geen account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	setup, err := New(user, password).Setup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withClosure := 0
	for _, device := range setup.Devices {
		if _, ok := device.Number(StateClosure); ok {
			withClosure++
			t.Logf("  bedienbaar: %-14s %s", device.ControllableName, device.Label)
		}
	}
	if withClosure == 0 {
		t.Fatal("geen enkel apparaat met een sluitingsstand")
	}
}
