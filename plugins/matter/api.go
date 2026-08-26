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
		result := map[string]any{
			"generatedAt": time.Now().UTC().Format(time.RFC3339),
			"fabric":      fabric, "nodes": statuses,
		}
		if a.backing != nil {
			result["sharingWindows"] = a.backing.sharingWindows()
		}
		return result, nil
	})
	stulp.OnRequest("bridge/devices", func(map[string]any, map[string]any) (any, error) {
		if a.bridge == nil {
			return nil, fmt.Errorf("Matter bridge is not running")
		}
		return map[string]any{"devices": a.bridge.Candidates(), "record": a.bridge.Record()}, nil
	})
	stulp.OnRequest("bridge/export", func(_, body map[string]any) (any, error) {
		if a.bridge == nil {
			return nil, fmt.Errorf("Matter bridge is not running")
		}
		deviceID, _ := body["deviceId"].(string)
		exported, ok := body["exported"].(bool)
		if deviceID == "" || !ok {
			return nil, fmt.Errorf("deviceId en exported zijn nodig")
		}
		endpoint, err := a.bridge.SetExported(deviceID, exported)
		if err != nil {
			return nil, err
		}
		return map[string]any{"endpoint": endpoint, "devices": a.bridge.Candidates()}, nil
	})

	// Native Matter Multi-Admin. Opening and revoking can require CASE to a
	// sleeping Thread node, so both use the same start/poll shape as
	// diagnostics. Holding the app protocol request open would serialize every
	// other settings call behind that sleepy device.
	stulp.OnRequest("sharing/open", func(_, body map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		deviceID, _ := body["deviceId"].(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		duration, err := sharingDuration(body)
		if err != nil {
			return nil, err
		}
		job, err := a.shareJob(deviceID)
		if err != nil {
			return nil, err
		}
		if !job.begin() {
			return job.snapshot(), nil
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			window, openErr := controller.OpenSharingWindow(ctx, deviceID, duration)
			if openErr == nil {
				if a.backing == nil {
					openErr = fmt.Errorf("Matter state is not available")
				} else if saveErr := a.backing.saveSharingWindow(window); saveErr != nil {
					// The accessory has already opened the window. Preserve the
					// usable code in this process even when durable storage failed.
					job.put("window", window)
					openErr = fmt.Errorf("window is open but its code could not be saved: %w", saveErr)
				} else {
					job.put("window", window)
				}
			}
			job.done(openErr)
		}()
		return job.snapshot(), nil
	})
	stulp.OnRequest("sharing/revoke", func(_, body map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		deviceID, _ := body["deviceId"].(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		job, err := a.shareJob(deviceID)
		if err != nil {
			return nil, err
		}
		if !job.begin() {
			return job.snapshot(), nil
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			revokeErr := controller.RevokeSharingWindow(ctx, deviceID)
			if revokeErr == nil && a.backing != nil {
				for _, window := range a.backing.sharingWindows() {
					if window.DeviceID == deviceID {
						revokeErr = a.backing.deleteSharingWindow(window.NodeID)
						break
					}
				}
			}
			if revokeErr == nil {
				job.put("revoked", true)
			}
			job.done(revokeErr)
		}()
		return job.snapshot(), nil
	})
	stulp.OnRequest("sharing/state", func(_, body map[string]any) (any, error) {
		deviceID, _ := body["deviceId"].(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		job, err := a.shareJob(deviceID)
		if err != nil {
			return nil, err
		}
		return job.snapshot(), nil
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

	// Diagnostiek gaat naar het apparaat zelf, en dat is geen milliseconden-werk:
	// een node met een slaap-interval van 17 seconden mag volgens de spec zó lang
	// over één antwoord doen, dus een CASE-sessie plus vijf cluster-reads kan
	// minuten duren. Daarom dezelfde vorm als scan en mesh, en niet omdat het
	// mooier staat: verzoeken van deze plugin komen bij Stulp op ÉÉN geordende
	// baan binnen (appproto: "verzoeken komen daar één voor één binnen"), dus een
	// synchrone diagnose hield ook het opnieuw laden van de pagina tegen — en het
	// aanzetten van een lamp.
	stulp.OnRequest("diagnostics", func(query, body map[string]any) (any, error) {
		controller, err := a.running()
		if err != nil {
			return nil, err
		}
		deviceID, _ := first(body["deviceId"], query["deviceId"]).(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		running, err := a.diagnosis(deviceID)
		if err != nil {
			return nil, err
		}
		if !running.begin() {
			return running.snapshot(), nil
		}
		go func() {
			// Ruim boven de MRP-uithoudingstijd van het traagste apparaat in deze
			// fabric (17s basis = ~190s aan hertransmissies) plus de reads erna.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			result, diagErr := controller.Diagnostics(ctx, deviceID)
			if diagErr == nil {
				running.put("diagnostics", result)
			}
			running.done(diagErr)
		}()
		return running.snapshot(), nil
	})
	stulp.OnRequest("diagnostics/state", func(query, body map[string]any) (any, error) {
		deviceID, _ := first(body["deviceId"], query["deviceId"]).(string)
		if deviceID == "" {
			return nil, fmt.Errorf("een apparaat-id is nodig")
		}
		running, err := a.diagnosis(deviceID)
		if err != nil {
			return nil, err
		}
		return running.snapshot(), nil
	})
}

// diagnosis geeft de diagnose-plek van één apparaat. Per apparaat, want de
// pagina kan er meerdere openzetten en dan hoort de tweede niet het antwoord van
// de eerste te zien.
//
// De grens is er omdat dit een map is die de pagina laat groeien: een
// apparaat-id komt van buiten, en zonder plafond kan iemand er onbeperkt in
// schrijven. maxDiagnoses ligt boven het aantal Matter-apparaten dat een huis
// heeft; erboven weigert hij luid in plaats van geheugen te blijven pakken.
func (a *app) diagnosis(deviceID string) (*scan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if running, ok := a.diagnoses[deviceID]; ok {
		return running, nil
	}
	if len(a.diagnoses) >= maxDiagnoses {
		return nil, fmt.Errorf("er staan al %d diagnoses open; herlaad de pagina", maxDiagnoses)
	}
	if a.diagnoses == nil {
		a.diagnoses = map[string]*scan{}
	}
	running := &scan{}
	a.diagnoses[deviceID] = running
	return running, nil
}

func (a *app) shareJob(deviceID string) (*scan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if running, ok := a.shares[deviceID]; ok {
		return running, nil
	}
	if len(a.shares) >= maxDiagnoses {
		return nil, fmt.Errorf("er staan al %d Matter-deelacties open; herlaad de pagina", maxDiagnoses)
	}
	if a.shares == nil {
		a.shares = map[string]*scan{}
	}
	running := &scan{}
	a.shares[deviceID] = running
	return running, nil
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

func sharingDuration(body map[string]any) (time.Duration, error) {
	seconds := 900.0
	if supplied, ok := body["seconds"]; ok {
		var number float64
		switch value := supplied.(type) {
		case float64:
			number = value
		case int:
			number = float64(value)
		default:
			return 0, fmt.Errorf("deelduur moet een aantal seconden zijn")
		}
		seconds = number
	}
	if seconds < 180 || seconds > 900 || seconds != float64(int(seconds)) {
		return 0, fmt.Errorf("deelduur moet 180..900 hele seconden zijn")
	}
	return time.Duration(seconds) * time.Second, nil
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
