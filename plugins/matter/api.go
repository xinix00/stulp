package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	mattercontroller "github.com/xinix00/stulp/plugins/matter/internal/controller"
	"github.com/xinix00/stulp/plugins/matter/internal/discovery"
)

// De config-pagina van deze plugin.
//
// Alles hier is vraag-antwoord: de pagina vraagt, de plugin antwoordt. Er is
// geen weg van de plugin naar het scherm, en die is er ook niet nodig -- wat
// lang duurt draait in een goroutine en de pagina haalt op wat er inmiddels is.
//
// Dat scheelt niet alleen een protocolvorm. Het houdt ook de last bij de
// plugin: een node wordt één keer bevraagd en het antwoord blijft staan, hoe
// vaak de pagina ook kijkt. Thread-nodes hebben er een hekel aan om elke
// seconde om diagnostiek gevraagd te worden.

// scan is een lopende of afgeronde verkenning.
type scan struct {
	mu       sync.Mutex
	running  bool
	started  time.Time
	finished time.Time
	result   map[string]any
	failure  string
}

func (s *scan) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"running": s.running}
	if !s.started.IsZero() {
		out["startedAt"] = s.started.UTC().Format(time.RFC3339)
	}
	if !s.finished.IsZero() {
		out["finishedAt"] = s.finished.UTC().Format(time.RFC3339)
	}
	if s.failure != "" {
		out["warning"] = s.failure
	}
	for key, value := range s.result {
		out[key] = value
	}
	return out
}

// begin claimt de verkenning. Twee tegelijk laten lopen zou de nodes dubbel
// bevragen zonder dat het antwoord er sneller van komt.
func (s *scan) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running, s.started, s.failure = true, time.Now(), ""
	s.result = map[string]any{}
	return true
}

func (s *scan) put(key string, value any) {
	s.mu.Lock()
	if s.result == nil {
		s.result = map[string]any{}
	}
	s.result[key] = value
	s.mu.Unlock()
}

func (s *scan) done(failure error) {
	s.mu.Lock()
	s.running, s.finished = false, time.Now()
	if failure != nil {
		s.failure = failure.Error()
	}
	s.mu.Unlock()
}

// registerAPI hangt de pagina aan de controller.
func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("network", func(map[string]any, map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		fabric, statuses, err := controller.Topology(context.Background())
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"generatedAt": time.Now().UTC().Format(time.RFC3339),
			"fabric":      fabric, "nodes": statuses,
		}, nil
	})

	// Zoeken op het netwerk. Start hem en kijk daarna.
	stulp.OnRequest("scan", func(_, body map[string]any) (any, error) {
		if !a.discovery.begin() {
			return a.discovery.snapshot(), nil
		}
		window := scanWindow(body)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), window+5*time.Second)
			defer cancel()
			// Alleen Matter's eigen diensten en de Thread border routers die ze
			// dragen. De rest van het netwerk gaat ons niet aan.
			nodes, err := discovery.BrowseServices(ctx, window,
				discovery.ServiceOperational, discovery.ServiceCommissionable, discovery.ServiceBorderRouter)
			operational, commissionable, routers := splitServices(nodes)
			a.discovery.put("operational", operational)
			a.discovery.put("commissionable", commissionable)
			a.discovery.put("borderRouters", routers)
			a.discovery.put("window", window.String())
			// Multicast wordt routineus geblokkeerd door VPN-software. Dat
			// melden is eerlijker dan een leeg netwerk als bevinding tonen.
			a.discovery.done(err)
		}()
		return a.discovery.snapshot(), nil
	})
	stulp.OnRequest("scan/state", func(map[string]any, map[string]any) (any, error) {
		return a.discovery.snapshot(), nil
	})

	// De mesh tekenen is het duurste wat deze pagina kan vragen: elke node wordt
	// bevraagd, één voor één om de border router te ontzien. Een groot huis kost
	// minuten, dus het antwoord groeit terwijl de pagina kijkt.
	stulp.OnRequest("mesh", func(_, body map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		if !a.mesh.begin() {
			return a.mesh.snapshot(), nil
		}
		window := scanWindow(body)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()
			nodes := map[string]mattercontroller.MeshNode{}
			links := map[string]mattercontroller.MeshLink{}
			publish := func() {
				a.mesh.put("nodes", values(nodes))
				a.mesh.put("links", drawLinks(linkValues(links)))
			}
			result, meshErr := controller.Mesh(ctx, window,
				func(start mattercontroller.MeshMap) {
					for _, node := range start.Nodes {
						nodes[node.NodeID] = node
					}
					a.mesh.put("routers", start.Routers)
					publish()
				},
				func(update mattercontroller.MeshUpdate) {
					nodes[update.Node.NodeID] = update.Node
					for _, link := range update.Links {
						links[linkID(link)] = link
					}
					publish()
				})
			if meshErr == nil {
				a.mesh.put("nodes", result.Nodes)
				a.mesh.put("routers", result.Routers)
				a.mesh.put("links", drawLinks(result.Links))
				a.mesh.put("unidentified", result.Unidentified)
				a.mesh.put("warnings", result.Warnings)
			}
			a.mesh.done(meshErr)
		}()
		return a.mesh.snapshot(), nil
	})
	stulp.OnRequest("mesh/state", func(map[string]any, map[string]any) (any, error) {
		return a.mesh.snapshot(), nil
	})

	stulp.OnRequest("diagnostics", func(query, body map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		deviceID, _ := first(body["deviceId"], query["deviceId"]).(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		return controller.Diagnostics(context.Background(), deviceID)
	})
}

func (a *app) running() (*mattercontroller.Controller, error) {
	if a.controller == nil {
		return nil, fmt.Errorf("Matter controller is not running")
	}
	return a.controller, nil
}

// scanWindow leest hoe lang er geluisterd wordt. Te kort mist slaperige nodes,
// te lang laat iemand kijken naar niets.
func scanWindow(body map[string]any) time.Duration {
	seconds, _ := body["window"].(float64)
	if seconds < 1 || seconds > 30 {
		return 4 * time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

func first(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

func values(nodes map[string]mattercontroller.MeshNode) []mattercontroller.MeshNode {
	out := make([]mattercontroller.MeshNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
	}
	return out
}

func linkValues(links map[string]mattercontroller.MeshLink) []mattercontroller.MeshLink {
	out := make([]mattercontroller.MeshLink, 0, len(links))
	for _, link := range links {
		out = append(out, link)
	}
	return out
}

// splitServices deelt op wat de verkenning vond.
func splitServices(nodes []discovery.Node) (operational, commissionable, routers []map[string]any) {
	operational, commissionable, routers = []map[string]any{}, []map[string]any{}, []map[string]any{}
	for _, node := range nodes {
		entry := map[string]any{
			"instance": node.Instance, "host": node.Host, "port": node.Port,
			"addresses": node.Addresses,
		}
		switch node.Kind {
		case "operational":
			entry["nodeId"] = node.NodeID
			entry["compressedFabricId"] = node.CompressedFabricID
			// SII and SAI are the node's own idle and active polling intervals.
			// A sleepy Thread device answers slowly by design, and these say
			// how slowly, which is the difference between "asleep" and "gone".
			entry["idleIntervalMs"] = node.Text["SII"]
			entry["activeIntervalMs"] = node.Text["SAI"]
			entry["activeThresholdMs"] = node.Text["SAT"]
			operational = append(operational, entry)
		case "commissionable":
			entry["deviceName"] = node.DeviceName
			entry["discriminator"] = node.Discriminator
			entry["vendorId"] = node.VendorID
			entry["productId"] = node.ProductID
			entry["commissioningMode"] = node.CommissioningMode
			commissionable = append(commissionable, entry)
		default:
			// Thread border routers publish their network name and extended PAN
			// ID, which is what ties a node's Thread diagnostics to a router.
			// The PAN ID arrives as eight raw bytes, not as text.
			entry["networkName"] = node.Text["nn"]
			entry["extendedPanId"] = hex.EncodeToString([]byte(node.Text["xp"]))
			entry["vendor"] = node.Text["vn"]
			entry["model"] = node.Text["mn"]
			entry["threadVersion"] = node.Text["tv"]
			routers = append(routers, entry)
		}
	}
	return operational, commissionable, routers
}
