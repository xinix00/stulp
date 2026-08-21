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
	routeLoud, retryInitial, retryMaximum, retryJitter :=
		routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter
	routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter =
		150*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, 0
	defer func() {
		routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter =
			routeLoud, retryInitial, retryMaximum, retryJitter
	}()

	const nodeID = 0x4242
	database := newBacking()
	device := addRecoveryMatterDevice(t, database, nodeID, 1, 7, "127.0.0.1:5540")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	controller := &Controller{
		store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: ctx,
		workers: make(map[uint64]context.CancelFunc), subscriptions: make(map[uint64]activeSubscription),
	}
	defer func() {
		cancel()
		controller.wg.Wait()
	}()

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

// A missing route belongs to the controller's IPv6 stack, not to one Matter
// node. This test starts the same number of workers as a modest installation
// and proves that they share one probe/backoff sequence instead of all doing a
// full CASE attempt on every tick.
func TestRouteWaitSingleFlightsAcrossNodes(t *testing.T) {
	routeLoud, retryInitial, retryMaximum, retryJitter :=
		routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter
	routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter =
		120*time.Millisecond, 15*time.Millisecond, 40*time.Millisecond, 0
	defer func() {
		routeWaitLoud, routeRetryInitial, routeRetryMaximum, routeRetryJitter =
			routeLoud, retryInitial, retryMaximum, retryJitter
	}()

	const workers = 12
	database := newBacking()
	devices := make([]Device, 0, workers)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	controller := &Controller{
		store: database, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: ctx,
		workers: make(map[uint64]context.CancelFunc), subscriptions: make(map[uint64]activeSubscription),
	}
	defer func() {
		cancel()
		controller.wg.Wait()
	}()
	routeErr := errors.New("send CASE Sigma1: write udp6: leannet: no IPv6 route (no router advertised)")
	var calls, active, maximum atomic.Int64
	for index := 0; index < workers; index++ {
		nodeID := uint64(0x5000 + index)
		devices = append(devices, addRecoveryMatterDevice(t, database, nodeID, 1, 7, "127.0.0.1:5540"))
		controller.wg.Add(1)
		go controller.maintainSubscription(ctx, nodeID, func(context.Context, uint64) error {
			calls.Add(1)
			current := active.Add(1)
			for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
			}
			time.Sleep(8 * time.Millisecond)
			active.Add(-1)
			return routeErr
		})
	}

	// With per-node 5-second polling this would be one call per worker at once.
	// The shared sequence admits only one and grows 15ms -> 30ms -> 40ms.
	time.Sleep(90 * time.Millisecond)
	if maximum.Load() != 1 {
		t.Fatalf("route probes in flight = %d, want exactly one", maximum.Load())
	}
	if got := calls.Load(); got > 4 {
		t.Fatalf("shared route wait performed %d probes in 90ms, want at most 4", got)
	}
	for _, device := range devices {
		current, err := database.Device(context.Background(), device.ID)
		if err != nil || !current.Available {
			t.Fatalf("quiet window changed %s availability: %#v, %v", device.ID, current, err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		allUnavailable := true
		for _, device := range devices {
			current, err := database.Device(context.Background(), device.ID)
			if err != nil || current.Available {
				allUnavailable = false
				break
			}
		}
		if allUnavailable {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not every node adopted the shared route error after the quiet window")
}

func TestRouteGateWakesEveryWorkerWhenProbeSucceeds(t *testing.T) {
	retryInitial, retryMaximum, retryJitter := routeRetryInitial, routeRetryMaximum, routeRetryJitter
	routeRetryInitial, routeRetryMaximum, routeRetryJitter = 15*time.Millisecond, 15*time.Millisecond, 0
	defer func() {
		routeRetryInitial, routeRetryMaximum, routeRetryJitter = retryInitial, retryMaximum, retryJitter
	}()

	controller := &Controller{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	firstCtx, firstToken, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || firstToken == 0 {
		t.Fatalf("initial route canary = token %d, error %v", firstToken, err)
	}
	_ = firstCtx
	routeErr := errors.New("send CASE Sigma1: write udp6: leannet: no IPv6 route (no router advertised)")
	controller.finishSubscriptionAttempt(firstCtx, firstToken, 0x6000, routeErr)

	const waiters = 10
	type result struct {
		token uint64
		err   error
	}
	results := make(chan result, waiters)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for index := 0; index < waiters; index++ {
		go func(nodeID uint64) {
			attemptCtx, token, attemptErr := controller.beginSubscriptionAttempt(ctx)
			if attemptErr == nil && token != 0 {
				// This is the sole retry probe. A successful Subscribe wakes every
				// waiter; they should return unrestricted (token zero).
				controller.subscriptionRouteReady(attemptCtx, nodeID)
			}
			results <- result{token: token, err: attemptErr}
		}(uint64(0x6100 + index))
	}

	probes := 0
	for index := 0; index < waiters; index++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("woken route waiter returned %v", got.err)
			}
			if got.token != 0 {
				probes++
			}
		case <-ctx.Done():
			t.Fatal("successful route probe did not wake every waiter")
		}
	}
	if probes != 1 {
		t.Fatalf("successful recovery used %d probes, want 1", probes)
	}
}

func TestRouteGateWaitHonorsCancellation(t *testing.T) {
	controller := &Controller{}
	attemptCtx, token, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || token == 0 {
		t.Fatalf("initial route canary = token %d, error %v", token, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = controller.beginSubscriptionAttempt(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled route wait = %v, want context.Canceled", err)
	}
	controller.finishSubscriptionAttempt(attemptCtx, token, 0, context.Canceled)
}

func TestLateUnrestrictedFailureCannotReopenRouteAfterNewerSuccess(t *testing.T) {
	controller := &Controller{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Establish the initial proof so subsequent attempts are unrestricted and
	// may overlap, as they do while all nodes reconnect after startup.
	initialCtx, initialToken, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || initialToken == 0 {
		t.Fatalf("initial route canary = token %d, error %v", initialToken, err)
	}
	controller.subscriptionRouteReady(initialCtx, 0x7000)

	olderCtx, olderToken, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || olderToken != 0 {
		t.Fatalf("older unrestricted attempt = token %d, error %v", olderToken, err)
	}
	newerCtx, newerToken, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || newerToken != 0 {
		t.Fatalf("newer unrestricted attempt = token %d, error %v", newerToken, err)
	}

	// B completes successfully after A began. A's still-later failure belongs
	// to the older proof epoch and must not undo B's positive route evidence.
	controller.subscriptionRouteReady(newerCtx, 0x7002)
	routeErr := errors.New("send CASE Sigma1: write udp6: leannet: no IPv6 route (no router advertised)")
	if started := controller.finishSubscriptionAttempt(olderCtx, olderToken, 0x7001, routeErr); !started.IsZero() {
		t.Fatalf("stale unrestricted failure opened route episode at %v", started)
	}

	controller.routeMu.Lock()
	known, recovering := controller.routeKnown, controller.routeRecovering
	controller.routeMu.Unlock()
	if !known || recovering {
		t.Fatalf("route after stale failure: known=%v recovering=%v", known, recovering)
	}
	_, nextToken, err := controller.beginSubscriptionAttempt(context.Background())
	if err != nil || nextToken != 0 {
		t.Fatalf("next attempt was gated after newer success: token=%d error=%v", nextToken, err)
	}
}
