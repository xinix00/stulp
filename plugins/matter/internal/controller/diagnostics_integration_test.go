package controller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The decoder unit tests feed values in directly. This one asks a commissioned
// node over real sockets: wildcard read request, report data, TLV, decode. If
// the wildcard path were encoded wrongly, or the node answered per-attribute
// paths only, nothing here would arrive.
func TestDiagnosticsReadFromACommissionedNode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	database := newBacking()
	// Ruim, want dit is een echte commissioning over echte sockets: PASE, CASE
	// en een abonnement. Onder een volle testrun -- alle pakketten parallel,
	// soms met een draaiende Stulp ernaast -- haalde dit een keer de dertig
	// seconden niet. Een test die faalt omdat de machine het druk had zegt
	// niets over de code.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	controller, err := New(ctx, database, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	controller.node.RetryInterval = 20 * time.Millisecond
	device, err := newStatefulMatterDevice(t, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer device.node.Close()
	go func() { _ = device.serve(ctx, controller.fabric) }()

	added, err := commissionAndStore(ctx, t, controller, database, CommissionRequest{
		Code: "34970112332", Address: device.node.LocalAddr().String(),
	})
	if err != nil {
		t.Fatalf("Commission: %v", err)
	}
	waitForSubscription(t, ctx, controller, fakeNodeID)

	diagnostics, err := controller.Diagnostics(ctx, added[0].ID)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diagnostics.Errors) != 0 {
		t.Fatalf("diagnostics reported errors: %v", diagnostics.Errors)
	}
	if diagnostics.VendorName != "Stulp Labs" || diagnostics.ProductName != "Fake Lamp" {
		t.Fatalf("identity = %#v", diagnostics)
	}
	if diagnostics.SoftwareVersion != "1.4.2" || diagnostics.SerialNumber != "SN-0001" {
		t.Fatalf("build = %#v", diagnostics)
	}
	if diagnostics.UpTimeSeconds == nil || *diagnostics.UpTimeSeconds != 93_784 {
		t.Fatalf("uptime = %v", diagnostics.UpTimeSeconds)
	}
	if diagnostics.BootReason != "hardwarewatchdog" {
		t.Fatalf("boot reason = %q", diagnostics.BootReason)
	}

	if diagnostics.Thread == nil {
		t.Fatalf("no Thread diagnostics: %#v", diagnostics)
	}
	if diagnostics.Thread.NetworkName != "Stulp-Thread" || diagnostics.Thread.RoutingRole != "router" {
		t.Fatalf("thread = %#v", diagnostics.Thread)
	}
	if len(diagnostics.Thread.Neighbours) != 1 {
		t.Fatalf("neighbours = %#v", diagnostics.Thread.Neighbours)
	}
	neighbour := diagnostics.Thread.Neighbours[0]
	if neighbour.LQI == nil || *neighbour.LQI != 180 {
		t.Fatalf("neighbour LQI = %v", neighbour.LQI)
	}
	if neighbour.AverageRSSI == nil || *neighbour.AverageRSSI != -62 {
		t.Fatalf("neighbour RSSI = %v", neighbour.AverageRSSI)
	}
	if neighbour.ExtAddress != "A1B2C3D4E5F60718" {
		t.Fatalf("neighbour address = %q", neighbour.ExtAddress)
	}

	// The node is on Thread, so the Wi-Fi cluster answers nothing. That has to
	// read as "not offered" rather than as an empty radio.
	if diagnostics.WiFi != nil {
		t.Fatalf("a Thread node reported Wi-Fi diagnostics: %#v", diagnostics.WiFi)
	}
}

// The mesh joins topology, diagnostics and DNS-SD. This drives it against the
// same fake accessory over real sockets, so the pieces are checked together
// rather than only in isolation.
func TestMeshDrawsACommissionedNode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	database := newBacking()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	controller, err := New(ctx, database, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	controller.node.RetryInterval = 20 * time.Millisecond
	device, err := newStatefulMatterDevice(t, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer device.node.Close()
	go func() { _ = device.serve(ctx, controller.fabric) }()

	if _, err := commissionAndStore(ctx, t, controller, database, CommissionRequest{
		Code: "34970112332", Address: device.node.LocalAddr().String(),
	}); err != nil {
		t.Fatalf("Commission: %v", err)
	}
	waitForSubscription(t, ctx, controller, fakeNodeID)

	// The callbacks are what let the page draw while nodes answer: the shape
	// first, then each answer with the links it proves.
	var progressMu sync.Mutex
	var opening MeshMap
	progress := make([]MeshUpdate, 0, 1)
	mesh, err := controller.Mesh(ctx, time.Second,
		func(start MeshMap) {
			progressMu.Lock()
			defer progressMu.Unlock()
			opening = start
		},
		func(update MeshUpdate) {
			progressMu.Lock()
			defer progressMu.Unlock()
			if update.Total != 1 {
				t.Errorf("progress reported a total of %d, want 1", update.Total)
			}
			progress = append(progress, update)
		})
	if err != nil {
		t.Fatalf("Mesh: %v", err)
	}
	if len(opening.Nodes) != 1 || !opening.Nodes[0].Pending {
		t.Fatalf("opening map = %#v, want one pending node", opening.Nodes)
	}
	if len(progress) != len(mesh.Nodes) {
		t.Fatalf("progress fired %d times for %d nodes", len(progress), len(mesh.Nodes))
	}
	// The links have to travel with the answer, not only at the end.
	if len(progress[0].Links) != 1 || progress[0].Links[0].Kind != "radio" {
		t.Fatalf("the answer carried links %#v", progress[0].Links)
	}
	if progress[0].Links[0].LQI == nil || *progress[0].Links[0].LQI != 180 {
		t.Fatalf("the streamed link lost its measurement: %#v", progress[0].Links[0])
	}
	if len(mesh.Nodes) != 1 {
		t.Fatalf("mesh nodes = %#v", mesh.Nodes)
	}
	drawn := mesh.Nodes[0]
	if drawn.Error != "" {
		t.Fatalf("node reported an error: %s", drawn.Error)
	}
	if drawn.Radio != "thread" || drawn.RoutingRole != "router" {
		t.Fatalf("node radio = %#v", drawn)
	}
	if drawn.Neighbours != 1 {
		t.Fatalf("neighbour count = %d, want 1", drawn.Neighbours)
	}
	if !drawn.SessionOpen || !drawn.Subscribed {
		t.Fatalf("a live node is drawn as idle: %#v", drawn)
	}
	// The neighbour belongs to no node Stulp knows, so it must be counted as
	// outside the fabric rather than drawn as one of ours.
	if len(mesh.Links) != 1 || mesh.Links[0].To != "" {
		t.Fatalf("links = %#v", mesh.Links)
	}
	if mesh.Unidentified != 1 {
		t.Fatalf("unidentified = %d, want 1", mesh.Unidentified)
	}
	if mesh.Links[0].LQI == nil || *mesh.Links[0].LQI != 180 {
		t.Fatalf("link LQI = %v", mesh.Links[0].LQI)
	}
}
