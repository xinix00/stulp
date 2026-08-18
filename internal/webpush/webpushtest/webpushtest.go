// Package webpushtest is een pushdienst met de browser erachter, voor tests.
//
// Een echte pushdienst neemt versleutelde bytes aan; een echte browser
// ontsleutelt ze en toont een melding. Beide kanten staan hier, zodat een test
// niet hoeft te constateren dat er "iets" verstuurd is maar kan zeggen wat er op
// de telefoon zou komen te staan.
//
// Het ontsleutelen is met opzet los opgeschreven van internal/webpush: het leidt
// af vanaf de private sleutel van de ontvanger, de andere kant op. Wie de
// versleuteling verkeerd om zet komt hier niet door.
package webpushtest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/xinix00/stulp/internal/webpush"
)

// Service is één nagemaakt toestel bij één nagemaakte pushdienst.
type Service struct {
	server  *httptest.Server
	private *ecdh.PrivateKey
	auth    []byte

	mu       sync.Mutex
	messages []webpush.Message
	headers  []http.Header
	status   int
	answer   string
}

// New start een pushdienst die alles aanneemt en na de test weer weg is.
func New(t *testing.T) *Service {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{private: private, auth: auth, status: http.StatusCreated}
	service.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if readErr != nil {
			t.Errorf("webpushtest: kon het verzoek niet lezen: %v", readErr)
		}
		service.mu.Lock()
		status, answer := service.status, service.answer
		service.headers = append(service.headers, request.Header.Clone())
		service.mu.Unlock()

		if status >= 200 && status < 300 {
			message, decryptErr := service.decrypt(body)
			if decryptErr != nil {
				// Een pushdienst zou dit niet merken, maar een test wil het weten:
				// bytes die de browser niet kan lezen zijn een stille storing.
				t.Errorf("webpushtest: de browser kon dit bericht niet lezen: %v", decryptErr)
			} else {
				service.mu.Lock()
				service.messages = append(service.messages, message)
				service.mu.Unlock()
			}
		}
		response.WriteHeader(status)
		if answer != "" {
			_, _ = response.Write([]byte(answer))
		}
	}))
	t.Cleanup(service.server.Close)
	return service
}

// Endpoint is het adres dat een browser bij het aanmelden zou afgeven.
func (s *Service) Endpoint() string { return s.server.URL + "/push/device" }

// Keys zijn de publieke sleutel en het gedeelde geheim, base64url, zoals de
// browser ze in zijn abonnement zet. Voor een test die via de Web API aanmeldt.
func (s *Service) Keys() (p256dh, auth string) {
	return base64.RawURLEncoding.EncodeToString(s.private.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(s.auth)
}

// Subscription is dit toestel als het pushprotocol het ziet, voor een test die
// rechtstreeks verstuurt.
func (s *Service) Subscription() webpush.Subscription {
	return webpush.Subscription{
		Endpoint: s.Endpoint(), P256dh: s.private.PublicKey().Bytes(), Auth: s.auth,
	}
}

// Client vertrouwt het certificaat van deze nagemaakte dienst. Zonder deze
// client weigert Go de verbinding, en dan test je de weigering.
func (s *Service) Client() *http.Client { return s.server.Client() }

// ClientFor vertrouwt meerdere nagemaakte diensten tegelijk. Nodig zodra een test
// twee toestellen heeft: elke httptest-server heeft zijn eigen certificaat, en een
// client die er maar één van kent laat de tweede afketsen op TLS in plaats van op
// wat de test wil aantonen.
func ClientFor(services ...*Service) *http.Client {
	pool := x509.NewCertPool()
	for _, service := range services {
		pool.AddCert(service.server.Certificate())
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// Messages is wat het toestel te zien zou krijgen, in volgorde van aankomst.
func (s *Service) Messages() []webpush.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]webpush.Message(nil), s.messages...)
}

// Headers zijn de koppen van elk verzoek dat binnenkwam.
func (s *Service) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// Answer laat de dienst hierna met deze status antwoorden. Zo is een verlopen
// abonnement (410) of een geweigerde ondertekening (403) na te doen.
func (s *Service) Answer(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.answer = status, body
}

// decrypt draait RFC 8291 terug: de kop noemt het zout en de eenmalige publieke
// sleutel van de afzender, en daarmee komt de ontvanger op dezelfde sleutel uit.
func (s *Service) decrypt(body []byte) (webpush.Message, error) {
	var message webpush.Message
	if len(body) < 22 {
		return message, fmt.Errorf("de body is %d bytes en bevat geen kop", len(body))
	}
	salt := body[:16]
	keyLength := int(body[20])
	if 21+keyLength > len(body) {
		return message, fmt.Errorf("de kop belooft een sleutel van %d bytes die er niet in past", keyLength)
	}
	senderKey := body[21 : 21+keyLength]
	ciphertext := body[21+keyLength:]

	sender, err := ecdh.P256().NewPublicKey(senderKey)
	if err != nil {
		return message, fmt.Errorf("de publieke sleutel in de kop lezen: %w", err)
	}
	shared, err := s.private.ECDH(sender)
	if err != nil {
		return message, err
	}
	authPRK, err := hkdf.Extract(sha256.New, shared, s.auth)
	if err != nil {
		return message, err
	}
	keyInfo := "WebPush: info\x00" + string(s.private.PublicKey().Bytes()) + string(senderKey)
	ikm, err := hkdf.Expand(sha256.New, authPRK, keyInfo, 32)
	if err != nil {
		return message, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return message, err
	}
	contentKey, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return message, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return message, err
	}
	block, err := aes.NewCipher(contentKey)
	if err != nil {
		return message, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return message, err
	}
	record, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return message, fmt.Errorf("ontsleutelen: %w", err)
	}
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		return message, fmt.Errorf("het record wordt niet met 0x02 afgesloten")
	}
	if err := json.Unmarshal(record[:len(record)-1], &message); err != nil {
		return message, fmt.Errorf("de inhoud is geen bericht: %w", err)
	}
	return message, nil
}
