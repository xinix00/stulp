package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// De accept-sleutel uit RFC 6455 §1.3. Dit is de enige plek waar we tegen de
// specificatie zelf kunnen toetsen in plaats van tegen onze eigen aanname.
func TestAcceptKeyMatchesTheSpecification(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key = %q", got)
	}
}

// fakeServer doet de serverkant van de handshake en stuurt daarna frames die de
// test kiest.
func fakeServer(t *testing.T, serve func(conn net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		key := request.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + magic))
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
		serve(conn)
	}()
	return "ws://" + listener.Addr().String() + "/subscribe/devices"
}

// serverFrame bouwt een ongemaskeerd frame, zoals een server hoort te sturen.
func serverFrame(final bool, opcode byte, payload []byte) []byte {
	first := opcode
	if final {
		first |= 0x80
	}
	frame := []byte{first}
	switch length := len(payload); {
	case length < 126:
		frame = append(frame, byte(length))
	case length <= 0xFFFF:
		frame = append(frame, 126, byte(length>>8), byte(length))
	default:
		frame = append(frame, 127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		frame = append(frame, extended[:]...)
	}
	return append(frame, payload...)
}

func TestReadsATextMessage(t *testing.T) {
	address := fakeServer(t, func(conn net.Conn) {
		conn.Write(serverFrame(true, opText, []byte(`{"type":"update"}`)))
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := Dial(address, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != `{"type":"update"}` {
		t.Fatalf("bericht = %q", message)
	}
}

// Een bericht mag in stukken komen. Wie alleen het eerste frame leest krijgt
// een halve JSON en een parsefout die nergens naar wijst.
func TestJoinsFragments(t *testing.T) {
	address := fakeServer(t, func(conn net.Conn) {
		conn.Write(serverFrame(false, opText, []byte(`{"type":`)))
		conn.Write(serverFrame(false, opContinuation, []byte(`"upd`)))
		conn.Write(serverFrame(true, opContinuation, []byte(`ate"}`)))
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := Dial(address, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != `{"type":"update"}` {
		t.Fatalf("samengevoegd bericht = %q", message)
	}
}

// Een ping hoort beantwoord te worden zonder dat de aanroeper er iets van merkt.
func TestAnswersPingAndHidesIt(t *testing.T) {
	pong := make(chan []byte, 1)
	address := fakeServer(t, func(conn net.Conn) {
		conn.Write(serverFrame(true, opPing, []byte("hallo")))
		reader := bufio.NewReader(conn)
		head := make([]byte, 2)
		if _, err := io.ReadFull(reader, head); err != nil {
			return
		}
		length := int(head[1] & 0x7F)
		mask := make([]byte, 4)
		io.ReadFull(reader, mask)
		payload := make([]byte, length)
		io.ReadFull(reader, payload)
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		if head[0]&0x0F == opPong {
			pong <- payload
		}
		conn.Write(serverFrame(true, opText, []byte("daarna")))
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := Dial(address, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "daarna" {
		t.Fatalf("de ping lekte naar de aanroeper: %q", message)
	}
	select {
	case answered := <-pong:
		if string(answered) != "hallo" {
			t.Fatalf("pong droeg %q, wil hallo", answered)
		}
	case <-time.After(time.Second):
		t.Fatal("geen pong teruggestuurd")
	}
}

// Een close van de server is het einde, niet een fout waar iemand op moet gokken.
func TestServerCloseReadsAsEOF(t *testing.T) {
	address := fakeServer(t, func(conn net.Conn) {
		conn.Write(serverFrame(true, opClose, []byte{0x03, 0xE8}))
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := Dial(address, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Read(); err != io.EOF {
		t.Fatalf("fout bij sluiten = %v, wil EOF", err)
	}
}

// Een frame dat meer aankondigt dan toegestaan moet weigeren vóórdat er
// geheugen voor gereserveerd is.
func TestRefusesAnOversizedFrameBeforeAllocating(t *testing.T) {
	address := fakeServer(t, func(conn net.Conn) {
		frame := []byte{0x80 | opText, 127}
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], 1<<40)
		conn.Write(append(frame, extended[:]...))
		time.Sleep(50 * time.Millisecond)
	})
	conn, err := Dial(address, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Read()
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("te groot frame gaf %v", err)
	}
}

// Een server die 101 antwoordt maar de verkeerde sleutel teruggeeft is geen
// websocket-server. Dat hoort te falen en niet stil door te gaan.
func TestRefusesAWrongAcceptKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		http.ReadRequest(bufio.NewReader(conn))
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: dit-klopt-niet\r\n\r\n")
	}()
	_, err = Dial("ws://"+listener.Addr().String()+"/", Options{})
	if err == nil || !strings.Contains(err.Error(), "accept key") {
		t.Fatalf("verkeerde sleutel gaf %v", err)
	}
}

// De handshake draagt de headers die de aanroeper meegeeft; zonder de API-key
// weigert de console.
func TestHandshakeCarriesTheHeaders(t *testing.T) {
	seen := make(chan string, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		seen <- request.Header.Get("X-API-KEY") + " " + request.URL.Path
	}()
	header := http.Header{}
	header.Set("X-API-KEY", "geheim")
	Dial("ws://"+listener.Addr().String()+"/proxy/protect/integration/v1/subscribe/devices",
		Options{Header: header, Timeout: time.Second})
	select {
	case got := <-seen:
		if got != "geheim /proxy/protect/integration/v1/subscribe/devices" {
			t.Fatalf("handshake stuurde %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("geen handshake ontvangen")
	}
}
