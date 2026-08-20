package main

import (
	"strings"
	"testing"
	"time"
)

// Koppelen is start-plus-poll: het startverzoek komt meteen terug en de pagina
// volgt de snapshot. Een verzoek dat minuten hangt sterft onderweg (de tunnel
// kapt hem op zijn timeout af en de gebruiker ziet een 502 terwijl stulp
// doorwerkt — gemeten 20-08); dit patroon houdt élk verzoek kort.
func TestPairCommissionIsStartPlusPoll(t *testing.T) {
	instance := &app{} // geen controller: het koppelen faalt, maar NA de start
	handlers := matterDriver{app: instance}.Pair()

	started, err := handlers["commission"](map[string]any{"code": "34970112332"})
	if err != nil {
		t.Fatalf("de start hoort meteen en zonder fout terug te komen: %v", err)
	}
	snapshot, ok := started.(map[string]any)
	if !ok {
		t.Fatalf("start gaf geen snapshot: %#v", started)
	}
	if running, _ := snapshot["running"].(bool); !running && snapshot["warning"] == nil {
		t.Fatalf("start-snapshot zegt niet dat er iets loopt: %#v", snapshot)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := handlers["commission_state"](nil)
		if err != nil {
			t.Fatal(err)
		}
		snapshot = state.(map[string]any)
		if running, _ := snapshot["running"].(bool); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("het koppelen bleef eeuwig 'running'")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if warning, _ := snapshot["warning"].(string); warning == "" {
		t.Fatalf("zonder controller hoort de mislukking in de snapshot te staan: %#v", snapshot)
	}

	// De naam moet één URL-padsegment kunnen zijn: de emit-route zet hem in het
	// pad (/emit/{event}), de browser codeert een '/' als %2F en leanhttp
	// weigert die dubbelzinnigheid met een 400 — gemeten 20-08 tegen de node,
	// toen deze handler nog "commission/state" heette.
	for name := range handlers {
		if strings.ContainsAny(name, "/?#%") {
			t.Fatalf("koppelbericht %q overleeft de emit-URL niet", name)
		}
	}
}
