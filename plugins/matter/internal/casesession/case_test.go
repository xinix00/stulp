package casesession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

func TestEstablishAuthenticatesPeerAndCarriesOperationalTraffic(t *testing.T) {
	fabric, err := credentials.NewFabric(0x1111, 0x2222, 0x3333, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const peerNodeID = 0x4444
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceCert, err := fabric.SignNode(&deviceKey.PublicKey, peerNodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	deviceNOC, err := credentials.MatterCertificate(deviceCert)
	if err != nil {
		t.Fatal(err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	initiator, err := transport.Listen("127.0.0.1:0", quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer initiator.Close()
	responder, err := transport.Listen("127.0.0.1:0", quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	initiator.RetryInterval = 20 * time.Millisecond
	responder.RetryInterval = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	serverSession := make(chan *transport.SecureSession, 1)
	serverError := make(chan error, 1)
	go func() {
		session, serveErr := Accept(ctx, responder, ResponderConfig{
			Fabric: fabric, LocalNodeID: peerNodeID, PrivateKey: deviceKey, NOC: deviceNOC,
		})
		if serveErr != nil {
			serverError <- serveErr
			return
		}
		serverSession <- session
	}()

	clientSession, err := Establish(ctx, initiator, responder.LocalAddr(), fabric, peerNodeID, deviceNOC)
	if err != nil {
		t.Fatal(err)
	}
	var peerSession *transport.SecureSession
	select {
	case err := <-serverError:
		t.Fatal(err)
	case peerSession = <-serverSession:
	}
	if peerSession == nil {
		t.Fatal("responder did not install the CASE session")
	}

	exchange, err := initiator.InitiateSecure(clientSession, message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Close()
	if err := exchange.SendOnce(0x02, []byte("operational read")); err != nil {
		t.Fatal(err)
	}
	peerExchange, err := responder.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer peerExchange.Close()
	opcode, payload, err := peerExchange.Receive(ctx)
	if err != nil || opcode != 0x02 || string(payload) != "operational read" {
		t.Fatalf("operational message opcode=%x payload=%q err=%v", opcode, payload, err)
	}
}

func TestEstablishRejectsDifferentNOC(t *testing.T) {
	fabric, err := credentials.NewFabric(1, 2, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cert, _ := fabric.SignNode(&key.PublicKey, 4, time.Now())
	noc, _ := credentials.MatterCertificate(cert)
	changed := append([]byte(nil), noc...)
	changed[len(changed)/2] ^= 1
	if credentials.EqualBytes(noc, changed) {
		t.Fatal("test mutation did not change NOC")
	}
}
