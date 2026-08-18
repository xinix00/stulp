package wiim

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// Zoeken op het echte netwerk.
//
// Draait alleen met STULP_WIIM_LIVE=1: multicast op een willekeurige machine
// levert willekeurige uitslagen, en een test die daarvan afhangt zegt niets.
func TestSearchTheRealNetwork(t *testing.T) {
	if os.Getenv("STULP_WIIM_LIVE") == "" {
		t.Skip("geen STULP_WIIM_LIVE; deze toets zoekt op het echte netwerk")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Eerst alles wat antwoordt, ongefilterd: dat scheidt "niets bereikt ons"
	// van "wel antwoorden, maar geen speler ertussen".
	options := SearchOptions{Wait: 6 * time.Second}
	if address := os.Getenv("STULP_WIIM_INTERFACE"); address != "" {
		options.Interface = net.ParseIP(address)
	}
	answers, err := Search(ctx, options)
	if err != nil {
		t.Fatalf("zoeken: %v", err)
	}
	t.Logf("  %d SSDP-antwoorden", len(answers))
	seen := map[string]bool{}
	for _, answer := range answers {
		if seen[answer.Location] {
			continue
		}
		seen[answer.Location] = true
		t.Logf("    %s", answer.Location)
	}

	players, err := Discover(ctx, options)
	if err != nil {
		t.Fatalf("ontdekken: %v", err)
	}
	t.Logf("  %d spelers herkend", len(players))
	for _, player := range players {
		t.Logf("    %s (%s) op %s:%d", player.Name, player.Model, player.Address, player.Port)
	}
}
