package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Stil wachten op de route is bedoeld voor één opstartmoment, niet voor een
// toestand. Vóór 21-08 lag daar geen bovengrens: de tak lustte eeuwig door,
// dus na een restore hielden achtentwintig workers stil een "subscription"
// vast die nooit bestond -- en omdat markNodeUnavailable werd overgeslagen,
// bleef élke tegel op UNDEFINED staan. Stilte die eruitzag als een werkende
// node. Deze test pint beide helften: binnen het venster niets grijs, erbuiten
// doorvallen naar het gedeelde pad dat markeert én blijft proberen.
func TestRouteWaitIsQuietThenFallsThrough(t *testing.T) {
	routeLoud, routePoll := routeWaitLoud, routeWaitPoll
	routeWaitLoud, routeWaitPoll = 150*time.Millisecond, 5*time.Millisecond
	defer func() { routeWaitLoud, routeWaitPoll = routeLoud, routePoll }()

	const nodeID = 0x4242
	database := newBacking()
	device := addRecoveryMatterDevice(t, database, nodeID, 1, 7, "127.0.0.1:5540")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	controller := &Controller{
		store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: ctx,
		workers: make(map[uint64]context.CancelFunc), subscriptions: make(map[uint64]activeSubscription),
	}

	var calls atomic.Int64
	routeErr := errors.New("send CASE Sigma1: write udp6: leannet: no IPv6 route (no router advertised)")
	controller.wg.Add(1)
	go controller.maintainSubscription(ctx, nodeID, func(context.Context, uint64) error {
		calls.Add(1)
		return routeErr
	})

	// Binnen het venster: stil. De worker schrijft niets over de tegel heen --
	// wat er staat is al eerlijk: een start van Stulp begint onbereikbaar
	// (bereikbaarheid is state en overleeft het document niet), en mid-run
	// staat er een toestand die deze run zelf heeft waargemaakt. De eerste
	// poging ná de router advertisement slaagt gewoon.
	time.Sleep(60 * time.Millisecond)
	early, err := database.Device(context.Background(), device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !early.Available {
		t.Fatalf("binnen routeWaitLoud hoort niets grijs te gaan: %#v", early)
	}
	if calls.Load() < 2 {
		t.Fatalf("binnen het venster hoort hij stil te blijven kijken, kreeg %d pogingen", calls.Load())
	}

	// Erbuiten: doorvallen. De node gaat grijs mét de échte fout erin, en de
	// backoff eronder blijft proberen -- niemand houdt nog iets vast.
	deadline := time.Now().Add(3 * time.Second)
	var late Device
	for time.Now().Before(deadline) {
		late, err = database.Device(context.Background(), device.ID)
		if err == nil && !late.Available {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if late.Available {
		t.Fatal("na routeWaitLoud hoort de node onbereikbaar te zijn, niet stil vastgehouden")
	}
	if !strings.Contains(late.Message, "no IPv6 route") {
		t.Fatalf("de melding hoort de échte oorzaak te dragen, kreeg %q", late.Message)
	}
	before := calls.Load()
	time.Sleep(1200 * time.Millisecond)
	if calls.Load() <= before {
		t.Fatal("na het doorvallen hoort de backoff het te blijven proberen")
	}
}
