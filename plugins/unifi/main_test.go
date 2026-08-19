package main

import (
	"errors"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// fakeDevice is een apparaat dat vertelt of zijn stand op te halen viel.
type fakeDevice struct {
	err   error
	tries int
}

func (f *fakeDevice) apply(protect.DeviceMessage) {}

func (f *fakeDevice) refresh() error {
	f.tries++
	return f.err
}

// Deze app is gebeurtenis-gestuurd, dus een mislukte stand komt niet vanzelf
// terug: er moet een volgende ronde klaargezet worden, met een oplopende pauze
// die terugvalt zodra het weer lukt.
func TestFailedRefreshAsksForAnotherRound(t *testing.T) {
	unavailable := &fakeDevice{err: errors.New("context deadline exceeded")}
	console := &app{
		devices: map[string]handler{"camera": unavailable},
		// refreshSoon zet geen ronde klaar zonder console; welke het is doet hier
		// niet toe, want fakeDevice praat er niet mee.
		client: &protect.Client{},
	}

	// Elke ronde die niks oplevert verdubbelt de pauze, tot het plafond.
	want := []time.Duration{settleTime, 2 * settleTime, 4 * settleTime}
	for round, expected := range want {
		console.refreshAll()
		if console.retry == nil || console.retryWait != expected {
			t.Fatalf("ronde %d: pauze = %s, wilde %s (timer=%v)", round+1, console.retryWait, expected, console.retry != nil)
		}
		// Wat de timer zelf doet als hij afgaat, zodat de volgende ronde erin kan.
		console.retry.Stop()
		console.retry = nil
	}
	if unavailable.tries != len(want) {
		t.Fatalf("apparaat werd %d keer bevraagd, wilde %d", unavailable.tries, len(want))
	}

	// En zodra het lukt is er niets meer klaar te zetten, met een pauze die weer
	// vanaf het begin begint.
	unavailable.err = nil
	console.refreshAll()
	if console.retry != nil || console.retryWait != 0 {
		t.Fatalf("na een geslaagde ronde: pauze = %s, timer = %v", console.retryWait, console.retry != nil)
	}
	console.refreshAll()
	if console.retryWait != 0 {
		t.Fatalf("pauze bleef staan op %s", console.retryWait)
	}
}

// Zonder console valt er niks op te halen: dan hoort er geen ronde klaar te
// staan, want OnConnect doet er zelf een zodra er weer een verbinding is.
func TestNoConsoleSchedulesNothing(t *testing.T) {
	console := &app{devices: map[string]handler{"camera": &fakeDevice{err: errors.New("er is nog geen console ingesteld")}}}
	console.refreshAll()
	if console.retry != nil {
		t.Fatal("er staat een ronde klaar zonder console")
	}
}

// stop laat geen ronde achter die bij de verbinding hoorde die net wegging.
func TestStopCancelsTheWaitingRound(t *testing.T) {
	console := &app{
		devices: map[string]handler{"camera": &fakeDevice{err: errors.New("weg")}},
		client:  &protect.Client{},
	}
	console.refreshAll()
	if console.retry == nil {
		t.Fatal("geen ronde klaargezet om te stoppen")
	}
	console.stop()
	if console.retry != nil || console.retryWait != 0 {
		t.Fatalf("stop liet een ronde staan: timer=%v pauze=%s", console.retry != nil, console.retryWait)
	}
}
