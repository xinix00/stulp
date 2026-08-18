// Package webpush bezorgt een bericht bij de pushdienst van een browser.
//
// Dit is de enige plek in Stulp die het huis uit belt om iemand te bereiken.
// Dat is met opzet een dunne laag: er staat geen account tegenover, geen
// abonnement en geen dienst van ons. De browser geeft zelf een adres af bij een
// pushdienst die hij vertrouwt -- web.push.apple.com voor een iPhone,
// fcm.googleapis.com voor Android -- en Stulp zet daar een versleuteld pakketje
// neer. De pushdienst kan niet lezen wat erin zit; hij weet alleen naar welk
// toestel het moet.
//
// Twee losse dingen komen samen in één verzoek:
//
//   - RFC 8291 versleutelt de inhoud met een sleutel die alleen de browser kan
//     afleiden. De ontvanger geeft daarvoor een publieke sleutel (p256dh) en een
//     geheim (auth) af bij het aanmelden.
//   - RFC 8292 (VAPID) ondertekent het verzoek zodat de pushdienst weet dat het
//     van dezelfde afzender komt als de vorige keer. Dat is geen toegangsbewijs
//     tot het toestel, maar een identiteit die misbruik traceerbaar houdt.
//
// Deze package kent de opslag niet: hij krijgt een abonnement en een sleutel
// mee en levert af. Wie wat bewaart staat in internal/store.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// recordSize is de recordgrootte die in de kop staat. Eén record is genoeg:
	// een pushbericht is een regel tekst, niet een bestand.
	recordSize = 4096
	// headerSize is salt(16) + recordgrootte(4) + sleutellengte(1) + sleutel(65).
	headerSize = 16 + 4 + 1 + 65
	// MaxPayload is wat er in dat ene record past: de kop, de GCM-tag en de
	// afsluitbyte gaan eraf. Elke pushdienst weigert meer, dus wordt een te lang
	// bericht hier geweigerd in plaats van stil afgekapt -- een melding die
	// halverwege ophoudt is erger dan een Flow die zegt dat hij niet paste.
	MaxPayload = recordSize - headerSize - 16 - 1

	// vapidLifetime is hoe lang een ondertekend verzoek geldig is. RFC 8292
	// staat maximaal 24 uur toe; de helft daarvan is ruim en scheelt niets.
	vapidLifetime = 12 * time.Hour

	authSecretSize = 16
	publicKeySize  = 65
	privateKeySize = 32
)

// ErrGone zegt dat de pushdienst dit abonnement niet meer kent. Dat is geen
// storing: de browser is gewist, de app is van het beginscherm gehaald of de
// pushdienst heeft het adres vervangen. De enige juiste reactie is het abonnement
// weggooien, dus is dit een eigen fout en geen HTTP-status om te herkennen.
var ErrGone = errors.New("de pushdienst kent dit abonnement niet meer")

// Subscription is wat een browser afgeeft bij het aanmelden.
type Subscription struct {
	// Endpoint is het https-adres van de pushdienst voor dit ene toestel.
	Endpoint string
	// P256dh is de publieke sleutel van de browser, 65 bytes ongecomprimeerd.
	P256dh []byte
	// Auth is het gedeelde geheim van 16 bytes uit RFC 8291.
	Auth []byte
}

// Message is wat het toestel te zien krijgt. De service worker leest deze velden
// terug uit de versleutelde inhoud.
//
// Een kop, een regel tekst en waar een tik naartoe gaat. Meer niet: alles wat
// hier bij komt moet door de browser vertaald worden naar iets zichtbaars, en er
// is geen enkele melding in een huis die daar meer voor nodig heeft.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is waar de browser naartoe gaat als er op de melding wordt getikt.
	URL string `json:"url,omitempty"`
	// Image is het adres van een plaatje bij de melding -- de momentopname van een
	// camera, bijvoorbeeld. Een adres en niet de bytes zelf: er past 3993 bytes in
	// één push en een foto van een deurbel is een megabyte. De service worker
	// haalt het op als hij de melding toont.
	//
	// Android laat het zien, een iPhone niet: Safari kent de image-optie niet en
	// toont alleen kop en tekst. Het bericht komt daar dus wel aan, de foto niet.
	Image string `json:"image,omitempty"`
}

// Sender verstuurt met één VAPID-identiteit.
type Sender struct {
	// Client mag leeg zijn; dan wordt er een met een tijdslimiet gebruikt.
	Client *http.Client
	// Subject is het contactadres dat in de VAPID-claim komt: mailto: of een
	// https-adres. Pushdiensten gebruiken het om een afzender te bereiken die
	// hen tot last is. Leeg mag: dan blijft de claim weg.
	Subject string
}

var defaultClient = &http.Client{Timeout: 15 * time.Second}

func (s Sender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient
}

// GenerateKey maakt een nieuwe VAPID-identiteit en geeft de private sleutel als
// 32 ruwe bytes terug. Dat is precies wat er van bewaard hoeft te worden: de
// publieke helft volgt eruit.
func GenerateKey() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("VAPID-sleutel maken: %w", err)
	}
	raw, err := key.Bytes()
	if err != nil {
		return nil, fmt.Errorf("VAPID-sleutel maken: %w", err)
	}
	return raw, nil
}

// PublicKey geeft de publieke helft als 65 ongecomprimeerde bytes. De browser
// heeft die nodig als applicationServerKey bij het aanmelden, en de pushdienst
// als k= in de ondertekening.
func PublicKey(privateKey []byte) ([]byte, error) {
	key, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return key.PublicKey.Bytes()
}

func parsePrivateKey(privateKey []byte) (*ecdsa.PrivateKey, error) {
	if len(privateKey) != privateKeySize {
		return nil, fmt.Errorf("VAPID-sleutel is %d bytes en moet %d zijn", len(privateKey), privateKeySize)
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), privateKey)
	if err != nil {
		return nil, fmt.Errorf("VAPID-sleutel lezen: %w", err)
	}
	return key, nil
}

// Validate weigert een abonnement dat niet klopt in plaats van het te repareren.
// De maten liggen vast in RFC 8291; iets anders betekent dat de browser iets
// anders bedoelde, en dan is versleutelen naar een gok toe zinloos.
func (s Subscription) Validate() error {
	target, err := url.Parse(s.Endpoint)
	if err != nil {
		return fmt.Errorf("pushadres lezen: %w", err)
	}
	if target.Scheme != "https" || target.Host == "" {
		return errors.New("een pushadres moet een https-adres zijn")
	}
	if len(s.P256dh) != publicKeySize {
		return fmt.Errorf("de publieke sleutel van de browser is %d bytes en moet %d zijn", len(s.P256dh), publicKeySize)
	}
	if len(s.Auth) != authSecretSize {
		return fmt.Errorf("het gedeelde geheim is %d bytes en moet %d zijn", len(s.Auth), authSecretSize)
	}
	if _, err := ecdh.P256().NewPublicKey(s.P256dh); err != nil {
		return fmt.Errorf("de publieke sleutel van de browser ligt niet op P-256: %w", err)
	}
	return nil
}

// Send versleutelt het bericht voor dit ene abonnement en zet het bij de
// pushdienst neer.
//
// Een geslaagde aflevering betekent dat de pushdienst het pakketje heeft
// aangenomen, niet dat de telefoon het gezien heeft. Verder komt Stulp niet, en
// dat is inherent aan push: het toestel kan uit staan.
func (s Sender) Send(ctx context.Context, privateKey []byte, subscription Subscription, message Message, ttl time.Duration) error {
	if err := subscription.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("bericht coderen: %w", err)
	}
	if len(payload) > MaxPayload {
		return fmt.Errorf("het bericht is %d bytes en er passen er %d in één push", len(payload), MaxPayload)
	}
	body, err := encrypt(subscription, payload)
	if err != nil {
		return err
	}
	authorization, err := authorization(subscription.Endpoint, privateKey, s.Subject, time.Now())
	if err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, subscription.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pushverzoek opbouwen: %w", err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	// Een deurbel is geen achtergrondwerk. Urgency high vraagt de pushdienst om
	// niet te wachten tot het toestel toch al wakker is.
	request.Header.Set("Urgency", "high")

	response, err := s.client().Do(request)
	if err != nil {
		return fmt.Errorf("pushdienst bereiken: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		response.Body.Close()
	}()
	switch {
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		return fmt.Errorf("%w (%s)", ErrGone, response.Status)
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	}
	// De uitleg van een pushdienst is een regel tekst. Meer dan dat hoeft niet
	// in een foutmelding te passen; het staat er zodat een 403 op een verkeerde
	// VAPID-sleutel te herkennen valt.
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if len(bytes.TrimSpace(detail)) == 0 {
		return fmt.Errorf("pushdienst weigerde het bericht: %s", response.Status)
	}
	return fmt.Errorf("pushdienst weigerde het bericht: %s: %s", response.Status, bytes.TrimSpace(detail))
}

// encrypt bouwt de body van RFC 8291: een kop met het zout en onze eenmalige
// publieke sleutel, gevolgd door één AES-128-GCM-record.
//
// De sleutel komt uit een ECDH tussen een sleutelpaar dat alleen voor dit
// bericht bestaat en de publieke sleutel van de browser, vermengd met het
// gedeelde geheim uit het abonnement. Daardoor kan de pushdienst -- die het
// pakketje wel doorgeeft -- de inhoud niet lezen, en kan een onderschept
// bericht niet met een ander bericht worden verwisseld.
func encrypt(subscription Subscription, plaintext []byte) ([]byte, error) {
	recipient, err := ecdh.P256().NewPublicKey(subscription.P256dh)
	if err != nil {
		return nil, fmt.Errorf("publieke sleutel van de browser lezen: %w", err)
	}
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("eenmalig sleutelpaar maken: %w", err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, fmt.Errorf("gedeelde sleutel afleiden: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("zout maken: %w", err)
	}
	sender := ephemeral.PublicKey().Bytes()

	// Twee keer HKDF, zoals RFC 8291 voorschrijft. De eerste ronde bindt het
	// gedeelde geheim aan beide publieke sleutels, de tweede leidt daar met het
	// zout sleutel en nonce uit af.
	authPRK, err := hkdf.Extract(sha256.New, shared, subscription.Auth)
	if err != nil {
		return nil, fmt.Errorf("sleutel afleiden: %w", err)
	}
	keyInfo := make([]byte, 0, len("WebPush: info")+1+2*publicKeySize)
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0)
	keyInfo = append(keyInfo, subscription.P256dh...)
	keyInfo = append(keyInfo, sender...)
	ikm, err := hkdf.Expand(sha256.New, authPRK, string(keyInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("sleutel afleiden: %w", err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, fmt.Errorf("sleutel afleiden: %w", err)
	}
	contentKey, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("sleutel afleiden: %w", err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("nonce afleiden: %w", err)
	}

	block, err := aes.NewCipher(contentKey)
	if err != nil {
		return nil, fmt.Errorf("versleutelen: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("versleutelen: %w", err)
	}
	// 0x02 sluit het laatste record af (RFC 8188). Zonder die byte weigert de
	// browser de inhoud, want dan lijkt er nog een record te volgen.
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02)

	body := make([]byte, 0, headerSize+len(record)+16)
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, recordSize)
	body = append(body, byte(len(sender)))
	body = append(body, sender...)
	return aead.Seal(body, nonce, record, nil), nil
}

// authorization ondertekent het verzoek volgens RFC 8292. De claim geldt voor
// de pushdienst als geheel, niet voor dit ene adres: aud is alleen schema en
// host, zodat het adres van het toestel niet in een ondertekend token belandt.
func authorization(endpoint string, privateKey []byte, subject string, now time.Time) (string, error) {
	key, err := parsePrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("pushadres lezen: %w", err)
	}
	if target.Scheme != "https" || target.Host == "" {
		return "", errors.New("een pushadres moet een https-adres zijn")
	}
	claims := map[string]any{
		"aud": target.Scheme + "://" + target.Host,
		"exp": now.Add(vapidLifetime).Unix(),
	}
	if subject != "" {
		claims["sub"] = subject
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("VAPID-claim coderen: %w", err)
	}
	signingInput := encode([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + encode(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, sValue, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("VAPID-token ondertekenen: %w", err)
	}
	// ES256 wil r en s achter elkaar met vaste lengte, niet de ASN.1-vorm die
	// ecdsa.SignASN1 geeft.
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	sValue.FillBytes(signature[32:])
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return "", fmt.Errorf("VAPID-sleutel lezen: %w", err)
	}
	return "vapid t=" + signingInput + "." + encode(signature) + ", k=" + encode(public), nil
}

func encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
