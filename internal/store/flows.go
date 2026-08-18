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
	if len(flow.Nodes) == 0 {
		return errors.New("a flow needs at least one ALS card")
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
	triggerCount, actionCount := 0, 0
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
		if kind == "trigger" {
			triggerCount++
		}
		if kind == "action" {
			actionCount++
		}
		nodes[node.ID] = *node
	}
	if triggerCount == 0 {
		return errors.New("a flow needs at least one ALS card")
	}
	if actionCount == 0 {
		return errors.New("a flow needs at least one DAN card")
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

func (s *Store) CreateFlow(_ context.Context, flow Flow) (Flow, error) {
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
	s.doc.Flows = append(s.doc.Flows, flow)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Flow{}, fmt.Errorf("create flow: %w", err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.create", ID: flow.ID, Data: flow})
	return flow, nil
}

func (s *Store) UpdateFlow(_ context.Context, flow Flow) (Flow, error) {
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
	flow.CreatedAt = existing.CreatedAt
	flow.LastRunAt, flow.LastError = existing.LastRunAt, existing.LastError
	flow.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.doc.Flows[index] = flow
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Flow{}, fmt.Errorf("update flow %q: %w", flow.ID, err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: flow.ID, Data: flow})
	return flow, nil
}

func (s *Store) Flow(_ context.Context, id string) (Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if index := indexOfFlow(s.doc.Flows, id); index >= 0 {
		return s.doc.Flows[index], nil
	}
	return Flow{}, fmt.Errorf("flow %q does not exist", id)
}

func (s *Store) Flows(_ context.Context) ([]Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	flows := append([]Flow(nil), s.doc.Flows...)
	sort.SliceStable(flows, func(left, right int) bool {
		if flows[left].CreatedAt != flows[right].CreatedAt {
			return flows[left].CreatedAt < flows[right].CreatedAt
		}
		return flows[left].ID < flows[right].ID
	})
	return flows, nil
}

func (s *Store) SetFlowEnabled(_ context.Context, id string, enabled bool) error {
	if err := s.mutateFlow(id, func(flow *Flow) { flow.Enabled = enabled }); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: id, Data: map[string]any{"enabled": enabled}})
	return nil
}

// DisableFlow turns a flow off and records why, so Manage explains a Flow that
// can no longer run instead of leaving the user to discover it.
func (s *Store) DisableFlow(_ context.Context, id, reason string) error {
	if err := s.mutateFlow(id, func(flow *Flow) {
		flow.Enabled, flow.LastError = false, reason
	}); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.update", ID: id,
		Data: map[string]any{"enabled": false, "lastError": reason}})
	return nil
}

func (s *Store) SetFlowResult(_ context.Context, id string, runAt time.Time, runError string) error {
	if err := s.mutateFlow(id, func(flow *Flow) {
		flow.LastRunAt = runAt.UTC().Format(time.RFC3339Nano)
		flow.LastError = runError
	}); err != nil {
		return err
	}
	s.publish(Event{Manager: "flow", Type: "flow.run", ID: id, Data: map[string]any{"error": runError}})
	return nil
}

func (s *Store) DeleteFlow(_ context.Context, id string) error {
	s.mu.Lock()
	before := len(s.doc.Flows)
	s.doc.Flows = removeWhere(s.doc.Flows, func(flow Flow) bool { return flow.ID == id })
	if len(s.doc.Flows) == before {
		s.mu.Unlock()
		return fmt.Errorf("flow %q does not exist", id)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete flow %q: %w", id, err)
	}
	s.publish(Event{Manager: "flow", Type: "flow.delete", ID: id})
	return nil
}

func (s *Store) mutateFlow(id string, change func(*Flow)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := indexOfFlow(s.doc.Flows, id)
	if index < 0 {
		return fmt.Errorf("flow %q does not exist", id)
	}
	change(&s.doc.Flows[index])
	s.doc.Flows[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("update flow %q: %w", id, err)
	}
	return nil
}

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
