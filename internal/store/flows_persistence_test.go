package store

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"
)

func TestFlowMutationsArePublishedOnlyAfterPersistence(t *testing.T) {
	backend := &flowFailingFiles{documents: make(map[string][]byte)}
	previousFiles := files
	files = backend
	t.Cleanup(func() { files = previousFiles })

	database, err := Open("/virtual/flow-transaction-test.json")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	events, cancel := database.Subscribe(8)
	defer cancel()

	writeFailure := errors.New("injected persistence failure")
	backend.writeErr = writeFailure
	rejected := mutableSnapshotFlow("Rejected create")
	rejected.ID = "rejected"
	if _, err := database.CreateFlow(ctx, rejected); !errors.Is(err, writeFailure) {
		t.Fatalf("CreateFlow error = %v, want injected persistence failure", err)
	}
	if flows, err := database.Flows(ctx); err != nil || len(flows) != 0 {
		t.Fatalf("failed create left flows %#v (error %v)", flows, err)
	}
	assertNoFlowEvent(t, events)

	backend.writeErr = nil
	created, err := database.CreateFlow(ctx, mutableSnapshotFlow("Committed"))
	if err != nil {
		t.Fatal(err)
	}
	assertFlowEvent(t, events, "flow.create", created.ID)
	runAt := time.Date(2026, time.August, 19, 12, 30, 0, 0, time.UTC)
	if err := database.SetFlowResult(ctx, created.ID, runAt, "remembered failure"); err != nil {
		t.Fatal(err)
	}
	assertFlowEvent(t, events, "flow.run", created.ID)

	before := mustStoredFlow(t, database, created.ID)
	persistedBefore := append([]byte(nil), backend.documents[database.Path()]...)
	backend.writeErr = writeFailure
	edit := cloneFlow(before)
	edit.Name = "Rejected update"
	edit.Nodes[0].Step.Args["device"].(map[string]any)["$device"] = "rejected-device"
	if _, err := database.UpdateFlow(ctx, edit); !errors.Is(err, writeFailure) {
		t.Fatalf("UpdateFlow error = %v, want injected persistence failure", err)
	}
	assertFlowMutationRejected(t, database, backend, events, before, persistedBefore)

	// A failed optimistic write must not consume the revision. The same
	// expected revision remains valid once persistence works again.
	conditional := cloneFlow(before)
	conditional.Name = "Rejected conditional update"
	if _, err := database.UpdateFlowIfUnchanged(ctx, conditional, before.Revision); !errors.Is(err, writeFailure) {
		t.Fatalf("UpdateFlowIfUnchanged error = %v, want injected persistence failure", err)
	}
	assertFlowMutationRejected(t, database, backend, events, before, persistedBefore)
	backend.writeErr = nil
	conditional.Name = "Committed conditional update"
	committed, err := database.UpdateFlowIfUnchanged(ctx, conditional, before.Revision)
	if err != nil {
		t.Fatalf("retry conditional update: %v", err)
	}
	assertFlowEvent(t, events, "flow.update", created.ID)
	before = committed

	persistedBefore = append([]byte(nil), backend.documents[database.Path()]...)
	backend.writeErr = writeFailure
	newRunAt := runAt.Add(time.Hour)
	if err := database.SetFlowResult(ctx, created.ID, newRunAt, "rejected history"); !errors.Is(err, writeFailure) {
		t.Fatalf("SetFlowResult error = %v, want injected persistence failure", err)
	}
	assertFlowMutationRejected(t, database, backend, events, before, persistedBefore)

	if err := database.DeleteFlow(ctx, created.ID); !errors.Is(err, writeFailure) {
		t.Fatalf("DeleteFlow error = %v, want injected persistence failure", err)
	}
	assertFlowMutationRejected(t, database, backend, events, before, persistedBefore)
}

func mustStoredFlow(t *testing.T, database *Store, id string) Flow {
	t.Helper()
	flow, err := database.Flow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

func assertFlowMutationRejected(
	t *testing.T,
	database *Store,
	backend *flowFailingFiles,
	events <-chan Event,
	want Flow,
	wantDocument []byte,
) {
	t.Helper()
	got := mustStoredFlow(t, database, want.ID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("failed persistence changed the in-memory flow:\n got  %#v\n want %#v", got, want)
	}
	if got.UpdatedAt != want.UpdatedAt || got.LastRunAt != want.LastRunAt || got.LastError != want.LastError {
		t.Fatalf("failed persistence changed flow history: got %#v, want %#v", got, want)
	}
	if persisted := backend.documents[database.Path()]; !bytes.Equal(persisted, wantDocument) {
		t.Fatal("failed persistence changed the stored document")
	}
	assertNoFlowEvent(t, events)
}

func assertFlowEvent(t *testing.T, events <-chan Event, eventType, id string) {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != eventType || event.ID != id {
			t.Fatalf("event = %#v, want type %q id %q", event, eventType, id)
		}
	default:
		t.Fatalf("missing %s event for %s", eventType, id)
	}
}

func assertNoFlowEvent(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("failed persistence published event %#v", event)
	default:
	}
}

type flowFailingFiles struct {
	documents map[string][]byte
	writeErr  error
}

func (f *flowFailingFiles) ReadFile(path string) ([]byte, error) {
	document, exists := f.documents[path]
	if !exists {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), document...), nil
}

func (f *flowFailingFiles) WriteFile(path string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.documents[path] = append([]byte(nil), data...)
	return nil
}

func TestFlowRunnableReportsWhatIsMissing(t *testing.T) {
	t.Parallel()
	step := func(cardType string) FlowStep {
		return FlowStep{AppID: "com.example", CardID: "card", CardType: cardType}
	}
	tests := []struct {
		name     string
		flow     Flow
		runnable bool
		missing  string
	}{
		{"empty", Flow{}, false, "add an ALS/trigger card"},
		{"trigger only", Flow{Nodes: []FlowNode{{ID: "a", Step: step("trigger")}}}, false, "add a DAN/action card"},
		{
			// Both cards are there but nothing joins them, and neither has an id
			// yet. Two empty ids must not look like one connected card.
			"unconnected without ids",
			Flow{Nodes: []FlowNode{{Step: step("trigger")}, {Step: step("action")}}},
			false, "connect the ALS/trigger card to a DAN/action card",
		},
		{
			"connected through a condition",
			Flow{
				Nodes: []FlowNode{{ID: "a", Step: step("trigger")}, {ID: "b", Step: step("condition")}, {ID: "c", Step: step("action")}},
				Edges: []FlowEdge{{From: "a", To: "b"}, {From: "b", To: "c"}},
			},
			true, "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runnable, missing := test.flow.Runnable()
			if runnable != test.runnable || missing != test.missing {
				t.Fatalf("Runnable() = (%t, %q), want (%t, %q)", runnable, missing, test.runnable, test.missing)
			}
		})
	}
}
