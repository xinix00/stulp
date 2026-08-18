package appproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Handler answers an incoming request. Returning an error turns into an err
// frame; returning a value turns into a res frame.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// EventHandler receives one-way messages. It must not block: it runs on the
// reader goroutine, and blocking there stops replies from being read.
type EventHandler func(method string, params json.RawMessage)

// ErrClosed is returned by Call once the session is finished.
var ErrClosed = errors.New("appproto: session is closed")

const maxQueuedRequests = 1024

// pingMethod is protocol plumbing, not an application callback. Handling it on
// the reader goroutine is intentional: a slow OnInit or device callback must
// not make a healthy connection look dead.
const pingMethod = "$appproto.ping"

// Session runs the protocol over a Conn: it correlates replies to requests and
// dispatches incoming requests to a handler.
//
// Both ends use the same type. Request ids are scoped to the sender, so both
// sides count from 1 without colliding.
type Session struct {
	conn    *Conn
	handle  Handler
	onEvent EventHandler

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan Frame
	closed  bool
	cause   error

	done chan struct{}

	requestMu      sync.Mutex
	requestCond    *sync.Cond
	requestQueue   []Frame
	requestsClosed bool
	handlerCtx     context.Context
	cancelHandler  context.CancelFunc
}

func NewSession(conn *Conn, handle Handler, onEvent EventHandler) *Session {
	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	session := &Session{
		conn:       conn,
		handle:     handle,
		onEvent:    onEvent,
		pending:    map[uint64]chan Frame{},
		done:       make(chan struct{}),
		handlerCtx: handlerCtx, cancelHandler: cancelHandler,
	}
	session.requestCond = sync.NewCond(&session.requestMu)
	return session
}

// Serve reads frames until the connection ends. It must run in its own
// goroutine and is the only caller of ReadFrame.
//
// Requests go through one ordered worker. The reader only appends to a bounded
// in-memory queue, so it remains free to deliver replies (including replies
// needed by the currently running handler) without spawning an unbounded
// goroutine per request or reordering requests. A peer that fills the generous
// queue is disconnected instead of consuming memory without limit.
func (s *Session) Serve() (serveErr error) {
	go s.answerLoop()
	defer func() {
		s.stopAnswers()
		s.finish(serveErr)
	}()

	for {
		frame, err := s.conn.ReadFrame()
		if err != nil {
			return err
		}

		switch frame.T {
		case KindResponse, KindError:
			s.deliver(frame)
		case KindRequest:
			if frame.M == pingMethod {
				if err := s.conn.WriteFrame(Frame{
					T: KindResponse, ID: frame.ID, R: json.RawMessage("true"),
				}); err != nil {
					return err
				}
				continue
			}
			if !s.enqueueRequest(frame) {
				return fmt.Errorf("appproto: more than %d requests queued", maxQueuedRequests)
			}
		case KindEvent:
			if s.onEvent != nil {
				s.onEvent(frame.M, frame.P)
			}
		}
	}
}

func (s *Session) enqueueRequest(frame Frame) bool {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.requestsClosed || len(s.requestQueue) >= maxQueuedRequests {
		return false
	}
	s.requestQueue = append(s.requestQueue, frame)
	s.requestCond.Signal()
	return true
}

func (s *Session) answerLoop() {
	for {
		s.requestMu.Lock()
		for len(s.requestQueue) == 0 && !s.requestsClosed {
			s.requestCond.Wait()
		}
		if s.requestsClosed {
			s.requestMu.Unlock()
			return
		}
		frame := s.requestQueue[0]
		s.requestQueue[0] = Frame{}
		s.requestQueue = s.requestQueue[1:]
		s.requestMu.Unlock()
		s.answer(frame)
	}
}

func (s *Session) stopAnswers() {
	s.requestMu.Lock()
	s.requestsClosed = true
	s.requestQueue = nil
	s.requestCond.Broadcast()
	s.requestMu.Unlock()
}

func (s *Session) answer(req Frame) {
	if s.handle == nil {
		_ = s.conn.WriteFrame(Frame{
			T: KindError, ID: req.ID,
			E: &Error{Message: "no handler for " + req.M},
		})
		return
	}

	result, err := s.handle(s.handlerCtx, req.M, req.P)
	if err != nil {
		_ = s.conn.WriteFrame(Frame{T: KindError, ID: req.ID, E: asProtoError(err)})
		return
	}
	encoded, merr := json.Marshal(result)
	if merr != nil {
		_ = s.conn.WriteFrame(Frame{
			T: KindError, ID: req.ID,
			E: &Error{Message: "result is not encodable: " + merr.Error()},
		})
		return
	}
	_ = s.conn.WriteFrame(Frame{T: KindResponse, ID: req.ID, R: encoded})
}

// asProtoError keeps a JavaScript error's shape and wraps anything else.
func asProtoError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Message: err.Error()}
}

// Call sends a request and waits for its reply.
//
// It returns as soon as the session ends, so a call against a dead app fails
// fast instead of hanging. That property is the reason the pending map is
// drained on close rather than left to a timeout.
func (s *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("appproto: encode params for %s: %w", method, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, s.closeCause()
	}
	s.nextID++
	id := s.nextID
	reply := make(chan Frame, 1)
	s.pending[id] = reply
	s.mu.Unlock()

	if err := s.conn.WriteFrame(Frame{T: KindRequest, ID: id, M: method, P: encoded}); err != nil {
		s.forget(id)
		return nil, err
	}

	select {
	case frame := <-reply:
		if frame.T == KindError {
			if frame.E != nil {
				return nil, frame.E
			}
			return nil, errors.New("appproto: error frame without detail")
		}
		return frame.R, nil
	case <-ctx.Done():
		s.forget(id)
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.closeCause()
	}
}

// Ping proves that this exact protocol session still has a peer. It is a
// request rather than a separate UDP probe: a reply from a newly started
// process on the same address says nothing about the old stream.
//
// A protocol error still counts as a pong. That keeps liveness compatible with
// an older peer which does not reserve pingMethod yet but did read and answer
// the request through its normal unknown-method path.
func (s *Session) Ping(ctx context.Context) error {
	_, err := s.Call(ctx, pingMethod, nil)
	if err == nil {
		return nil
	}
	var peerError *Error
	if errors.As(err, &peerError) {
		return nil
	}
	return err
}

// Notify sends a one-way message. There is no reply and no acknowledgement, so
// this is only for things no caller needs the result of.
func (s *Session) Notify(method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("appproto: encode params for %s: %w", method, err)
	}
	return s.conn.WriteFrame(Frame{T: KindEvent, M: method, P: encoded})
}

func (s *Session) deliver(frame Frame) {
	s.mu.Lock()
	reply, ok := s.pending[frame.ID]
	delete(s.pending, frame.ID)
	s.mu.Unlock()
	if ok {
		reply <- frame
	}
	// A reply to an unknown id means the peer answered something we already
	// gave up on. Dropping it is correct; there is nobody left to hand it to.
}

func (s *Session) forget(id uint64) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// finish closes the session and releases every waiting call.
func (s *Session) finish(cause error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cause = cause
	s.pending = map[uint64]chan Frame{}
	s.mu.Unlock()

	s.cancelHandler()
	close(s.done)
	_ = s.conn.Close()
}

// Close ends the session. Waiting calls fail immediately.
func (s *Session) Close() error {
	s.finish(ErrClosed)
	return nil
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) closeCause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != nil {
		return s.cause
	}
	return ErrClosed
}
