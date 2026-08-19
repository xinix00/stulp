// Package rtsp haalt H.264 van een camera.
//
// Alleen wat nodig is om te kijken: DESCRIBE om te weten wat er is, SETUP en
// PLAY om het te krijgen, en RTP over dezelfde TCP-verbinding. Dat laatste is
// een keuze: RTP over UDP is de gewone weg, maar dan moet er een poort open en
// moet iemand nadenken over NAT. Over TCP werkt het overal, en een camera in
// huis is niet zo ver weg dat de extra bevestigingen iets kosten.
//
// Wat dit niet doet: opnamen terugspoelen, geluid, meerdere sporen, RTCP
// behalve het negeren ervan. Zie PORTED.md.
package rtsp

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxPacket begrenst één ingesloten pakket. RTP over TCP draagt een lengte van
// zestien bits, dus meer dan dit kan sowieso niet -- de controle staat er voor
// het geval de camera iets anders stuurt dan hij aankondigt.
const maxPacket = 0xFFFF

// Network reads do not need a buffer as large as the largest legal packet.
// bufio keeps filling a destination across reads, and large reads bypass its
// buffer. This one stays resident for the lifetime of every active camera.
const readBufferSize = 8 << 10

// Stream is een lopende verbinding met een camera.
type Stream struct {
	conn     net.Conn
	reader   *bufio.Reader
	session  string
	target   *url.URL
	auth     string
	sequence int

	// Media is wat de camera aankondigde: welke codec, welk spoor, en bij
	// H.264 de parametersets.
	Media Media
}

// SPS en PPS zijn er alleen bij H.264.
func (s *Stream) SPS() []byte { return s.Media.SPS }
func (s *Stream) PPS() []byte { return s.Media.PPS }

// Codec is wat er uit deze stroom komt.
func (s *Stream) Codec() Codec { return s.Media.Codec }

// Dial zet de verbinding op tot en met PLAY.
func Dial(address string, timeout time.Duration) (*Stream, error) {
	target, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("rtsp: %q is not a usable address: %w", address, err)
	}
	secure := false
	switch target.Scheme {
	case "rtsp":
	case "rtsps":
		secure = true
	default:
		return nil, fmt.Errorf("rtsp: %q is not rtsp or rtsps", target.Scheme)
	}
	host := target.Host
	if target.Port() == "" {
		if secure {
			host = net.JoinHostPort(host, "322")
		} else {
			host = net.JoinHostPort(host, "554")
		}
	}
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	var conn net.Conn
	dialer := &net.Dialer{Timeout: timeout}
	if secure {
		// Zelfde afweging als bij de API: een console geeft zichzelf een
		// certificaat uit dat door niets te verifiëren is.
		conn, err = tls.DialWithDialer(dialer, "tcp", host, &tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("rtsp: connect %s: %w", host, err)
	}

	stream := &Stream{conn: conn, reader: bufio.NewReaderSize(conn, readBufferSize), target: target}
	if user := target.User; user != nil {
		password, _ := user.Password()
		stream.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password))
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := stream.setup(); err != nil {
		conn.Close()
		return nil, err
	}
	// Het opzetten is klaar; wachten op beeld mag zo lang duren als het duurt.
	// Wel een leesdeadline per pakket, hieronder.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return stream, nil
}

func (s *Stream) setup() error {
	describe, body, err := s.request("DESCRIBE", s.target.String(), map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return err
	}
	if describe != 200 {
		return fmt.Errorf("rtsp: DESCRIBE answered %d", describe)
	}
	media, err := parseSDP(body)
	if err != nil {
		return err
	}
	s.Media = media

	// Interleaved: kanaal 0 draagt RTP, kanaal 1 RTCP. Alles over dezelfde
	// verbinding, dus er hoeft niets open te staan.
	code, _, err := s.request("SETUP", s.control(media.Control), map[string]string{
		"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1",
	})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtsp: SETUP answered %d", code)
	}
	code, _, err = s.request("PLAY", s.target.String(), nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("rtsp: PLAY answered %d", code)
	}
	return nil
}

// control bouwt de URL van het spoor. Een absolute a=control gaat voor; anders
// hangt hij achter de stream-URL.
func (s *Stream) control(media string) string {
	if media == "" {
		return s.target.String()
	}
	if strings.HasPrefix(media, "rtsp://") || strings.HasPrefix(media, "rtsps://") {
		return media
	}
	base := s.target.String()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimPrefix(media, "/")
}

func (s *Stream) request(method, target string, headers map[string]string) (int, string, error) {
	s.sequence++
	var request strings.Builder
	fmt.Fprintf(&request, "%s %s RTSP/1.0\r\n", method, target)
	fmt.Fprintf(&request, "CSeq: %d\r\n", s.sequence)
	request.WriteString("User-Agent: Stulp\r\n")
	if s.session != "" {
		fmt.Fprintf(&request, "Session: %s\r\n", s.session)
	}
	if s.auth != "" {
		fmt.Fprintf(&request, "Authorization: %s\r\n", s.auth)
	}
	for name, value := range headers {
		fmt.Fprintf(&request, "%s: %s\r\n", name, value)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(s.conn, request.String()); err != nil {
		return 0, "", fmt.Errorf("rtsp: send %s: %w", method, err)
	}
	return s.readResponse(method)
}

func (s *Stream) readResponse(method string) (int, string, error) {
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return 0, "", fmt.Errorf("rtsp: read %s: %w", method, err)
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return 0, "", fmt.Errorf("rtsp: %s got %q, which is not an RTSP answer", method, strings.TrimSpace(line))
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", fmt.Errorf("rtsp: %s got status %q", method, parts[1])
	}

	length := 0
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(name) {
		case "session":
			// De sessie kan een timeout achter een puntkomma dragen.
			s.session, _, _ = strings.Cut(value, ";")
			s.session = strings.TrimSpace(s.session)
		case "content-length":
			length, _ = strconv.Atoi(value)
		}
	}
	if length <= 0 {
		return code, "", nil
	}
	if length > maxPacket {
		return 0, "", fmt.Errorf("rtsp: %s announced a body of %d bytes", method, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(s.reader, body); err != nil {
		return 0, "", err
	}
	return code, string(body), nil
}

// Packet is één RTP-pakket met beeld.
type Packet struct {
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// ReadPacket levert het volgende RTP-pakket van het videokanaal.
//
// RTCP en alles op andere kanalen wordt overgeslagen: die dragen statistiek en
// daar kijkt niemand naar.
func (s *Stream) ReadPacket(timeout time.Duration) (Packet, error) {
	for {
		if timeout > 0 {
			if err := s.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				return Packet{}, err
			}
		}
		magic, err := s.reader.ReadByte()
		if err != nil {
			return Packet{}, err
		}
		if magic != '$' {
			// Een RTSP-antwoord tussen de pakketten door -- een keepalive die
			// beantwoord wordt. Overslaan tot de volgende $.
			if err := s.skipTextResponse(magic); err != nil {
				return Packet{}, err
			}
			continue
		}
		var head [3]byte
		if _, err := io.ReadFull(s.reader, head[:]); err != nil {
			return Packet{}, err
		}
		channel := head[0]
		length := int(binary.BigEndian.Uint16(head[1:]))
		if length > maxPacket {
			return Packet{}, fmt.Errorf("rtsp: interleaved frame announces %d bytes", length)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(s.reader, body); err != nil {
			return Packet{}, err
		}
		if channel != 0 {
			continue
		}
		packet, ok := parseRTP(body)
		if !ok {
			continue
		}
		return packet, nil
	}
}

// skipTextResponse leest een RTSP-antwoord weg dat tussen de pakketten staat.
func (s *Stream) skipTextResponse(first byte) error {
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(first)+line, "RTSP/") {
		return fmt.Errorf("rtsp: unexpected byte 0x%02X in the stream", first)
	}
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			return nil
		}
	}
}

// Keepalive houdt de sessie open. Een camera hangt op als er een tijd lang niets
// van de kant van de client komt, ook al stuurt hij zelf beeld.
func (s *Stream) Keepalive() error {
	s.sequence++
	request := fmt.Sprintf("OPTIONS %s RTSP/1.0\r\nCSeq: %d\r\nSession: %s\r\n\r\n",
		s.target.String(), s.sequence, s.session)
	if err := s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer s.conn.SetWriteDeadline(time.Time{})
	_, err := io.WriteString(s.conn, request)
	return err
}

func (s *Stream) Close() error { return s.conn.Close() }

// parseRTP haalt de nuttige lading uit een RTP-pakket.
func parseRTP(packet []byte) (Packet, bool) {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return Packet{}, false
	}
	csrc := int(packet[0] & 0x0F)
	extension := packet[0]&0x10 != 0
	offset := 12 + csrc*4
	if len(packet) < offset {
		return Packet{}, false
	}
	if extension {
		if len(packet) < offset+4 {
			return Packet{}, false
		}
		words := int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		offset += 4 + words*4
		if len(packet) < offset {
			return Packet{}, false
		}
	}
	return Packet{
		Timestamp: binary.BigEndian.Uint32(packet[4:8]),
		Marker:    packet[1]&0x80 != 0,
		Payload:   packet[offset:],
	}, true
}

// describe haalt alleen de beschrijving op en stopt daarna. Bedoeld om te zien
// wat een camera aankondigt zonder er een stroom bij op te zetten.
func describe(address string, timeout time.Duration) (string, error) {
	target, err := url.Parse(address)
	if err != nil {
		return "", err
	}
	host := target.Host
	if target.Port() == "" {
		host = net.JoinHostPort(host, "322")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", host,
		&tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	stream := &Stream{conn: conn, reader: bufio.NewReaderSize(conn, readBufferSize), target: target}
	_, body, err := stream.request("DESCRIBE", target.String(), map[string]string{"Accept": "application/sdp"})
	return body, err
}
