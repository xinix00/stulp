package appproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipe levert twee gekoppelde sessies, zoals een socketpair dat straks doet.
func pipe(t *testing.T, aHandle, bHandle Handler) (*Session, *Session) {
	t.Helper()
	x, y := net.Pipe()
	a := NewSession(NewConn(x), aHandle, nil)
	b := NewSession(NewConn(y), bHandle, nil)
	go a.Serve()
	go b.Serve()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestRoundTrip(t *testing.T) {
	a, _ := pipe(t, nil, func(_ context.Context, method string, params json.RawMessage) (any, error) {
		if method != "device.onInit" {
			return nil, errors.New("onverwachte methode " + method)
		}
		var in map[string]string
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		return map[string]string{"echo": in["deviceId"]}, nil
	})

	raw, err := a.Call(context.Background(), "device.onInit", map[string]string{"deviceId": "d1"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["echo"] != "d1" {
		t.Fatalf("kreeg %v", out)
	}
}

// Een fout moet ongewijzigd overkomen. Wat een plugin zegt over een apparaat is
// wat de gebruiker leest, dus er mag niets omheen: geen soortnaam ervoor, geen
// omhulsel eromheen.
func TestErrorArrivesWhole(t *testing.T) {
	a, _ := pipe(t, nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &Error{Message: "een apparaat-id is nodig"}
	})

	_, err := a.Call(context.Background(), "boom", nil)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("fout is geen protocolfout: %T %v", err, err)
	}
	if pe.Message != "een apparaat-id is nodig" {
		t.Fatalf("bericht ging verloren: %+v", pe)
	}
	if err.Error() != "een apparaat-id is nodig" {
		t.Fatalf("er zit iets omheen: %q", err.Error())
	}
}

// Een aanroep naar een dode peer moet meteen falen, niet hangen. Dat is de
// eigenschap waar de supervisor op leunt als een app-proces omvalt.
func TestCallFailsFastWhenPeerDies(t *testing.T) {
	x, y := net.Pipe()
	a := NewSession(NewConn(x), nil, nil)
	go a.Serve()

	blocked := make(chan struct{})
	go func() {
		_, err := a.Call(context.Background(), "traag", nil)
		if err == nil {
			t.Error("aanroep slaagde terwijl de peer weg was")
		}
		close(blocked)
	}()

	time.Sleep(20 * time.Millisecond)
	y.Close()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("aanroep bleef hangen nadat de peer wegviel")
	}
}

func TestCallRespectsContext(t *testing.T) {
	a, _ := pipe(t, nil, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		time.Sleep(time.Second)
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Call(ctx, "traag", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verwachtte deadline, kreeg %v", err)
	}
}

func TestCallDeadlineDoesNotCloseTheStreamMidFrame(t *testing.T) {
	stream := newBlockingStream()
	session := NewSession(NewConn(stream), nil, nil)
	go session.Serve()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Call(ctx, "peer-is-not-reading", nil)
		done <- err
	}()
	select {
	case <-stream.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not block")
	}

	// The caller's deadline passes while its frame is half out. Abandoning it
	// would leave a length prefix without a body, and closing the connection
	// would take every other call to this app with it -- so neither happens.
	select {
	case err := <-done:
		t.Fatalf("half-written frame was abandoned: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-stream.closed:
		t.Fatal("one caller's deadline closed the app connection")
	default:
	}

	// Closing the session is what releases it: the ping watcher does this for a
	// peer that went silent, and Stop does it for an app that is going away.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing the session did not release the blocked write")
	}
}

func TestCallDeadlineWhileWaitingForWriterKeepsStreamUsable(t *testing.T) {
	stream := newBlockingStream()
	session := NewSession(NewConn(stream), nil, nil)
	defer session.Close()
	go session.Serve()

	firstDone := make(chan error, 1)
	go func() {
		_, err := session.Call(context.Background(), "first-blocked-write", nil)
		firstDone <- err
	}()
	select {
	case <-stream.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("first write did not block")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := session.Call(ctx, "waiting-for-write-lock", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write returned %v, want context deadline", err)
	}
	select {
	case <-stream.closed:
		t.Fatal("a frame that wrote no bytes closed the stream")
	default:
	}

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not release the first writer")
	}
}

func TestPingBypassesBlockedApplicationHandler(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a, _ := pipe(t, nil, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method != "slow" {
			return nil, fmt.Errorf("protocol ping leaked into application handler as %q", method)
		}
		close(entered)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	callDone := make(chan error, 1)
	go func() {
		_, err := a.Call(context.Background(), "slow", nil)
		callDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("application handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Ping(ctx); err != nil {
		t.Fatalf("ping waited behind application handler: %v", err)
	}

	close(release)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
}

func TestCloseInterruptsBlockedWrite(t *testing.T) {
	stream := newBlockingStream()
	session := NewSession(NewConn(stream), nil, nil)
	go session.Serve()

	writeDone := make(chan error, 1)
	go func() {
		_, err := session.Call(context.Background(), "peer-is-not-reading", nil)
		writeDone <- err
	}()
	select {
	case <-stream.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("test write did not block")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close waited for the blocked writer")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("blocked writer survived Close")
	}
}

// Beide kanten mogen tegelijk requests sturen; ids zijn per verzender, dus die
// botsen niet.
func TestBidirectional(t *testing.T) {
	var a, b *Session
	a, b = pipe(t,
		func(_ context.Context, m string, _ json.RawMessage) (any, error) { return "van-a:" + m, nil },
		func(_ context.Context, m string, _ json.RawMessage) (any, error) { return "van-b:" + m, nil })

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); mustCall(t, a, "naar-b") }()
		go func() { defer wg.Done(); mustCall(t, b, "naar-a") }()
	}
	wg.Wait()
}

func TestRequestsRunOneAtATimeInArrivalOrder(t *testing.T) {
	x, y := net.Pipe()
	client := NewConn(x)
	var mu sync.Mutex
	var order []string
	server := NewSession(NewConn(y), func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "first" {
			time.Sleep(40 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, method)
		mu.Unlock()
		return method, nil
	}, nil)
	go server.Serve()
	defer server.Close()

	if err := client.WriteFrame(Frame{T: KindRequest, ID: 1, M: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(Frame{T: KindRequest, ID: 2, M: "second"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.ReadFrame(); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("requests ran in %v, not arrival order", order)
	}
}

func mustCall(t *testing.T, s *Session, method string) {
	t.Helper()
	raw, err := s.Call(context.Background(), method, nil)
	if err != nil {
		t.Error(err)
		return
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Error(err)
		return
	}
	if !strings.HasSuffix(got, method) {
		t.Errorf("antwoord %q hoort bij %q", got, method)
	}
}

// ---------------------------------------------------------------------------
// Framing: de harde grenzen
// ---------------------------------------------------------------------------

// Een te grote lengte-prefix mag geen geheugen reserveren en mag niet
// afgekapt worden -- het is een protocolfout die de verbinding beëindigt.
func TestOversizedPrefixIsRejectedWithoutAllocating(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 4_000_000_000)

	conn := NewConn(readOnly{bytes.NewReader(header[:])})
	_, err := conn.ReadFrame()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("verwachtte ErrFrameTooLarge, kreeg %v", err)
	}
}

func TestStricterRawLimitRejectsBeforeAllocating(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 1024)

	conn := NewConn(readOnly{bytes.NewReader(header[:])})
	_, err := conn.ReadRawLimit(128)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("stricter raw limit returned %v", err)
	}
}

func TestRequestQueueHasAByteBudget(t *testing.T) {
	session := NewSession(NewConn(discard{}), nil, nil)
	half := strings.Repeat("x", maxQueuedRequestBytes/2)
	if !session.enqueueRequest(Frame{M: "one", P: json.RawMessage(half)}) {
		t.Fatal("the first half-budget request did not fit")
	}
	if session.enqueueRequest(Frame{M: "two", P: json.RawMessage(half)}) {
		t.Fatal("method bytes let the request queue exceed its byte budget")
	}
}

// Een frame dat halverwege ophoudt is geen kortere boodschap maar een kapotte
// verbinding. Stilzwijgend doorgaan met de helft zou erger zijn dan stoppen.
func TestTruncatedFrameIsAnError(t *testing.T) {
	var buf bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 100)
	buf.Write(header[:])
	buf.WriteString(`{"t":"ev","m":"half`) // korter dan aangekondigd

	conn := NewConn(readOnly{bytes.NewReader(buf.Bytes())})
	_, err := conn.ReadFrame()
	if err == nil {
		t.Fatal("afgekapt frame werd geaccepteerd")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("verwachtte ErrUnexpectedEOF, kreeg %v", err)
	}
}

func TestWriteRefusesOversizedFrame(t *testing.T) {
	conn := NewConn(discard{})
	huge := strings.Repeat("x", MaxFrameSize+1)
	err := conn.WriteFrame(Frame{T: KindEvent, M: "log", P: json.RawMessage(`"` + huge + `"`)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("verwachtte ErrFrameTooLarge, kreeg %v", err)
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	var buf bytes.Buffer
	body := []byte(`{"t":"nonsense"}`)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	buf.Write(header[:])
	buf.Write(body)

	conn := NewConn(readOnly{bytes.NewReader(buf.Bytes())})
	if _, err := conn.ReadFrame(); err == nil {
		t.Fatal("onbekend frame-type werd geaccepteerd")
	}
}

// Een frame op precies de grens moet er wél doorheen: de cap is een grens, geen
// marge.
func TestFrameAtTheLimitRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(rwc{Reader: &buf, Writer: &buf})

	// Iets ruim onder de cap, zodat de JSON-omhulling er nog bij past.
	payload := strings.Repeat("y", MaxFrameSize-1024)
	if err := conn.WriteFrame(Frame{T: KindEvent, M: "log", P: mustJSON(t, payload)}); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := json.Unmarshal(frame.P, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("lengte %d, verwacht %d", len(got), len(payload))
	}
}

// Schrijven mag vanuit meerdere goroutines; frames mogen niet door elkaar lopen.
func TestConcurrentWritesStayIntact(t *testing.T) {
	var buf syncBuffer
	conn := NewConn(rwc{Reader: &buf, Writer: &buf})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := conn.WriteFrame(Frame{T: KindEvent, M: "log", P: mustJSON(t, strings.Repeat("z", n*37))}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	reader := NewConn(readOnly{&buf})
	for i := 0; i < 50; i++ {
		if _, err := reader.ReadFrame(); err != nil {
			t.Fatalf("frame %d onleesbaar: %v", i, err)
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type readOnly struct{ io.Reader }

func (readOnly) Write([]byte) (int, error) { return 0, errors.New("alleen lezen") }
func (readOnly) Close() error              { return nil }

type discard struct{}

func (discard) Read([]byte) (int, error)    { return 0, io.EOF }
func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Close() error                { return nil }

type rwc struct {
	io.Reader
	io.Writer
}

func (rwc) Close() error { return nil }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Read(p)
}

type blockingStream struct {
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingStream() *blockingStream {
	return &blockingStream{writeStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (s *blockingStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *blockingStream) Write([]byte) (int, error) {
	s.writeOnce.Do(func() { close(s.writeStarted) })
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *blockingStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// Een app handelt één verzoek tegelijk af, want de app-kant mag niet ineens
// gelijktijdig zijn. Precies daarom moet plumbing -- een leesactie op een
// ingebed bestand -- ernaast kunnen: anders staat een configuratiepagina achter
// een apparaat dat op het netwerk wacht.
func TestBesideQueueAnswersWhileHandlerIsBusy(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server, client := net.Pipe()
	session := NewSession(NewConn(server), func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "ui.asset" {
			return "asset", nil
		}
		close(entered)
		<-release
		return "slow", nil
	}, nil)
	session.AnswerBesideQueue("ui.asset")
	go session.Serve()
	defer session.Close()

	caller := NewSession(NewConn(client), nil, nil)
	go caller.Serve()
	defer caller.Close()

	slow := make(chan error, 1)
	go func() {
		_, err := caller.Call(context.Background(), "device.refresh", nil)
		slow <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("de trage handler kwam niet op gang")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := caller.Call(ctx, "ui.asset", map[string]any{"path": "settings/index.html"})
	if err != nil {
		t.Fatalf("ui.asset moest naast de rij kunnen: %v", err)
	}
	if string(result) != `"asset"` {
		t.Fatalf("ui.asset gaf %s", result)
	}

	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("het trage verzoek eindigde met %v", err)
	}
}

// Wat niet gemarkeerd is blijft in de rij staan: één lane voor app-werk is de
// hele afspraak met een app die niet gelijktijdig is.
func TestUnmarkedMethodStaysInTheQueue(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server, client := net.Pipe()
	session := NewSession(NewConn(server), func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "device.refresh" {
			close(entered)
			<-release
		}
		return method, nil
	}, nil)
	go session.Serve()
	defer session.Close()

	caller := NewSession(NewConn(client), nil, nil)
	go caller.Serve()
	defer caller.Close()

	go func() { _, _ = caller.Call(context.Background(), "device.refresh", nil) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("de trage handler kwam niet op gang")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := caller.Call(ctx, "ui.asset", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("een ongemarkeerde methode kwam er langs de rij: %v", err)
	}
	close(release)
}
