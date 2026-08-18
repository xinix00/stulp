package rtsp

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeCamera doet de serverkant van RTSP: DESCRIBE, SETUP, PLAY, en daarna
// ingesloten RTP-pakketten. Genoeg om de hele weg te lopen zonder camera.
func fakeCamera(t *testing.T, packets [][]byte) string {
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
		for {
			method, sequence, err := readRequest(reader)
			if err != nil {
				return
			}
			switch method {
			case "DESCRIBE":
				body := sampleSDP
				io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+sequence+
					"\r\nContent-Type: application/sdp\r\nContent-Length: "+
					itoa(len(body))+"\r\n\r\n"+body)
			case "SETUP":
				io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+sequence+
					"\r\nSession: 12345678;timeout=60\r\nTransport: RTP/AVP/TCP;interleaved=0-1\r\n\r\n")
			case "PLAY":
				io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+sequence+"\r\nSession: 12345678\r\n\r\n")
				for _, packet := range packets {
					frame := []byte{'$', 0, byte(len(packet) >> 8), byte(len(packet))}
					conn.Write(append(frame, packet...))
				}
				time.Sleep(200 * time.Millisecond)
				return
			default:
				io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+sequence+"\r\n\r\n")
			}
		}
	}()
	return "rtsp://" + listener.Addr().String() + "/stream"
}

func readRequest(reader *bufio.Reader) (method, sequence string, err error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	method, _, _ = strings.Cut(strings.TrimSpace(line), " ")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return method, sequence, nil
		}
		if name, value, found := strings.Cut(line, ":"); found && strings.EqualFold(name, "CSeq") {
			sequence = strings.TrimSpace(value)
		}
	}
}

// rtp bouwt een pakket met een lading.
func rtp(timestamp uint32, marker bool, payload []byte) []byte {
	packet := make([]byte, 12, 12+len(payload))
	packet[0] = 0x80
	packet[1] = 96
	if marker {
		packet[1] |= 0x80
	}
	binary.BigEndian.PutUint32(packet[4:8], timestamp)
	return append(packet, payload...)
}

// De hele weg: verbinden, de beschrijving lezen, en frames terugkrijgen.
func TestDialReadsTheStream(t *testing.T) {
	address := fakeCamera(t, [][]byte{
		rtp(1000, false, []byte{0x67, 1, 2}),         // SPS los
		rtp(1000, true, []byte{0x65, 'b', 'e', 'e'}), // keyframe, einde
		rtp(4600, true, []byte{0x41, 'l', 'd'}),      // volgend frame
	})
	stream, err := Dial(address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if len(stream.SPS()) == 0 || len(stream.PPS()) == 0 {
		t.Fatal("de beschrijving leverde geen parametersets")
	}

	var assembler Assembler
	var frames int
	for frames < 2 {
		packet, err := stream.ReadPacket(2 * time.Second)
		if err != nil {
			break
		}
		if _, _, complete := assembler.Push(packet); complete {
			frames++
		}
	}
	if frames != 2 {
		t.Fatalf("%d frames uit de stroom, wil 2", frames)
	}
}

// Een camera met een codec die deze lezer niet kent hoort te falen bij het
// opzetten, met een melding die zegt wat hij wél kan -- niet later met een leeg
// beeld.
func TestDialRefusesAnUnsupportedCodec(t *testing.T) {
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
		reader := bufio.NewReader(conn)
		_, sequence, err := readRequest(reader)
		if err != nil {
			return
		}
		body := "v=0\nm=video 0 RTP/AVP 96\na=control:trackID=0\na=rtpmap:96 H265/90000\n"
		io.WriteString(conn, "RTSP/1.0 200 OK\r\nCSeq: "+sequence+
			"\r\nContent-Length: "+itoa(len(body))+"\r\n\r\n"+body)
		time.Sleep(100 * time.Millisecond)
	}()
	_, err = Dial("rtsp://"+listener.Addr().String()+"/stream", 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "H.264 and AV1") {
		t.Fatalf("een H.265-stream gaf %v", err)
	}
}
