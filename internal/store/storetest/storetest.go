// Package storetest builds store values in the shape tests read best.
package storetest

import (
	"fmt"

	"github.com/xinix00/stulp/internal/store"
)

// LinearFlow chains a trigger, its conditions and its actions into the node
// graph a Flow is stored as. A test that means "this card, then that one" says
// so here instead of spelling out node and edge literals, which is the part
// that would otherwise be copied into every Flow fixture.
func LinearFlow(name string, enabled bool, trigger store.FlowStep, conditions, actions []store.FlowStep) store.Flow {
	steps := make([]store.FlowStep, 0, 1+len(conditions)+len(actions))
	if trigger.AppID != "" {
		steps = append(steps, trigger)
	}
	steps = append(steps, conditions...)
	steps = append(steps, actions...)

	flow := store.Flow{
		Name: name, Enabled: enabled,
		Nodes: make([]store.FlowNode, 0, len(steps)),
		Edges: make([]store.FlowEdge, 0, max(0, len(steps)-1)),
	}
	for index, step := range steps {
		id := fmt.Sprintf("node-%d", index)
		flow.Nodes = append(flow.Nodes, store.FlowNode{ID: id, X: 80 + float64(index*400), Y: 120, Step: step})
		if index > 0 {
			flow.Edges = append(flow.Edges, store.FlowEdge{
				ID: fmt.Sprintf("edge-%d", index-1), From: flow.Nodes[index-1].ID, To: id,
			})
		}
	}
	return flow
}
