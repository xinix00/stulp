package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// FlowStep points at a manifest-declared plugin Flow card. Args are the values
// selected by the user. Device arguments use {"$device":"DEVICE_ID"} so the
// app runtime can restore the real SDK device object before invoking code.
type FlowStep struct {
	AppID    string         `json:"appId"`
	CardID   string         `json:"cardId"`
	CardType string         `json:"cardType"`
	Args     map[string]any `json:"args,omitempty"`
	State    map[string]any `json:"state,omitempty"`
	Inverted bool           `json:"inverted,omitempty"`
}

// FlowNode places one plugin Flow card on the visual canvas. The card itself
// remains a FlowStep so the same SDK invocation path is used by basic and
// visual Flows.
type FlowNode struct {
	ID   string   `json:"id"`
	X    float64  `json:"x"`
	Y    float64  `json:"y"`
	Step FlowStep `json:"step"`
}

// FlowEdge directs successful execution from one card to the next. Conditions
// only traverse their outgoing edges when they evaluate to true.
type FlowEdge struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

type Flow struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	Nodes     []FlowNode `json:"nodes"`
	Edges     []FlowEdge `json:"edges"`
	LastRunAt string     `json:"lastRunAt,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	CreatedAt string     `json:"createdAt"`
	UpdatedAt string     `json:"updatedAt"`
	// Revision counts the stored versions of this flow. It is what an optimistic
	// update compares, because a timestamp is not a version: two writes in the
	// same clock tick share it.
	Revision uint64 `json:"revision"`
}

// FlowSummary is graph-free metadata for list views and bounded APIs. Reading
// it does not clone every card argument merely to count cards and connections.
type FlowSummary struct {
	ID        string
	Name      string
	Enabled   bool
	LastRunAt string
	LastError string
	CreatedAt string
	UpdatedAt string
	Revision  uint64
	NodeCount int
	EdgeCount int
}

// ErrFlowChanged means an optimistic update was based on an older revision.
var ErrFlowChanged = errors.New("flow changed since it was read")

// Runnable says whether this flow could ever do something: a card that can
// start it, a card that can act, and a path between them. It is a question, not
// a rule -- a flow is built one card at a time, exactly like on the canvas, and
// an incomplete flow is stored and shown rather than refused. The engine has
// nothing to start until the path exists.
func (flow Flow) Runnable() (bool, string) {
	kinds := make([]string, len(flow.Nodes))
	indices := make(map[string]int, len(flow.Nodes))
	queue := make([]int, 0, len(flow.Nodes))
	actions := 0
	for index, node := range flow.Nodes {
		kind := flowStepKind(node.Step)
		kinds[index] = kind
		if node.ID != "" {
			indices[node.ID] = index
		}
		switch kind {
		case "trigger":
			queue = append(queue, index)
		case "action":
			actions++
		}
	}
	if len(queue) == 0 {
		return false, "add an ALS/trigger card"
	}
	if actions == 0 {
		return false, "add a DAN/action card"
	}
	reachable := make([][]int, len(flow.Nodes))
	for _, edge := range flow.Edges {
		from, fromExists := indices[edge.From]
		to, toExists := indices[edge.To]
		if fromExists && toExists {
			reachable[from] = append(reachable[from], to)
		}
	}
	seen := make([]bool, len(flow.Nodes))
	for len(queue) > 0 {
		index := queue[0]
		queue = queue[1:]
		if seen[index] {
			continue
		}
		seen[index] = true
		if kinds[index] == "action" {
			return true, ""
		}
		queue = append(queue, reachable[index]...)
	}
	return false, "connect the ALS/trigger card to a DAN/action card"
}

// Summary is this flow without its graph: what a list view and a bounded API
// need. One place builds it, so a summary cannot say something different
// depending on who asked.
func (flow Flow) Summary() FlowSummary {
	return FlowSummary{
		ID: flow.ID, Name: flow.Name, Enabled: flow.Enabled,
		LastRunAt: flow.LastRunAt, LastError: flow.LastError,
		CreatedAt: flow.CreatedAt, UpdatedAt: flow.UpdatedAt, Revision: flow.Revision,
		NodeCount: len(flow.Nodes), EdgeCount: len(flow.Edges),
	}
}

// Steps lists every card in a flow, in node order.
func (flow Flow) Steps() []FlowStep {
	steps := make([]FlowStep, 0, len(flow.Nodes))
	for _, node := range flow.Nodes {
		steps = append(steps, node.Step)
	}
	return steps
}

func (flow *Flow) normalize() error {
	flow.Name = strings.TrimSpace(flow.Name)
	if flow.Name == "" {
		return errors.New("flow name is required")
	}
	if len(flow.Name) > 160 {
		return errors.New("flow name is too long")
	}
	return flow.validateGraph()
}

func validateFlowStep(step FlowStep, expected string) error {
	if step.AppID == "" || step.CardID == "" {
		return fmt.Errorf("%s appId and cardId are required", expected)
	}
	if step.CardType != expected && !(expected == "trigger" && step.CardType == "device-trigger") {
		return fmt.Errorf("%s card %q has invalid type %q", expected, step.CardID, step.CardType)
	}
	return nil
}

func (flow *Flow) validateGraph() error {
	if len(flow.Nodes) > 128 || len(flow.Edges) > 256 {
		return errors.New("a flow supports at most 128 cards and 256 connections")
	}
	nodes := make(map[string]FlowNode, len(flow.Nodes))
	for index := range flow.Nodes {
		node := &flow.Nodes[index]
		node.ID = strings.TrimSpace(node.ID)
		if node.ID == "" {
			node.ID = newID()
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("duplicate flow card id %q", node.ID)
		}
		if math.IsNaN(node.X) || math.IsInf(node.X, 0) || math.IsNaN(node.Y) || math.IsInf(node.Y, 0) ||
			math.Abs(node.X) > 1_000_000 || math.Abs(node.Y) > 1_000_000 {
			return fmt.Errorf("flow card %q has an invalid position", node.ID)
		}
		kind := flowStepKind(node.Step)
		if kind == "" {
			return fmt.Errorf("flow card %q has invalid type %q", node.ID, node.Step.CardType)
		}
		if err := validateFlowStep(node.Step, kind); err != nil {
			return err
		}
		nodes[node.ID] = *node
	}

	connections := make(map[string]struct{}, len(flow.Edges))
	adjacency := make(map[string][]string, len(flow.Nodes))
	for index := range flow.Edges {
		edge := &flow.Edges[index]
		edge.ID = strings.TrimSpace(edge.ID)
		if edge.ID == "" {
			edge.ID = newID()
		}
		from, fromExists := nodes[edge.From]
		to, toExists := nodes[edge.To]
		if !fromExists || !toExists {
			return fmt.Errorf("connection %q references a missing card", edge.ID)
		}
		if edge.From == edge.To {
			return fmt.Errorf("card %q cannot connect to itself", edge.From)
		}
		if flowStepKind(to.Step) == "trigger" {
			return errors.New("an ALS card cannot have an incoming connection")
		}
		key := edge.From + "\x00" + edge.To
		if _, exists := connections[key]; exists {
			return fmt.Errorf("duplicate connection from %q to %q", edge.From, edge.To)
		}
		connections[key] = struct{}{}
		adjacency[from.ID] = append(adjacency[from.ID], to.ID)
	}
	if graphHasCycle(nodes, adjacency) {
		return errors.New("flow connections may not contain a cycle")
	}
	return nil
}

func graphHasCycle(nodes map[string]FlowNode, adjacency map[string][]string) bool {
	state := make(map[string]uint8, len(nodes))
	var visit func(string) bool
	visit = func(id string) bool {
		if state[id] == 1 {
			return true
		}
		if state[id] == 2 {
			return false
		}
		state[id] = 1
		for _, next := range adjacency[id] {
			if visit(next) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for id := range nodes {
		if visit(id) {
			return true
		}
	}
	return false
}

func flowStepKind(step FlowStep) string {
	switch step.CardType {
	case "trigger", "device-trigger":
		return "trigger"
	case "condition":
		return "condition"
	case "action":
		return "action"
	default:
		return ""
	}
}

// cloneFlow separates every mutable container in a flow snapshot, so a caller
// can keep editing the flow it handed in (or got back) without reaching into
// the stored document. Args and State hold decoded JSON, which is exactly the
// set of shapes cloneJSONValue covers.
func cloneFlow(flow Flow) Flow {
	if flow.Nodes != nil {
		nodes := make([]FlowNode, len(flow.Nodes))
		copy(nodes, flow.Nodes)
		for index := range nodes {
			nodes[index].Step.Args = cloneJSONObject(nodes[index].Step.Args)
			nodes[index].Step.State = cloneJSONObject(nodes[index].Step.State)
		}
		flow.Nodes = nodes
	}
	if flow.Edges != nil {
		edges := make([]FlowEdge, len(flow.Edges))
		copy(edges, flow.Edges)
		flow.Edges = edges
	}
	return flow
}

func cloneJSONObject(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for name, value := range source {
		cloned[name] = cloneJSONValue(value)
	}
	return cloned
}

// cloneJSONValue copies the containers of a decoded JSON value. Anything else
// is a scalar (string, bool, number, nil) and is immutable, so sharing it is
// safe. A value that is neither -- an app handing in its own map type, say --
// is stored as-is; it went through JSON on its way in or out anyway.
func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONObject(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, child := range typed {
			cloned[index] = cloneJSONValue(child)
		}
		return cloned
	default:
		return value
	}
}

func (s *Store) CreateFlow(ctx context.Context, flow Flow) (Flow, error) {
	flow = cloneFlow(flow)
	if err := flow.normalize(); err != nil {
		return Flow{}, err
	}
	if flow.ID == "" {
		flow.ID = newID()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	flow.CreatedAt, flow.UpdatedAt = now, now

	s.mu.Lock()
	for _, existing := range s.doc.Flows {
		if existing.ID == flow.ID {
			s.mu.Unlock()
			return Flow{}, fmt.Errorf("flow %q already exists", flow.ID)
		}
	}
	flows := make([]Flow, len(s.doc.Flows)+1)
	copy(flows, s.doc.Flows)
	flows[len(s.doc.Flows)] = cloneFlow(flow)
	err := s.commitFlowsLocked(flows)
	s.mu.Unlock()
	if err != nil {
		return Flow{}, fmt.Errorf("create flow: %w", err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.create", ID: flow.ID, Data: cloneFlow(flow)})
	return flow, nil
}

func (s *Store) UpdateFlow(ctx context.Context, flow Flow) (Flow, error) {
	return s.updateFlow(ctx, flow, nil)
}

// UpdateFlowIfUnchanged updates a flow only when expectedRevision is still the
// stored one. The comparison and the write share the same store lock.
func (s *Store) UpdateFlowIfUnchanged(ctx context.Context, flow Flow, expectedRevision uint64) (Flow, error) {
	return s.updateFlow(ctx, flow, &expectedRevision)
}

func (s *Store) updateFlow(ctx context.Context, flow Flow, expectedRevision *uint64) (Flow, error) {
	flow = cloneFlow(flow)
	if flow.ID == "" {
		return Flow{}, errors.New("flow id is required")
	}
	if err := flow.normalize(); err != nil {
		return Flow{}, err
	}
	s.mu.Lock()
	index := indexOfFlow(s.doc.Flows, flow.ID)
	if index < 0 {
		s.mu.Unlock()
		return Flow{}, fmt.Errorf("flow %q does not exist", flow.ID)
	}
	// A rewrite keeps the flow's own history: when it was made, and what
	// happened the last time it ran.
	existing := s.doc.Flows[index]
	if expectedRevision != nil && existing.Revision != *expectedRevision {
		s.mu.Unlock()
		return Flow{}, fmt.Errorf("flow %q: %w", flow.ID, ErrFlowChanged)
	}
	flow.CreatedAt = existing.CreatedAt
	flow.LastRunAt, flow.LastError = existing.LastRunAt, existing.LastError
	flow.UpdatedAt, flow.Revision = nowRFC3339(), existing.Revision+1
	flows := append([]Flow(nil), s.doc.Flows...)
	flows[index] = cloneFlow(flow)
	err := s.commitFlowsLocked(flows)
	s.mu.Unlock()
	if err != nil {
		return Flow{}, fmt.Errorf("update flow %q: %w", flow.ID, err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: flow.ID, Data: cloneFlow(flow)})
	return flow, nil
}

func (s *Store) Flow(_ context.Context, id string) (Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if index := indexOfFlow(s.doc.Flows, id); index >= 0 {
		return cloneFlow(s.doc.Flows[index]), nil
	}
	return Flow{}, fmt.Errorf("flow %q does not exist", id)
}

func (s *Store) Flows(_ context.Context) ([]Flow, error) {
	s.mu.RLock()
	var flows []Flow
	if s.doc.Flows != nil {
		flows = make([]Flow, len(s.doc.Flows))
		for index := range s.doc.Flows {
			flows[index] = cloneFlow(s.doc.Flows[index])
		}
	}
	s.mu.RUnlock()
	sort.SliceStable(flows, func(left, right int) bool {
		if flows[left].CreatedAt != flows[right].CreatedAt {
			return flows[left].CreatedAt < flows[right].CreatedAt
		}
		return flows[left].ID < flows[right].ID
	})
	return flows, nil
}

func (s *Store) FlowSummaries(_ context.Context) ([]FlowSummary, error) {
	s.mu.RLock()
	summaries := make([]FlowSummary, len(s.doc.Flows))
	for index, flow := range s.doc.Flows {
		summaries[index] = flow.Summary()
	}
	s.mu.RUnlock()
	sort.SliceStable(summaries, func(left, right int) bool {
		if summaries[left].CreatedAt != summaries[right].CreatedAt {
			return summaries[left].CreatedAt < summaries[right].CreatedAt
		}
		return summaries[left].ID < summaries[right].ID
	})
	return summaries, nil
}

func (s *Store) SetFlowEnabled(ctx context.Context, id string, enabled bool) error {
	if err := s.mutateFlow(ctx, id, func(flow *Flow) { flow.Enabled = enabled }); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: id, Data: map[string]any{"enabled": enabled}})
	return nil
}

// DisableFlow turns a flow off and records why, so Manage explains a Flow that
// can no longer run instead of leaving the user to discover it.
func (s *Store) DisableFlow(ctx context.Context, id, reason string) error {
	if err := s.mutateFlow(ctx, id, func(flow *Flow) {
		flow.Enabled, flow.LastError = false, reason
	}); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: id,
		Data: map[string]any{"enabled": false, "lastError": reason}})
	return nil
}

func (s *Store) SetFlowResult(ctx context.Context, id string, runAt time.Time, runError string) error {
	if err := s.mutateFlow(ctx, id, func(flow *Flow) {
		flow.LastRunAt = runAt.UTC().Format(time.RFC3339Nano)
		flow.LastError = runError
	}); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.run", ID: id, Data: map[string]any{"error": runError}})
	return nil
}

func (s *Store) DeleteFlow(ctx context.Context, id string) error {
	s.mu.Lock()
	before := len(s.doc.Flows)
	flows := removeWhere(s.doc.Flows, func(flow Flow) bool { return flow.ID == id })
	if len(flows) == before {
		s.mu.Unlock()
		return fmt.Errorf("flow %q does not exist", id)
	}
	err := s.commitFlowsLocked(flows)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete flow %q: %w", id, err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.delete", ID: id})
	return nil
}

func (s *Store) mutateFlow(ctx context.Context, id string, change func(*Flow)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := indexOfFlow(s.doc.Flows, id)
	if index < 0 {
		return fmt.Errorf("flow %q does not exist", id)
	}
	flows := append([]Flow(nil), s.doc.Flows...)
	flows[index] = cloneFlow(flows[index])
	change(&flows[index])
	flows[index].UpdatedAt, flows[index].Revision = nowRFC3339(), flows[index].Revision+1
	if err := s.commitFlowsLocked(flows); err != nil {
		return fmt.Errorf("update flow %q: %w", id, err)
	}
	return nil
}

// commitFlowsLocked persists a copy-on-write document and only makes it
// visible after the write succeeds. Besides avoiding rollback bookkeeping,
// this keeps a rejected revision completely out of readers' reach. The
// notification slice is copied too because saveDocument sorts and bounds it.
// Callers hold s.mu for writing.
func (s *Store) commitFlowsLocked(flows []Flow) error {
	candidate := *s.doc
	candidate.Flows = flows
	if s.doc.Notifications != nil {
		candidate.Notifications = append([]Notification(nil), s.doc.Notifications...)
	}
	if s.path != InMemoryPath {
		if err := saveDocument(s.path, &candidate); err != nil {
			return err
		}
	}
	s.doc = &candidate
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func indexOfFlow(flows []Flow, id string) int {
	for index, flow := range flows {
		if flow.ID == id {
			return index
		}
	}
	return -1
}

// FlowsUsingApp reports the flows that stop working once an app is gone: the
// ones holding a card the app owns, and the ones whose built-in cards point at
// a device the app supplied.
func (s *Store) FlowsUsingApp(ctx context.Context, appID string, deviceIDs []string) ([]Flow, error) {
	flows, err := s.Flows(ctx)
	if err != nil {
		return nil, err
	}
	removed := make(map[string]struct{}, len(deviceIDs))
	for _, id := range deviceIDs {
		removed[id] = struct{}{}
	}
	affected := make([]Flow, 0)
	for _, flow := range flows {
		for _, step := range flow.Steps() {
			if step.AppID != appID &&
				!referencesRemovedDevice(step.Args, removed) &&
				!referencesRemovedDevice(step.State, removed) {
				continue
			}
			affected = append(affected, flow)
			break
		}
	}
	return affected, nil
}

// referencesRemovedDevice walks an argument tree looking for the
// {"$device": ID} references the Flow editor writes. Arguments nest, so the
// search cannot stop at the top level.
func referencesRemovedDevice(value any, removed map[string]struct{}) bool {
	switch typed := value.(type) {
	case map[string]any:
		if id, _ := typed["$device"].(string); id != "" {
			if _, gone := removed[id]; gone {
				return true
			}
		}
		for _, child := range typed {
			if referencesRemovedDevice(child, removed) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if referencesRemovedDevice(child, removed) {
				return true
			}
		}
	}
	return false
}

// DisableFlowsFor turns off the flows an uninstalled app leaves behind and
// records why. Deleting them would throw away work the user can recover by
// reinstalling the app; leaving them enabled would fail quietly at the next
// trigger. Disabled and explained is the honest middle.
func (s *Store) DisableFlowsFor(ctx context.Context, app App, deviceIDs []string) ([]Flow, error) {
	affected, err := s.FlowsUsingApp(ctx, app.ID, deviceIDs)
	if err != nil {
		return nil, err
	}
	name := app.ID
	if title, _ := app.Manifest["name"].(map[string]any); title != nil {
		for _, language := range []string{"nl", "en"} {
			if text, _ := title[language].(string); text != "" {
				name = text
				break
			}
		}
	}
	reason := "Uitgeschakeld: de app " + name + " is verwijderd, dus deze Flow mist kaarten of apparaten."
	disabled := make([]Flow, 0, len(affected))
	for _, flow := range affected {
		if err := s.DisableFlow(ctx, flow.ID, reason); err != nil {
			return disabled, err
		}
		flow.Enabled, flow.LastError = false, reason
		disabled = append(disabled, flow)
	}
	return disabled, nil
}
