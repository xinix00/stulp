package webpush

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// browserSubscription maakt echt sleutelmateriaal zoals een browser afgeeft. Het
// ontsleutelen zit in webpushtest; hier gaat het om de bytes die eruit komen.
func browserSubscription(t *testing.T, endpoint string) Subscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, authSecretSize)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return Subscription{Endpoint: endpoint, P256dh: key.PublicKey().Bytes(), Auth: auth}
}

// Twee keer hetzelfde bericht mag niet twee keer dezelfde bytes opleveren: zout
// en sleutelpaar horen per bericht te zijn, en gelijke bytes zouden betekenen dat
// een van die twee vast staat.
func TestEveryMessageGetsItsOwnSaltAndKey(t *testing.T) {
	subscription := browserSubscription(t, "https://push.example/abc")
	first, err := encrypt(subscription, []byte("hetzelfde"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := encrypt(subscription, []byte("hetzelfde"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("twee berichten leverden dezelfde bytes op")
	}
	if string(first[:16]) == string(second[:16]) {
		t.Fatal("twee berichten kregen hetzelfde zout")
	}
	if string(first[21:headerSize]) == string(second[21:headerSize]) {
		t.Fatal("twee berichten kregen hetzelfde sleutelpaar")
	}
}

func TestHeaderSaysWhatTheBrowserNeedsToDecrypt(t *testing.T) {
	body, err := encrypt(browserSubscription(t, "https://push.example/abc"), []byte("hoi"))
	if err != nil {
		t.Fatal(err)
	}
	if size := binary.BigEndian.Uint32(body[16:20]); size != recordSize {
		t.Fatalf("de kop noemt recordgrootte %d in plaats van %d", size, recordSize)
	}
	if length := int(body[20]); length != publicKeySize {
		t.Fatalf("de kop noemt een sleutel van %d bytes in plaats van %d", length, publicKeySize)
	}
	if body[21] != 4 {
		t.Fatalf("de publieke sleutel begint met 0x%02x en is dus niet ongecomprimeerd", body[21])
	}
	// Kop plus record plus GCM-tag: één record en niets ertussen.
	if want := headerSize + len("hoi") + 1 + 16; len(body) != want {
		t.Fatalf("de body is %d bytes en zou %d moeten zijn", len(body), want)
	}
}

func TestAuthorizationIsATokenThePushServiceCanVerify(t *testing.T) {
	key := mustKey(t)
	header, err := authorization("https://push.example/some/long/device/path?token=x", key,
		"mailto:iemand@example.net", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token, publicKey := splitAuthorization(t, header)

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("het token heeft %d delen in plaats van 3", len(parts))
	}
	var claims struct {
		Audience string `json:"aud"`
		Subject  string `json:"sub"`
		Expires  int64  `json:"exp"`
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	// Alleen schema en host: het adres van de telefoon hoort niet in een token
	// dat de pushdienst leest en misschien bewaart.
	if claims.Audience != "https://push.example" {
		t.Fatalf("aud is %q", claims.Audience)
	}
	if claims.Subject != "mailto:iemand@example.net" {
		t.Fatalf("sub is %q", claims.Subject)
	}
	if remaining := time.Until(time.Unix(claims.Expires, 0)); remaining <= 0 || remaining > 24*time.Hour {
		t.Fatalf("exp ligt %v vooruit en moet tussen nu en 24 uur liggen", remaining)
	}

	// De ondertekening moet kloppen met de sleutel die er in k= bij zit, want dat
	// is precies wat een pushdienst controleert.
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), publicKey)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) != 64 {
		t.Fatalf("de ondertekening is %d bytes en ES256 wil 64", len(signature))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(public, digest[:], r, s) {
		t.Fatal("de pushdienst zou deze ondertekening afwijzen")
	}
	expected, err := PublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if string(publicKey) != string(expected) {
		t.Fatal("k= is niet de publieke sleutel van deze identiteit")
	}
}

// Zonder sub weigert Apple het token. Met een leeg contactadres hoort de claim
// weg te blijven in plaats van als lege string mee te gaan.
func TestAuthorizationLeavesOutAnEmptyContact(t *testing.T) {
	header, err := authorization("https://push.example/device", mustKey(t), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token, _ := splitAuthorization(t, header)
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if _, present := claims["sub"]; present {
		t.Fatalf("de claim bevat een sub die er niet is: %v", claims)
	}
}

func TestAuthorizationRefusesAnEndpointThatIsNotHTTPS(t *testing.T) {
	if _, err := authorization("http://push.example/device", mustKey(t), "", time.Now()); err == nil {
		t.Fatal("een adres over gewone http werd ondertekend")
	}
}

func TestValidateRefusesWhatCannotBeEncryptedFor(t *testing.T) {
	valid := browserSubscription(t, "https://push.example/abc")
	other := browserSubscription(t, "https://push.example/def")

	cases := map[string]Subscription{
		"geen adres":               {P256dh: valid.P256dh, Auth: valid.Auth},
		"http in plaats van https": {Endpoint: "http://push.example/abc", P256dh: valid.P256dh, Auth: valid.Auth},
		"sleutel te kort":          {Endpoint: valid.Endpoint, P256dh: valid.P256dh[:64], Auth: valid.Auth},
		"geheim te kort":           {Endpoint: valid.Endpoint, P256dh: valid.P256dh, Auth: valid.Auth[:15]},
		// Een punt met de juiste lengte die niet op de curve ligt: de vorm klopt,
		// de wiskunde niet.
		"sleutel niet op de curve": {Endpoint: valid.Endpoint, P256dh: append([]byte{4, 1}, other.P256dh[2:]...), Auth: valid.Auth},
	}
	for name, subscription := range cases {
		if err := subscription.Validate(); err == nil {
			t.Fatalf("%s werd aangenomen", name)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("een geldig abonnement werd geweigerd: %v", err)
	}
}

func TestPublicKeyRefusesAKeyOfTheWrongSize(t *testing.T) {
	if _, err := PublicKey(make([]byte, 31)); err == nil {
		t.Fatal("een sleutel van 31 bytes werd aangenomen")
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func splitAuthorization(t *testing.T, header string) (token string, publicKey []byte) {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("Authorization begint niet met \"vapid t=\": %q", header)
	}
	token, keyPart, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("Authorization bevat geen k=: %q", header)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(keyPart)
	if err != nil {
		t.Fatal(err)
	}
	return token, decoded
}
