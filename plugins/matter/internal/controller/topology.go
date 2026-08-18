package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// NodeStatus is one commissioned accessory and how Stulp is currently talking
// to it. A node is one physical device; its endpoints become separate Stulp
// devices, which is why Devices is a list.
type NodeStatus struct {
	NodeID  string `json:"nodeId"`
	Address string `json:"address,omitempty"`
	// SessionOpen means an encrypted CASE session is established right now.
	// Subscribed means that session also carries a live attribute
	// subscription, which is what makes sensor values arrive unasked.
	SessionOpen bool `json:"sessionOpen"`
	Subscribed  bool `json:"subscribed"`
	// Credentialed reports whether Stulp still holds this node's operational
	// certificate. Without it no session can ever be established, which is a
	// different problem from a node that is merely unreachable.
	Credentialed bool         `json:"credentialed"`
	Devices      []NodeDevice `json:"devices"`
}

// NodeDevice is one endpoint of a node as Stulp presents it.
type NodeDevice struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Endpoint           string `json:"endpoint,omitempty"`
	Available          bool   `json:"available"`
	UnavailableMessage string `json:"unavailableMessage,omitempty"`
}

// Fabric identifies the local Matter fabric these nodes belong to.
type Fabric struct {
	FabricID     string `json:"fabricId"`
	ControllerID string `json:"controllerId"`
	Nodes        int    `json:"nodes"`
	// Unplaceable names devices that carry no usable node identity. They stay
	// visible in Manage; naming them here keeps the map from quietly being
	// smaller than the home.
	Unplaceable []string `json:"unplaceable,omitempty"`
}

// Topology reports the fabric and every node in it, grouped from the devices
// the store holds. Session and subscription state is read from the live
// controller, so an unreachable node is visible as such instead of merely
// stale.
func (c *Controller) Topology(ctx context.Context) (Fabric, []NodeStatus, error) {
	devices, err := c.store.Devices(ctx)
	if err != nil {
		return Fabric{}, nil, err
	}
	byNode := make(map[uint64]*NodeStatus)
	order := make([]uint64, 0)
	unplaceable := make([]string, 0)
	for _, device := range devices {
		// The map needs only the node identity, deliberately less than a
		// connection needs. A node whose credential is missing is exactly the
		// one worth seeing, so it must not be filtered out by the stricter
		// check that establishing a session performs.
		nodeText, _ := device.Store["matter.nodeId"].(string)
		nodeID, parseErr := strconv.ParseUint(nodeText, 16, 64)
		if parseErr != nil || nodeID == 0 {
			unplaceable = append(unplaceable, device.Name)
			continue
		}
		status := byNode[nodeID]
		if status == nil {
			address, _ := device.Store["matter.address"].(string)
			credential, _ := device.Store["matter.noc"].(string)
			status = &NodeStatus{
				NodeID: fmt.Sprintf("%016X", nodeID), Address: address,
				Credentialed: credential != "",
			}
			byNode[nodeID] = status
			order = append(order, nodeID)
		}
		status.Devices = append(status.Devices, NodeDevice{
			ID: device.ID, Name: device.Name,
			Endpoint:  fmt.Sprint(device.Store["matter.endpoint"]),
			Available: device.Available, UnavailableMessage: device.Message,
		})
	}

	for nodeID, status := range byNode {
		status.SessionOpen = c.lookupSession(nodeID) != nil
	}
	c.subMu.RLock()
	for nodeID, status := range byNode {
		_, subscribed := c.subscriptions[nodeID]
		status.Subscribed = subscribed
	}
	c.subMu.RUnlock()

	sort.Slice(order, func(left, right int) bool { return order[left] < order[right] })
	nodes := make([]NodeStatus, 0, len(order))
	for _, nodeID := range order {
		nodes = append(nodes, *byNode[nodeID])
	}
	return Fabric{
		FabricID:     fmt.Sprintf("%016X", c.fabric.ID),
		ControllerID: fmt.Sprintf("%016X", c.fabric.ControllerNodeID),
		Nodes:        len(nodes),
		Unplaceable:  unplaceable,
	}, nodes, nil
}
