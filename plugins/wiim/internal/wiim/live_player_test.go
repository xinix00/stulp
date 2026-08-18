package wiim

import (
	"context"
	"os"
	"testing"
	"time"
)

// Tegen een echte speler.
//
// Draait alleen met STULP_WIIM_ADDRESS. Wat hier getoetst wordt is wat een
// nagebouwde speler niet kan bewijzen: dat de standen aankomen zoals wij ze
// lezen, en dat een opdracht aangenomen wordt.
func TestAgainstARealPlayer(t *testing.T) {
	address := os.Getenv("STULP_WIIM_ADDRESS")
	if address == "" {
		t.Skip("geen STULP_WIIM_ADDRESS; deze toets vraagt een echte speler")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := New(address)
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("de stand opvragen: %v", err)
	}
	t.Logf("  toestand:  %q", status.State)
	t.Logf("  titel:     %q  (metadata aanwezig: %v)", status.Track.Title, status.Track.Present)
	t.Logf("  artiest:   %q", status.Track.Artist)
	t.Logf("  album:     %q", status.Track.Album)
	t.Logf("  hoes:      %q", status.Track.ArtURI)
	t.Logf("  volume:    %v  gedempt=%v", status.Volume, status.Muted)
	t.Logf("  positie:   %v van %v", status.Position, status.Duration)
	t.Logf("  loop:      raw=%q shuffle=%v herhaal=%q bekend=%v",
		status.Loop.Raw, status.Loop.Shuffle, status.Loop.Repeat, status.Loop.Known)

	// En de andere weg naar de speler: httpapi.asp over https. Poort 80 staat
	// dicht op deze speler, dus dat pad moest kloppen.
	answer, err := client.Command(ctx, "getStatusEx")
	if err != nil {
		t.Fatalf("httpapi.asp: %v", err)
	}
	if len(answer) > 200 {
		answer = answer[:200] + "…"
	}
	t.Logf("  getStatusEx: %s", answer)
}
