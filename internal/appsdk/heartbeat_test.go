package appsdk

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
)

func TestServeReturnsWhenStulpLeavesTheOldConnectionHalfOpen(t *testing.T) {
	stulpSide, appSide := net.Pipe()
	peer := appproto.NewConn(stulpSide)
	defer peer.Close()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveWithHeartbeat(appproto.NewConn(appSide), Plugin{}, time.Millisecond, 30*time.Millisecond)
	}()

	hello, err := peer.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if hello.T != appproto.KindRequest || hello.M != "hello" {
		t.Fatalf("first app frame = %+v, expected hello", hello)
	}
	welcome, err := json.Marshal(Welcome{Protocol: ProtocolVersion, AppID: "com.demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.WriteFrame(appproto.Frame{T: appproto.KindResponse, ID: hello.ID, R: welcome}); err != nil {
		t.Fatal(err)
	}

	// The stream stays open and readable, exactly like the stale connection
	// observed after Stulp restarted. Consuming but not answering the ping also
	// proves that a successful write alone is not considered liveness.
	if _, err := peer.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-serveDone:
		if err == nil || !strings.Contains(err.Error(), "stopped answering heartbeats") {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("half-open Stulp connection did not release Serve for reconnect")
	}
}

func TestMonitorSessionRejectsAConnectedPeerThatStopsAnswering(t *testing.T) {
	x, y := net.Pipe()
	session := appproto.NewSession(appproto.NewConn(x), nil, nil)
	go session.Serve()
	peer := appproto.NewConn(y)
	defer peer.Close()

	// Read the ping so its write completes, but deliberately never answer it.
	gotPing := make(chan struct{})
	go func() {
		if _, err := peer.ReadFrame(); err == nil {
			close(gotPing)
		}
	}()

	ticks := make(chan time.Time, 1)
	failed := make(chan error, 1)
	go func() {
		failed <- monitorSession(context.Background(), session, ticks, 30*time.Millisecond)
	}()
	ticks <- time.Now()

	select {
	case <-gotPing:
	case <-time.After(time.Second):
		t.Fatal("heartbeat sent no protocol ping")
	}
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("silent peer was considered alive")
		}
	case <-time.After(time.Second):
		t.Fatal("silent peer was not rejected after the heartbeat timeout")
	}

	_ = session.Close()
}

func TestMonitorSessionKeepsAResponsivePeerAlive(t *testing.T) {
	x, y := net.Pipe()
	a := appproto.NewSession(appproto.NewConn(x), nil, nil)
	b := appproto.NewSession(appproto.NewConn(y), nil, nil)
	go a.Serve()
	go b.Serve()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	finished := make(chan error, 1)
	go func() { finished <- monitorSession(ctx, a, ticks, 100*time.Millisecond) }()
	ticks <- time.Now()

	select {
	case <-a.Done():
		t.Fatal("responsive peer was disconnected")
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
