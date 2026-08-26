// Package modbus is een Modbus/TCP-client met alleen wat deze app nodig heeft.
//
// Modbus/TCP is een klein binair protocol. Elk bericht begint met zeven bytes
// MBAP-header -- transactie-id, protocol-id, lengte, unit-id -- en daarachter
// staat een PDU die begint met een functiecode. Deze client kent de vier wegen
// die de Sigenergy-registerkaart gebruikt: 0x03 voor holding registers, 0x04
// voor read-only input registers, 0x06 voor een enkel holding register en 0x10
// voor meerdere holding registers.
//
// De client staat hier omdat deze app hem nodig heeft, niet omdat Stulp hem
// aanbiedt. Een installatie zonder Sigenergy draagt hem dus niet.
//
// Wat er begrensd wordt en waarom staat bij de constanten hieronder. Kort: aan
// de andere kant zit firmware die wij niet schreven, en een lengte die zij
// aankondigt is een belofte en geen feit.
package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	// headerSize is de MBAP-header: transactie(2) protocol(2) lengte(2) unit(1).
	headerSize = 7

	// maxPDU is wat Modbus zelf toestaat: 253 bytes protocoldata. Het lengteveld
	// telt de unit-id mee, dus dat veld mag er één meer aankondigen.
	maxPDU         = 253
	maxLengthField = maxPDU + 1

	// MaxReadRegisters en MaxWriteRegisters volgen uit die 253 bytes. Ze staan
	// hier als getal omdat een vraag die te groot is hier hoort te stranden en
	// niet bij het apparaat: een omvormer die 200 registers krijgt gevraagd
	// antwoordt met een uitzondering, en dan weet je niet of het aan het adres
	// lag of aan het aantal.
	MaxReadRegisters  = 125
	MaxWriteRegisters = 123

	// protocolID is nul voor Modbus. Iets anders betekent dat er geen
	// Modbus-server aan de lijn hangt.
	protocolID = 0

	functionReadHolding   = 0x03
	functionReadInput     = 0x04
	functionWriteSingle   = 0x06
	functionWriteMultiple = 0x10

	// exceptionBit staat aan in de functiecode van een foutantwoord.
	exceptionBit = 0x80
)

// Exception is een foutantwoord van het apparaat: de vraag kwam aan, en het
// apparaat weigert hem.
//
// Dit is met opzet een eigen type. Voor de aanroeper is het verschil met een
// kapotte lijn wezenlijk: een uitzondering betekent dat deze unit of dit adres
// niet bestaat en dat de verbinding gewoon nog staat, en dat is precies wat het
// aftasten naar unit-ids nodig heeft om door te kunnen lopen.
type Exception struct {
	Function byte
	Code     byte
}

func (e Exception) Error() string {
	return fmt.Sprintf("the device refused function 0x%02X: %s", e.Function, exceptionText(e.Code))
}

// exceptionText komt uit lib/modbus/utils.js van de bron. De teksten zijn de
// standaardbetekenissen; code 10 draagt de aanwijzing die de bron erbij zette,
// want dat is bij Sigenergy in de praktijk de melding voor een verkeerd unit-id.
func exceptionText(code byte) string {
	switch code {
	case 1:
		return "illegal function (this device does not know this function code)"
	case 2:
		return "illegal data address (this address does not exist on this device)"
	case 3:
		return "illegal data value (this device does not accept this value)"
	case 4:
		return "device failure (the device broke off while carrying out the request)"
	case 5:
		return "acknowledge (the device accepted the request but needs longer)"
	case 6:
		return "device busy (the device is still working on an earlier request)"
	case 8:
		return "memory parity error"
	case 10:
		return "gateway path unavailable (usually the wrong unit id for this device type)"
	case 11:
		return "gateway target device failed to respond"
	}
	return "unknown exception code " + strconv.Itoa(int(code))
}

// Client is één Modbus/TCP-verbinding naar één adres.
//
// Alle units achter dat adres delen hem: het unit-id zit in elk bericht, dus
// één socket kan de omvormer, de batterij en het systeem bedienen. Dat scheelt
// niet alleen sockets -- een Sigenergy-systeem staat maar een handvol
// gelijktijdige verbindingen toe.
//
// Veilig vanaf elke goroutine, en één vraag tegelijk.
type Client struct {
	address string
	timeout time.Duration

	// Modbus/TCP staat toe om meerdere vragen tegelijk open te hebben; het
	// transactie-id is er juist voor om de antwoorden weer te sorteren. Deze
	// client doet dat niet, omdat er niets is dat erop wacht: het pollen loopt
	// toch op één ritme, en één vraag tegelijk maakt het toetsen van dat
	// transactie-id sluitend in plaats van een aanwijzing.
	mu          sync.Mutex
	conn        net.Conn
	transaction uint16
	closed      bool
}

// New maakt een client. Er wordt nog niet verbonden: dat gebeurt bij de eerste
// vraag, zodat een apparaat dat uit staat de start van de app niet ophoudt.
func New(host string, port int, timeout time.Duration) *Client {
	if port == 0 {
		port = 502
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{address: net.JoinHostPort(host, strconv.Itoa(port)), timeout: timeout}
}

// Address is waar deze client naartoe praat, voor in een melding.
func (c *Client) Address() string { return c.address }

// Close sluit de verbinding en zet de client stil. Een client die gesloten is
// gaat niet vanzelf weer open: wie opnieuw wil verbinden maakt een nieuwe.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.drop()
}

// ReadHolding leest count holding registers vanaf start met functiecode 0x03.
func (c *Client) ReadHolding(unit uint8, start, count uint16) ([]uint16, error) {
	return c.readRegisters(unit, start, count, functionReadHolding, "holding")
}

// ReadInput leest count input registers vanaf start met de standaard
// functiecode 0x04. De Sigenergy-registerkaart gebruikt deze route bewust niet:
// de fabrikant schrijft voor zijn 30000- én leesbare 40000-registers 0x03 voor.
// De methode blijft beschikbaar voor protocoltests en eventuele andere kaarten.
func (c *Client) ReadInput(unit uint8, start, count uint16) ([]uint16, error) {
	return c.readRegisters(unit, start, count, functionReadInput, "input")
}

func (c *Client) readRegisters(unit uint8, start, count uint16, function byte, area string) ([]uint16, error) {
	if count == 0 || count > MaxReadRegisters {
		return nil, fmt.Errorf("modbus: asked for %d %s registers at %d; a read carries 1 to %d",
			count, area, start, MaxReadRegisters)
	}
	request := make([]byte, 4)
	binary.BigEndian.PutUint16(request[0:2], start)
	binary.BigEndian.PutUint16(request[2:4], count)

	answer, err := c.transact(unit, function, request)
	if err != nil {
		return nil, fmt.Errorf("modbus: read %d %s registers at %d on unit %d: %w", count, area, start, unit, err)
	}
	// Het bytegetal is wat het apparaat zegt te sturen; de rest van het antwoord
	// is wat het stuurde. Als die twee niet kloppen is het antwoord niet te
	// vertrouwen, en dan is stoppen beter dan de helft van een meting melden.
	if len(answer) < 1 {
		return nil, fmt.Errorf("modbus: read %d %s registers at %d on unit %d: the answer was empty", count, area, start, unit)
	}
	if got, want := int(answer[0]), int(count)*2; got != want || len(answer)-1 != want {
		return nil, fmt.Errorf("modbus: read %d %s registers at %d on unit %d: the answer announces %d bytes and carries %d, expected %d",
			count, area, start, unit, got, len(answer)-1, want)
	}
	values := make([]uint16, count)
	for i := range values {
		values[i] = binary.BigEndian.Uint16(answer[1+i*2 : 3+i*2])
	}
	return values, nil
}

// WriteSingle schrijft één holding register met functiecode 0x06.
//
// Sigenergy markeert sommige opdrachtregisters als write-only en schrijft voor
// die enkele waarden expliciet 0x06 voor. Zo'n opdracht als een 0x10-blok
// versturen wordt niet door iedere firmware aangenomen.
func (c *Client) WriteSingle(unit uint8, start, value uint16) error {
	request := make([]byte, 4)
	binary.BigEndian.PutUint16(request[0:2], start)
	binary.BigEndian.PutUint16(request[2:4], value)

	answer, err := c.transact(unit, functionWriteSingle, request)
	if err != nil {
		return fmt.Errorf("modbus: write register at %d on unit %d: %w", start, unit, err)
	}
	// 0x06 bevestigt de opdracht door adres en waarde letterlijk te herhalen.
	if len(answer) != 4 {
		return fmt.Errorf("modbus: write register at %d on unit %d: the confirmation is %d bytes, expected 4",
			start, unit, len(answer))
	}
	gotStart := binary.BigEndian.Uint16(answer[0:2])
	gotValue := binary.BigEndian.Uint16(answer[2:4])
	if gotStart != start || gotValue != value {
		return fmt.Errorf("modbus: write register at %d on unit %d with value %d: the device confirms register %d with value %d",
			start, unit, value, gotStart, gotValue)
	}
	return nil
}

// WriteHolding schrijft registers vanaf start, met functiecode 0x10.
//
// De methode aanvaardt ook één waarde voor apparaten die juist 0x10
// voorschrijven. Een Sigenergy-opdrachtregister dat 0x06 verlangt loopt via
// WriteSingle.
func (c *Client) WriteHolding(unit uint8, start uint16, values []uint16) error {
	if len(values) == 0 || len(values) > MaxWriteRegisters {
		return fmt.Errorf("modbus: asked to write %d registers at %d; a write carries 1 to %d",
			len(values), start, MaxWriteRegisters)
	}
	request := make([]byte, 5+len(values)*2)
	binary.BigEndian.PutUint16(request[0:2], start)
	binary.BigEndian.PutUint16(request[2:4], uint16(len(values)))
	request[4] = byte(len(values) * 2)
	for i, value := range values {
		binary.BigEndian.PutUint16(request[5+i*2:7+i*2], value)
	}

	answer, err := c.transact(unit, functionWriteMultiple, request)
	if err != nil {
		return fmt.Errorf("modbus: write %d registers at %d on unit %d: %w", len(values), start, unit, err)
	}
	// Het apparaat herhaalt adres en aantal. Dat is de enige bevestiging die
	// Modbus geeft dat het geschreven heeft wat er gevraagd werd, dus die wordt
	// getoetst en niet weggegooid.
	if len(answer) != 4 {
		return fmt.Errorf("modbus: write %d registers at %d on unit %d: the confirmation is %d bytes, expected 4",
			len(values), start, unit, len(answer))
	}
	gotStart := binary.BigEndian.Uint16(answer[0:2])
	gotCount := binary.BigEndian.Uint16(answer[2:4])
	if gotStart != start || int(gotCount) != len(values) {
		return fmt.Errorf("modbus: write %d registers at %d on unit %d: the device confirms %d registers at %d",
			len(values), start, unit, gotCount, gotStart)
	}
	return nil
}

// transact stuurt één vraag en levert de PDU van het antwoord zonder de
// functiecode.
//
// Elke fout hier sluit de verbinding. Dat is geen voorzichtigheid maar het
// enige wat klopt: na een half gelezen antwoord staat er nog van alles in de
// stroom, en de volgende vraag zou dat als haar eigen antwoord lezen. Een
// Exception sluit hem juist níet -- dat is een compleet en netjes antwoord, en
// de lijn is nog in de pas.
func (c *Client) transact(unit uint8, function byte, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, errors.New("this connection is closed")
	}
	conn, err := c.connect()
	if err != nil {
		return nil, err
	}

	c.transaction++
	wanted := c.transaction

	frame := make([]byte, headerSize+1+len(payload))
	binary.BigEndian.PutUint16(frame[0:2], wanted)
	binary.BigEndian.PutUint16(frame[2:4], protocolID)
	binary.BigEndian.PutUint16(frame[4:6], uint16(2+len(payload))) // unit + functiecode + rest
	frame[6] = unit
	frame[7] = function
	copy(frame[8:], payload)

	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		c.drop()
		return nil, err
	}
	if _, err := conn.Write(frame); err != nil {
		c.drop()
		return nil, fmt.Errorf("sending the request failed: %w", err)
	}

	var header [headerSize]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		c.drop()
		return nil, fmt.Errorf("no answer: %w", err)
	}
	transaction := binary.BigEndian.Uint16(header[0:2])
	protocol := binary.BigEndian.Uint16(header[2:4])
	length := binary.BigEndian.Uint16(header[4:6])
	answeringUnit := header[6]

	if protocol != protocolID {
		c.drop()
		return nil, fmt.Errorf("the answer carries protocol id %d; Modbus is 0", protocol)
	}
	// Toetsen vóór het reserveren. Een tegenpartij die 65535 aankondigt mag geen
	// geheugen kosten om te kunnen weigeren, en zo'n lengte kán ook niet kloppen:
	// Modbus draagt hoogstens 253 bytes per bericht.
	if length < 2 || length > maxLengthField {
		c.drop()
		return nil, fmt.Errorf("the answer announces %d bytes; Modbus carries 2 to %d", length, maxLengthField)
	}
	// Het transactie-id hoort bij de vraag. Zonder deze toets leest een hapering
	// -- een antwoord dat te laat kwam en bleef staan -- als het antwoord op de
	// volgende vraag, en dan staat er een vermogen op de tegel van de SoC.
	if transaction != wanted {
		c.drop()
		return nil, fmt.Errorf("the answer belongs to request %d, not to %d", transaction, wanted)
	}
	if answeringUnit != unit {
		c.drop()
		return nil, fmt.Errorf("unit %d answered a question for unit %d", answeringUnit, unit)
	}

	body := make([]byte, length-1) // de unit-id is al gelezen
	if _, err := io.ReadFull(conn, body); err != nil {
		c.drop()
		return nil, fmt.Errorf("the answer broke off: %w", err)
	}

	switch answered := body[0]; {
	case answered == function|exceptionBit:
		// Een uitzondering is een fout met een reden, geen lege waarde. De
		// verbinding blijft staan: dit antwoord was compleet.
		if len(body) < 2 {
			return nil, fmt.Errorf("the device refused function 0x%02X without saying why", function)
		}
		return nil, Exception{Function: function, Code: body[1]}
	case answered != function:
		c.drop()
		return nil, fmt.Errorf("the answer carries function 0x%02X; 0x%02X was asked", answered, function)
	}
	return body[1:], nil
}

// connect levert de verbinding en zet hem op als hij er niet is. Aanroepen met
// de lock vast.
func (c *Client) connect() (net.Conn, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	conn, err := net.DialTimeout("tcp", c.address, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s failed: %w", c.address, err)
	}
	c.conn = conn
	return conn, nil
}

// drop gooit de verbinding weg, zodat de volgende vraag opnieuw verbindt.
// Aanroepen met de lock vast.
func (c *Client) drop() error {
	conn := c.conn
	c.conn = nil
	if conn == nil {
		return nil
	}
	return conn.Close()
}
