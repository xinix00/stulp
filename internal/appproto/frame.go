// Package appproto is the wire protocol between Stulp and an out-of-process
// app. See docs/app-processes.md for the design it implements.
//
// Length-prefixed JSON, four message kinds, both directions on one connection.
// JSON rather than a schema language because the payloads are dynamic
// JavaScript values — device settings, capability values, flow tokens — which
// come out of app.json and the store as JSON already.
package appproto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameSize caps a single frame.
//
// The cap is enforced on both sides and a violation ends the connection. It is
// never truncated and never partially decoded: a peer that announces more than
// this is either broken or hostile, and continuing with half a message is worse
// than stopping. The supervisor restarts the app, which is a visible failure
// rather than a silent one.
const MaxFrameSize = 8 << 20 // 8 MiB

// ErrFrameTooLarge is returned when either side sees an oversized frame.
var ErrFrameTooLarge = errors.New("appproto: frame exceeds maximum size")

// Kind is the message type. Four is the whole protocol.
type Kind string

const (
	KindRequest  Kind = "req" // expects exactly one res or err
	KindResponse Kind = "res"
	KindError    Kind = "err"
	KindEvent    Kind = "ev" // one-way, unacknowledged
)

// Frame is one message.
//
// Field names are short because they repeat on every message and this runs on a
// hub, not a workstation.
type Frame struct {
	T  Kind            `json:"t"`
	ID uint64          `json:"id,omitempty"`
	M  string          `json:"m,omitempty"` // method, on req and ev
	P  json.RawMessage `json:"p,omitempty"` // params
	R  json.RawMessage `json:"r,omitempty"` // result
	E  *Error          `json:"e,omitempty"`
}

// Error carries a failure across the boundary.
//
// A plugin is a Go program, so an error is what a Go error is: a sentence. It
// travels as its own field rather than as a result value so the caller cannot
// mistake a failure for an answer, and it stays whole -- what a plugin says
// about a device is what the user gets to read.
type Error struct {
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Conn is framed access to a byte stream.
//
// Read must be called from a single goroutine; Write is safe from several. That
// asymmetry is deliberate and matches how both sides use it: one reader
// goroutine feeding a queue, and writes coming from wherever a reply is
// produced.
type Conn struct {
	rw io.ReadWriteCloser
	br *bufio.Reader

	writeMu sync.Mutex
	bw      *bufio.Writer

	closeOnce sync.Once
	closeErr  error
}

func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{
		rw: rw,
		br: bufio.NewReaderSize(rw, 64<<10),
		bw: bufio.NewWriterSize(rw, 64<<10),
	}
}

// WriteRaw sends one length-prefixed message without interpreting it.
//
// WriteFrame is this plus the encoding. It exists on its own for the attach
// greeting, which travels over the same connection before either side speaks
// frames: one framing rule in the codebase rather than two.
func (c *Conn) WriteRaw(body []byte) error {
	if len(body) > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(body))
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.bw.Write(header[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(body); err != nil {
		return err
	}
	return c.bw.Flush()
}

// ReadRaw reads the next length-prefixed message without decoding it. io.EOF
// means the peer closed cleanly.
//
// The cap is checked before the allocation: a corrupt or hostile prefix must
// not be able to make us reserve four gigabytes.
func (c *Conn) ReadRaw() ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])

	if length > MaxFrameSize {
		return nil, fmt.Errorf("%w: peer announced %d bytes", ErrFrameTooLarge, length)
	}
	if length == 0 {
		return nil, errors.New("appproto: empty frame")
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(c.br, body); err != nil {
		// A short read here is a truncated frame, not a smaller message.
		// ReadFull turns that into ErrUnexpectedEOF, which the caller must
		// treat as a dead connection rather than as data.
		return nil, fmt.Errorf("appproto: truncated frame of %d bytes: %w", length, err)
	}
	return body, nil
}

// WriteFrame encodes and sends one frame.
func (c *Conn) WriteFrame(f Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("appproto: encode: %w", err)
	}
	if len(body) > MaxFrameSize {
		// Refuse to send what the peer is required to reject. Failing here
		// names the oversized message; failing there would only report a
		// protocol violation with no clue what caused it.
		return fmt.Errorf("%w: %d bytes in %s %s", ErrFrameTooLarge, len(body), f.T, f.M)
	}
	return c.WriteRaw(body)
}

// ReadFrame reads the next frame. io.EOF means the peer closed cleanly.
func (c *Conn) ReadFrame() (Frame, error) {
	body, err := c.ReadRaw()
	if err != nil {
		return Frame{}, err
	}

	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return Frame{}, fmt.Errorf("appproto: decode: %w", err)
	}
	switch f.T {
	case KindRequest, KindResponse, KindError, KindEvent:
	default:
		return Frame{}, fmt.Errorf("appproto: unknown frame kind %q", f.T)
	}
	return f, nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		// Every complete frame is flushed by WriteRaw, so there is no useful
		// buffered data to preserve here. More importantly, Close must not wait
		// for writeMu: closing the stream is precisely how a heartbeat aborts a
		// write that is stuck against an unreachable peer.
		c.closeErr = c.rw.Close()
	})
	return c.closeErr
}
