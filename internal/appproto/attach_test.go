package appproto

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// socketPath levert een pad voor een unix-socket dat kort genoeg is.
//
// Niet t.TempDir(): op macOS is dat een pad diep onder /var/folders, en samen met
// de naam van de test komt het over de 104 bytes die in sun_path passen. Dan
// faalt de test op het besturingssysteem in plaats van op wat hij meet.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "sa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return filepath.Join(directory, "s")
}

// De begroeting gaat over dezelfde verbinding als de frames erna. Die verbinding
// buffert, dus als de sessie een nieuw omhulsel zou krijgen, zouden de bytes die
// al binnen zijn in het oude blijven staan -- en dan hangt de handshake op een
// hello die wél verstuurd is. Deze test is er om dat te betrappen: hij stuurt de
// begroeting en het eerste verzoek zo snel achter elkaar dat ze in één lezing
// belanden.
func TestAttachKeepsWhatIsAlreadyBufferedAfterTheGreeting(t *testing.T) {
	path := socketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	greetings := make(chan Attach, 1)
	served := make(chan error, 1)
	go func() {
		raw, acceptErr := listener.Accept()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		conn := NewConn(raw)
		if writeErr := Greet(conn, 1, ""); writeErr != nil {
			served <- writeErr
			return
		}
		greeting, readErr := ReadAttach(conn)
		if readErr != nil {
			served <- readErr
			return
		}
		greetings <- greeting
		if writeErr := WriteAttachReply(conn, AttachReply{OK: true}); writeErr != nil {
			served <- writeErr
			return
		}
		// Dezelfde conn, zoals de supervisor hem doorgeeft aan de runtime.
		session := NewSession(conn, func(_ context.Context, method string, params json.RawMessage) (any, error) {
			return map[string]string{"answered": method}, nil
		}, nil)
		served <- session.Serve()
	}()

	raw, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	conn := NewConn(raw)
	if err := SendAttach(conn, "com.stulp.demo", "", 1, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case greeting := <-greetings:
		if greeting.AppID != "com.stulp.demo" {
			t.Fatalf("greeting arrived as %#v", greeting)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the greeting never arrived")
	}

	session := NewSession(conn, nil, nil)
	go session.Serve()
	defer session.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := session.Call(ctx, "hello", map[string]any{"protocol": 1})
	if err != nil {
		t.Fatalf("the protocol did not survive the greeting: %v", err)
	}
	var reply map[string]string
	if err := json.Unmarshal(answer, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["answered"] != "hello" {
		t.Fatalf("unexpected answer: %#v", reply)
	}
}

// Een geweigerde attach hoort een zin terug te geven. Wie zijn app in een
// container zet leest alleen de log van die container; "unknown app" is daar het
// hele verschil met een verbinding die zomaar dichtgaat.
func TestRefusedAttachReturnsTheReason(t *testing.T) {
	path := socketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		raw, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		conn := NewConn(raw)
		if writeErr := Greet(conn, 1, ""); writeErr != nil {
			conn.Close()
			return
		}
		if _, readErr := ReadAttach(conn); readErr != nil {
			conn.Close()
			return
		}
		WriteAttachReply(conn, AttachReply{Error: `app "com.stulp.demo" is disabled`})
		conn.Close()
	}()

	raw, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	conn := NewConn(raw)
	defer conn.Close()
	err = SendAttach(conn, "com.stulp.demo", "", 1, nil)
	if err == nil {
		t.Fatal("a refused attach reported success")
	}
	if !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("the reason did not survive: %v", err)
	}
}

// Een app die zich met een token meldt, hoort niet door te gaan tegen iemand die
// zich niet als Stulp kan bewijzen. Anders kan wie de poort overneemt een app
// apparaten, instellingen en Flow-opdrachten voorschotelen die niet van Stulp
// komen.
func TestAnAppRefusesAStulpThatCannotProveItself(t *testing.T) {
	const secret = "geheim"
	const appID = "com.stulp.demo"
	token := Token(secret, appID)

	for _, testCase := range []struct{ name, proof string }{
		{"no proof at all", ""},
		{"a proof from nowhere", "zomaar-iets"},
		{"a proof from a stulp with another secret", StulpProof(Token("ander", appID), "x", appID)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := socketPath(t)
			listener, err := Listen(path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			go func() {
				raw, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				conn := NewConn(raw)
				nonce, _ := Nonce()
				// Een nonce vragen kan iedereen; hem beantwoorden niet.
				if writeErr := Greet(conn, 1, nonce); writeErr != nil {
					conn.Close()
					return
				}
				if _, readErr := ReadAttach(conn); readErr != nil {
					conn.Close()
					return
				}
				WriteAttachReply(conn, AttachReply{OK: true, Proof: testCase.proof})
				conn.Close()
			}()

			raw, err := Dial(path)
			if err != nil {
				t.Fatal(err)
			}
			conn := NewConn(raw)
			defer conn.Close()
			err = SendAttach(conn, appID, token, 1, nil)
			if err == nil {
				t.Fatal("the app accepted a stulp that could not prove itself")
			}
			if !strings.Contains(err.Error(), "prove it is stulp") {
				t.Fatalf("the failure was about something else: %v", err)
			}
		})
	}
}

func TestGreetingWithoutAnAppIDIsRefused(t *testing.T) {
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	go func() {
		body, _ := json.Marshal(Attach{Protocol: 1})
		NewConn(x).WriteRaw(body)
	}()
	if _, err := ReadAttach(NewConn(y)); err == nil {
		t.Fatal("a greeting without an app id was accepted")
	}
}

// De uid van de andere kant komt van de kernel en niet van de app. Deze test kan
// niet aantonen dat een andere gebruiker geweigerd wordt -- daar zou een tweede
// gebruiker voor nodig zijn -- maar wel dat het slot bestaat, dat het over een
// echte unix-socket werkt, en dat het onze eigen verbinding doorlaat.
func TestPeerCredentialsRecogniseOurselves(t *testing.T) {
	path := socketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		raw, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- raw
		}
	}()
	client, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var server net.Conn
	select {
	case server = <-accepted:
		defer server.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("no connection was accepted")
	}

	uid, err := PeerUID(server)
	if err != nil {
		t.Fatalf("peer credentials are unavailable: %v", err)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("the peer reads as uid %d, we are %d", uid, os.Getuid())
	}
	if err := CheckPeer(server); err != nil {
		t.Fatalf("our own connection was refused: %v", err)
	}
}

// Een Stulp die niet netjes gestopt is laat een socket achter. De volgende start
// hoort die op te ruimen en niet te struikelen over "address already in use".
func TestListenReplacesASocketLeftBehind(t *testing.T) {
	path := socketPath(t)
	first, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sluiten zonder op te ruimen: net.Listener verwijdert het bestand normaal
	// zelf, dus dit bootst na wat een kill -9 achterlaat.
	if unixListener, ok := first.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	first.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the socket was cleaned up after all, so this test proves nothing: %v", err)
	}

	second, err := Listen(path)
	if err != nil {
		t.Fatalf("a stale socket blocked the next start: %v", err)
	}
	second.Close()
}

// Een gewoon bestand op dit pad is een vergissing van wie het pad koos. Dat
// verwijderen zou erger zijn dan weigeren.
func TestListenRefusesToRemoveSomethingThatIsNotASocket(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("niet een socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("a regular file was taken over as a socket")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
}

// De socket hoort niet voor anderen te openen te zijn. Het echte slot is de uid,
// maar dit is het slot dat ervoor zit.
func TestTheSocketIsNotReadableByOthers(t *testing.T) {
	path := socketPath(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("the socket is reachable by others: %04o", mode)
	}
}
