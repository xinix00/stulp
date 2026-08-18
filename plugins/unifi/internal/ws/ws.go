// Package ws is een websocket-client, alleen wat een luisteraar nodig heeft.
//
// De console duwt gebeurtenissen naar ons en verwacht verder niets terug dan
// een pong. Dat is een klein deel van RFC 6455, en het staat hier omdat deze
// plugin het nodig heeft -- niet omdat Stulp het aanbiedt. Een app die geen
// websocket praat linkt hem niet.
//
// Wat dit doet: de handshake, frames lezen (ook opgedeeld over meerdere
// fragmenten), pings beantwoorden, en netjes sluiten. Wat het niet doet:
// compressie (per-message-deflate), uitbreidingen, en de serverkant.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// magic is de vaste waarde uit RFC 6455 waarmee de server zijn antwoord tekent.
const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxMessage begrenst één bericht. Een console die meer stuurt is stuk of niet
// van ons, en dan is stoppen beter dan geheugen vullen.
const maxMessage = 8 << 20

// Opcodes, voor zover deze client ze kent.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Conn is een open verbinding.
type Conn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Options zijn de keuzes bij het opzetten.
type Options struct {
	// Header gaat mee in de handshake. Hier zet je je API-key.
	Header http.Header
	// TLS is de instelling voor wss. Nul betekent de standaard van Go, die het
	// zelfondertekende certificaat van een console zal weigeren.
	TLS *tls.Config
	// Timeout geldt voor het opzetten, niet voor het luisteren daarna.
	Timeout time.Duration
}

// Dial opent een verbinding naar een ws:// of wss://-adres.
func Dial(address string, options Options) (*Conn, error) {
	target, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("websocket: %q is not a usable address: %w", address, err)
	}
	secure := false
	switch target.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("websocket: %q is not ws or wss", target.Scheme)
	}
	host := target.Host
	if target.Port() == "" {
		if secure {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}

	var raw net.Conn
	dialer := &net.Dialer{Timeout: timeout}
	if secure {
		raw, err = tls.DialWithDialer(dialer, "tcp", host, options.TLS)
	} else {
		raw, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("websocket: connect %s: %w", host, err)
	}

	if err := raw.SetDeadline(time.Now().Add(timeout)); err != nil {
		raw.Close()
		return nil, err
	}
	key, err := handshake(raw, target, options.Header)
	if err != nil {
		raw.Close()
		return nil, err
	}
	reader := bufio.NewReader(raw)
	if err := readHandshake(reader, key); err != nil {
		raw.Close()
		return nil, err
	}
	// De opzet is klaar; luisteren mag zo lang duren als het duurt.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		raw.Close()
		return nil, err
	}
	return &Conn{conn: raw, reader: reader}, nil
}

func handshake(conn net.Conn, target *url.URL, header http.Header) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	path := target.RequestURI()
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&request, "Host: %s\r\n", target.Host)
	request.WriteString("Upgrade: websocket\r\n")
	request.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\n", key)
	request.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, values := range header {
		for _, value := range values {
			fmt.Fprintf(&request, "%s: %s\r\n", name, value)
		}
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return "", fmt.Errorf("websocket: send handshake: %w", err)
	}
	return key, nil
}

// readHandshake toetst het antwoord, inclusief de accept-sleutel.
//
// Die controle is niet ceremonieel: hij bewijst dat er een websocket-server aan
// de andere kant zit en niet een proxy of een inlogpagina die vrolijk 101
// antwoordt op alles.
func readHandshake(reader *bufio.Reader, key string) error {
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return fmt.Errorf("websocket: read handshake: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		excerpt := strings.TrimSpace(string(body))
		if excerpt != "" {
			excerpt = ": " + excerpt
		}
		return fmt.Errorf("websocket: server answered %s%s", response.Status, excerpt)
	}
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		return errors.New("websocket: server did not upgrade the connection")
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		return errors.New("websocket: server returned the wrong accept key")
	}
	return nil
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Read levert het volgende volledige bericht.
//
// Pings worden onderweg beantwoord en pongs overgeslagen, zodat de aanroeper
// alleen ziet wat de toepassing aangaat. Een close van de server komt terug als
// io.EOF.
func (c *Conn) Read() ([]byte, error) {
	var message []byte
	var messageOpcode byte
	for {
		final, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.write(opClose, payload)
			return nil, io.EOF
		case opText, opBinary:
			message = payload
			messageOpcode = opcode
		case opContinuation:
			if messageOpcode == 0 {
				return nil, errors.New("websocket: continuation without a message to continue")
			}
			message = append(message, payload...)
		default:
			return nil, fmt.Errorf("websocket: unknown opcode 0x%X", opcode)
		}
		if len(message) > maxMessage {
			return nil, fmt.Errorf("websocket: message exceeds %d bytes", maxMessage)
		}
		if final {
			return message, nil
		}
	}
}

func (c *Conn) readFrame() (final bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.reader, head[:]); err != nil {
		return false, 0, nil, err
	}
	final = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	switch length {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	// Toetsen vóór het reserveren: een lengte van vier gigabyte mag geen vier
	// gigabyte geheugen kosten om te kunnen weigeren.
	if length > maxMessage {
		return false, 0, nil, fmt.Errorf("websocket: frame announces %d bytes, more than the %d allowed", length, maxMessage)
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.reader, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return final, opcode, payload, nil
}

// Ping stuurt een ping. Zonder dit merkt niemand een verbinding die dood is
// zonder gesloten te zijn -- TCP ziet dat namelijk ook niet.
func (c *Conn) Ping() error { return c.write(opPing, nil) }

// Send stuurt een tekstbericht.
func (c *Conn) Send(message []byte) error { return c.write(opText, message) }

// write stuurt één frame. Een client maskeert altijd; dat schrijft RFC 6455
// voor en een server die het niet ziet verbreekt de verbinding.
func (c *Conn) write(opcode byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80|length))
	case length <= 0xFFFF:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		header = append(header, extended[:]...)
	}
	header = append(header, mask[:]...)

	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer c.conn.SetWriteDeadline(time.Time{})
	if _, err := c.conn.Write(append(header, masked...)); err != nil {
		return fmt.Errorf("websocket: write: %w", err)
	}
	return nil
}

// Close sluit de verbinding, met een afscheid als dat nog lukt.
func (c *Conn) Close() error {
	_ = c.write(opClose, []byte{0x03, 0xE8}) // 1000: normale sluiting
	return c.conn.Close()
}
