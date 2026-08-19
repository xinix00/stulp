package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFlowSnapshotsDoNotShareMutableState(t *testing.T) {
	t.Parallel()
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	events, cancel := database.Subscribe(1)
	defer cancel()

	input := mutableSnapshotFlow("Snapshot")
	created, err := database.CreateFlow(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	mutateFlowContainers(&input, "input")
	mutateFlowContainers(&created, "created")
	assertStoredFlowContainers(t, database, created.ID)
	published, ok := (<-events).Data.(Flow)
	if !ok {
		t.Fatal("flow.create event does not contain a Flow snapshot")
	}
	assertFlowContainers(t, published)

	byID, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	mutateFlowContainers(&byID, "by-id")
	assertStoredFlowContainers(t, database, created.ID)

	listed, err := database.Flows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("Flows returned %d flows, want 1", len(listed))
	}
	mutateFlowContainers(&listed[0], "listed")
	assertStoredFlowContainers(t, database, created.ID)

	summaries, err := database.FlowSummaries(ctx)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("FlowSummaries returned %#v, error %v", summaries, err)
	}
	if summaries[0].NodeCount != 2 || summaries[0].EdgeCount != 1 || summaries[0].Name != "Snapshot" {
		t.Fatalf("Flow summary is incomplete: %#v", summaries[0])
	}
	summaries[0].Name = "caller-owned"
	if stored, err := database.Flow(ctx, created.ID); err != nil || stored.Name != "Snapshot" {
		t.Fatalf("Flow summary aliases the store: %#v, error %v", stored, err)
	}
}

func TestUpdateFlowIfUnchangedRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	created, err := database.CreateFlow(ctx, mutableSnapshotFlow("Before"))
	if err != nil {
		t.Fatal(err)
	}
	runAt := time.Date(2026, time.August, 19, 10, 30, 0, 0, time.UTC)
	if err := database.SetFlowResult(ctx, created.ID, runAt, "previous failure"); err != nil {
		t.Fatal(err)
	}
	current, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := database.Subscribe(1)
	defer cancel()

	edit := current
	edit.Name = "Fresh edit"
	edit.CreatedAt = "forged"
	edit.LastRunAt = "forged"
	edit.LastError = "forged"
	updated, err := database.UpdateFlowIfUnchanged(ctx, edit, current.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CreatedAt != current.CreatedAt || updated.LastRunAt != current.LastRunAt || updated.LastError != current.LastError {
		t.Fatalf("conditional update lost flow history: before=%#v after=%#v", current, updated)
	}
	if updated.UpdatedAt == current.UpdatedAt {
		t.Fatal("conditional update did not advance the revision")
	}
	mutateFlowContainers(&updated, "updated")
	assertStoredFlowContainers(t, database, created.ID)
	published, ok := (<-events).Data.(Flow)
	if !ok {
		t.Fatal("flow.update event does not contain a Flow snapshot")
	}
	if published.Name != "Fresh edit" {
		t.Fatalf("published name = %q, want Fresh edit", published.Name)
	}
	assertFlowContainers(t, published)

	stale := current
	stale.Name = "Stale edit"
	if _, err := database.UpdateFlowIfUnchanged(ctx, stale, current.Revision); !errors.Is(err, ErrFlowChanged) {
		t.Fatalf("stale update error = %v, want ErrFlowChanged", err)
	}
	stored, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Fresh edit" || stored.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("stale update changed stored flow: %#v", stored)
	}
}

func TestUpdateFlowIfUnchangedAllowsOneConcurrentWriter(t *testing.T) {
	t.Parallel()
	database, err := Open(InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	created, err := database.CreateFlow(ctx, mutableSnapshotFlow("Before"))
	if err != nil {
		t.Fatal(err)
	}
	left, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	left.Name, right.Name = "Left", "Right"

	type result struct {
		name string
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	update := func(flow Flow) {
		<-start
		_, err := database.UpdateFlowIfUnchanged(ctx, flow, created.Revision)
		results <- result{name: flow.Name, err: err}
	}
	go update(left)
	go update(right)
	close(start)

	winner, successes, conflicts := "", 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			winner = result.name
			successes++
		case errors.Is(result.err, ErrFlowChanged):
			conflicts++
		default:
			t.Fatalf("conditional update returned unexpected error: %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent updates: successes=%d conflicts=%d, want 1 each", successes, conflicts)
	}
	stored, err := database.Flow(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != winner {
		t.Fatalf("stored name = %q, successful writer = %q", stored.Name, winner)
	}
}

func mutableSnapshotFlow(name string) Flow {
	return Flow{
		Name: name, Enabled: true,
		Nodes: []FlowNode{
			{
				ID: "trigger", X: 20, Y: 30,
				Step: FlowStep{
					AppID: "com.example", CardID: "changed", CardType: "trigger",
					// These are the shapes a flow argument can have: everything
					// reaches the store as decoded JSON, from the web API, from MCP
					// or over an app's own JSON protocol.
					Args: map[string]any{
						"device": map[string]any{"$device": "sensor"},
						"values": []any{"first", map[string]any{"nested": "kept"}},
					},
					State: map[string]any{"tokens": []any{"kept", "second"}},
				},
			},
			{
				ID: "action", X: 300, Y: 30,
				Step: FlowStep{AppID: "com.example", CardID: "notify", CardType: "action"},
			},
		},
		Edges: []FlowEdge{{ID: "edge", From: "trigger", To: "action"}},
	}
}

func mutateFlowContainers(flow *Flow, marker string) {
	flow.Nodes[0].Step.Args["device"].(map[string]any)["$device"] = marker
	flow.Nodes[0].Step.Args["values"].([]any)[1].(map[string]any)["nested"] = marker
	flow.Nodes[0].Step.State["tokens"].([]any)[0] = marker
	flow.Nodes[0].ID = marker
	flow.Edges[0].To = marker
}

func assertStoredFlowContainers(t *testing.T, database *Store, id string) {
	t.Helper()
	flow, err := database.Flow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertFlowContainers(t, flow)
}

func assertFlowContainers(t *testing.T, flow Flow) {
	t.Helper()
	if flow.Nodes[0].ID != "trigger" || flow.Edges[0].To != "action" {
		t.Fatalf("flow snapshot shares a caller slice: nodes=%#v edges=%#v", flow.Nodes, flow.Edges)
	}
	args, state := flow.Nodes[0].Step.Args, flow.Nodes[0].Step.State
	if got := args["device"].(map[string]any)["$device"]; got != "sensor" {
		t.Fatalf("flow snapshot nested map value = %v, want sensor", got)
	}
	if got := args["values"].([]any)[1].(map[string]any)["nested"]; got != "kept" {
		t.Fatalf("flow snapshot map in slice value = %v, want kept", got)
	}
	if got := state["tokens"].([]any)[0]; got != "kept" {
		t.Fatalf("flow snapshot slice value = %v, want kept", got)
	}
}
