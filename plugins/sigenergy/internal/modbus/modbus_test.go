package modbus

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// device is een nagebouwde Modbus/TCP-server. Hij leest een compleet bericht en
// laat de test bepalen wat er teruggaat -- ook iets wat een echt apparaat nooit
// zou sturen, want juist daar gaat het hier om.
type device struct {
	listener net.Listener
	seen     chan request
}

type request struct {
	transaction uint16
	unit        uint8
	function    byte
	payload     []byte
}

// newDevice start de server. reply krijgt elk binnengekomen bericht en levert de
// rauwe bytes van het antwoord; nil betekent stil blijven.
func newDevice(t *testing.T, reply func(request) []byte) *device {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &device{listener: listener, seen: make(chan request, 8)}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for {
					var header [headerSize]byte
					if _, err := io.ReadFull(conn, header[:]); err != nil {
						return
					}
					length := binary.BigEndian.Uint16(header[4:6])
					body := make([]byte, length-1)
					if _, err := io.ReadFull(conn, body); err != nil {
						return
					}
					incoming := request{
						transaction: binary.BigEndian.Uint16(header[0:2]),
						unit:        header[6],
						function:    body[0],
						payload:     body[1:],
					}
					select {
					case d.seen <- incoming:
					default:
					}
					answer := reply(incoming)
					if answer == nil {
						return
					}
					if _, err := conn.Write(answer); err != nil {
						return
					}
				}
			}()
		}
	}()
	return d
}

func (d *device) client(t *testing.T) *Client {
	t.Helper()
	host, port, err := net.SplitHostPort(d.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	client := New(host, number, 2*time.Second)
	t.Cleanup(func() { client.Close() })
	return client
}

// frame bouwt een goed gevormd antwoord.
func frame(transaction uint16, unit uint8, pdu []byte) []byte {
	out := make([]byte, headerSize+len(pdu))
	binary.BigEndian.PutUint16(out[0:2], transaction)
	binary.BigEndian.PutUint16(out[2:4], protocolID)
	binary.BigEndian.PutUint16(out[4:6], uint16(1+len(pdu)))
	out[6] = unit
	copy(out[7:], pdu)
	return out
}

// registers bouwt de PDU van een geslaagde leesactie.
func registers(function byte, values ...uint16) []byte {
	pdu := []byte{function, byte(len(values) * 2)}
	for _, value := range values {
		pdu = binary.BigEndian.AppendUint16(pdu, value)
	}
	return pdu
}

func TestReadsHoldingRegisters(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, registers(functionReadHolding, 0x0001, 0x86A0))
	})
	values, err := d.client(t).ReadHolding(247, 30005, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != 0x0001 || values[1] != 0x86A0 {
		t.Fatalf("registers = %v", values)
	}
	got := <-d.seen
	if got.unit != 247 || got.function != functionReadHolding {
		t.Fatalf("de vraag ging als unit %d functie 0x%02X de deur uit", got.unit, got.function)
	}
	if start := binary.BigEndian.Uint16(got.payload[0:2]); start != 30005 {
		t.Fatalf("gevraagd adres = %d", start)
	}
	if count := binary.BigEndian.Uint16(got.payload[2:4]); count != 2 {
		t.Fatalf("gevraagd aantal = %d", count)
	}
}

func TestReadsInputRegistersWithFunction04(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, registers(functionReadInput, 4))
	})
	values, err := d.client(t).ReadInput(2, 32000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != 4 {
		t.Fatalf("registers = %v", values)
	}
	got := <-d.seen
	if got.unit != 2 || got.function != functionReadInput {
		t.Fatalf("de vraag ging als unit %d functie 0x%02X de deur uit", got.unit, got.function)
	}
	if start := binary.BigEndian.Uint16(got.payload[0:2]); start != 32000 {
		t.Fatalf("gevraagd adres = %d", start)
	}
}

// Een uitzondering is een fout met een leesbare reden. Wie hier een lege waarde
// van maakt zet een nul op de tegel waar geen meting was.
func TestExceptionIsAnErrorWithAReason(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, []byte{functionReadHolding | exceptionBit, 2})
	})
	_, err := d.client(t).ReadHolding(1, 40000, 1)
	if err == nil {
		t.Fatal("een uitzondering leverde geen fout")
	}
	var exception Exception
	if !errors.As(err, &exception) {
		t.Fatalf("fout = %v, wil een Exception", err)
	}
	if exception.Code != 2 {
		t.Fatalf("uitzonderingscode = %d", exception.Code)
	}
	if !strings.Contains(err.Error(), "illegal data address") {
		t.Fatalf("de melding zegt niet wat er mis is: %v", err)
	}
}

// Een uitzondering is een compleet antwoord, dus de verbinding blijft bruikbaar.
// Dat is wat het aftasten naar unit-ids nodig heeft: elke misser mag geen nieuwe
// verbinding kosten.
func TestExceptionKeepsTheConnectionUsable(t *testing.T) {
	answers := 0
	d := newDevice(t, func(in request) []byte {
		answers++
		if answers == 1 {
			return frame(in.transaction, in.unit, []byte{functionReadHolding | exceptionBit, 10})
		}
		return frame(in.transaction, in.unit, registers(functionReadHolding, 42))
	})
	client := d.client(t)
	if _, err := client.ReadHolding(3, 30005, 1); err == nil {
		t.Fatal("de eerste vraag hoorde te falen")
	}
	values, err := client.ReadHolding(1, 30005, 1)
	if err != nil {
		t.Fatalf("na een uitzondering werkte de verbinding niet meer: %v", err)
	}
	if values[0] != 42 {
		t.Fatalf("register = %v", values)
	}
}

// Een antwoord dat bij een eerdere vraag hoort mag nooit als dit antwoord
// doorgaan. Zonder deze toets lees je bij een hapering het vorige vermogen als
// het huidige.
func TestRefusesAnAnswerWithTheWrongTransactionID(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction+1, in.unit, registers(functionReadHolding, 7))
	})
	_, err := d.client(t).ReadHolding(1, 30005, 1)
	if err == nil || !strings.Contains(err.Error(), "belongs to request") {
		t.Fatalf("verkeerd transactie-id gaf %v", err)
	}
}

// Een aangekondigde lengte wordt getoetst vóórdat er geheugen voor gereserveerd
// wordt. Een tegenpartij die 65535 bytes belooft mag geen geheugen kosten om te
// kunnen weigeren -- en zo'n bericht kan sowieso niet kloppen.
func TestRefusesAnAnnouncedLengthThatIsTooBig(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		bad := make([]byte, headerSize)
		binary.BigEndian.PutUint16(bad[0:2], in.transaction)
		binary.BigEndian.PutUint16(bad[2:4], protocolID)
		binary.BigEndian.PutUint16(bad[4:6], 0xFFFF)
		bad[6] = in.unit
		return bad
	})
	done := make(chan error, 1)
	go func() {
		_, err := d.client(t).ReadHolding(1, 30005, 1)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "announces") {
			t.Fatalf("te grote lengte gaf %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("de client bleef wachten op bytes die nooit komen")
	}
}

// Een antwoord met een andere functiecode is niet het antwoord op deze vraag.
func TestRefusesAnAnswerWithTheWrongFunction(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, []byte{functionWriteMultiple, 0, 0, 0, 1})
	})
	_, err := d.client(t).ReadHolding(1, 30005, 1)
	if err == nil || !strings.Contains(err.Error(), "carries function") {
		t.Fatalf("verkeerde functiecode gaf %v", err)
	}
}

// Een antwoord dat minder registers draagt dan het aankondigt is niet te
// vertrouwen; de helft van een meting melden is erger dan niets melden.
func TestRefusesAnAnswerThatDoesNotCarryWhatItAnnounces(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, []byte{functionReadHolding, 8, 0, 1, 0, 2})
	})
	_, err := d.client(t).ReadHolding(1, 30005, 2)
	if err == nil || !strings.Contains(err.Error(), "announces 8 bytes") {
		t.Fatalf("kort antwoord gaf %v", err)
	}
}

func TestWritesHoldingRegisters(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		// Het apparaat herhaalt adres en aantal; dat is de hele bevestiging.
		return frame(in.transaction, in.unit, []byte{functionWriteMultiple,
			in.payload[0], in.payload[1], in.payload[2], in.payload[3]})
	})
	if err := d.client(t).WriteHolding(1, 42000, []uint16{1}); err != nil {
		t.Fatal(err)
	}
	got := <-d.seen
	if got.function != functionWriteMultiple {
		t.Fatalf("functiecode = 0x%02X", got.function)
	}
	if start := binary.BigEndian.Uint16(got.payload[0:2]); start != 42000 {
		t.Fatalf("adres = %d", start)
	}
	if count := binary.BigEndian.Uint16(got.payload[2:4]); count != 1 {
		t.Fatalf("aantal = %d", count)
	}
	if got.payload[4] != 2 {
		t.Fatalf("bytegetal = %d", got.payload[4])
	}
	if value := binary.BigEndian.Uint16(got.payload[5:7]); value != 1 {
		t.Fatalf("geschreven waarde = %d", value)
	}
}

func TestWritesOneHoldingRegisterWithFunction06(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, append([]byte{functionWriteSingle}, in.payload...))
	})
	if err := d.client(t).WriteSingle(2, 42000, 0); err != nil {
		t.Fatal(err)
	}
	got := <-d.seen
	if got.function != functionWriteSingle {
		t.Fatalf("functiecode = 0x%02X", got.function)
	}
	if len(got.payload) != 4 {
		t.Fatalf("payload = %v", got.payload)
	}
	if start := binary.BigEndian.Uint16(got.payload[0:2]); start != 42000 {
		t.Fatalf("adres = %d", start)
	}
	if value := binary.BigEndian.Uint16(got.payload[2:4]); value != 0 {
		t.Fatalf("geschreven waarde = %d", value)
	}
}

func TestRefusesASingleWriteConfirmationThatDoesNotMatch(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, []byte{functionWriteSingle, 0xA4, 0x10, 0, 1})
	})
	err := d.client(t).WriteSingle(2, 42000, 0)
	if err == nil || !strings.Contains(err.Error(), "confirms") {
		t.Fatalf("verkeerde bevestiging gaf %v", err)
	}
}

// Een bevestiging die een ander adres of aantal noemt betekent dat er iets
// anders geschreven is dan gevraagd.
func TestRefusesAWriteConfirmationThatDoesNotMatch(t *testing.T) {
	d := newDevice(t, func(in request) []byte {
		return frame(in.transaction, in.unit, []byte{functionWriteMultiple, 0xA4, 0x11, 0, 1})
	})
	err := d.client(t).WriteHolding(1, 42000, []uint16{1})
	if err == nil || !strings.Contains(err.Error(), "confirms") {
		t.Fatalf("verkeerde bevestiging gaf %v", err)
	}
}

// Een vraag die groter is dan Modbus kan dragen hoort hier te stranden en niet
// bij het apparaat: anders komt er een uitzondering terug en weet je niet of het
// aan het adres lag of aan het aantal.
func TestRefusesReadsAndWritesThatAreTooBig(t *testing.T) {
	client := New("127.0.0.1", 1, time.Second)
	if _, err := client.ReadHolding(1, 0, 0); err == nil {
		t.Fatal("nul registers lezen hoorde te falen")
	}
	if _, err := client.ReadHolding(1, 0, MaxReadRegisters+1); err == nil {
		t.Fatal("126 registers lezen hoorde te falen")
	}
	if err := client.WriteHolding(1, 0, nil); err == nil {
		t.Fatal("niets schrijven hoorde te falen")
	}
	if err := client.WriteHolding(1, 0, make([]uint16, MaxWriteRegisters+1)); err == nil {
		t.Fatal("124 registers schrijven hoorde te falen")
	}
}
