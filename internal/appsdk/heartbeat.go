package appsdk

import (
	"context"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
)

const (
	sessionHeartbeatInterval = 5 * time.Second
	sessionHeartbeatTimeout  = 5 * time.Second
)

// monitorSession checks the app-protocol stream itself. A UDP echo from the
// current Stulp process could succeed while this Session is still attached to
// an old, half-open TCP connection, so it would not be a useful liveness proof.
func monitorSession(ctx context.Context, session *appproto.Session, ticks <-chan time.Time, timeout time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-session.Done():
			return nil
		case <-ticks:
			if err := probeSession(ctx, session, timeout); err != nil {
				select {
				case <-ctx.Done():
					return nil
				case <-session.Done():
					return nil
				default:
					return err
				}
			}
		}
	}
}

// probeSession has its own outer timer. Session.Call honors its context while
// waiting for a response, but a write can itself block when a peer has vanished.
// The outer timer lets serve close the connection, which interrupts both cases.
func probeSession(ctx context.Context, session *appproto.Session, timeout time.Duration) error {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- session.Ping(probeCtx) }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		return context.DeadlineExceeded
	case <-ctx.Done():
		return nil
	case <-session.Done():
		return nil
	}
}
