package supervisor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/plugin/plugintest"
	"github.com/xinix00/stulp/internal/store"
)

// attachFixture is een Stulp met één app die zichzelf moet melden.
type attachFixture struct {
	apps     *Supervisor
	database *store.Store
	appID    string
	root     string
	socket   string
}

// socketPath levert een pad voor een unix-socket dat kort genoeg is.
//
// Niet t.TempDir(): op macOS is dat een pad diep onder /var/folders, en samen met
// de naam van de test komt het over de 104 bytes die in sun_path passen.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "sa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	return filepath.Join(directory, "s")
}

// newAttachFixture zet een app neer die "external" is en dus op zichzelf wacht.
//
// De binary staat er wel bij. Dat is met opzet: het bewijst dat "external" de
// reden is dat Stulp hem niet start, en niet dat er niets te starten valt.
func newAttachFixture(t *testing.T, external bool) *attachFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	appID := "com.stulp.attach"
	declaration := ""
	if external {
		declaration = `"external":true,`
	}
	if err := os.WriteFile(filepath.Join(root, "app.json"), []byte(`{
  "id":"`+appID+`","version":"1.0.0","sdk":3,`+declaration+`"name":{"en":"Attach"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugintest.Install(t, root, appID)

	appManifest, appRoot, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.InstallApp(ctx, appManifest, appRoot, ""); err != nil {
		t.Fatal(err)
	}

	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	apps.retryBase = 20 * time.Millisecond
	apps.retryMax = 40 * time.Millisecond
	t.Cleanup(apps.Close)

	socket := socketPath(t)
	listener, err := appproto.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go apps.ServeAttach(listener)

	return &attachFixture{apps: apps, database: database, appID: appID, root: appRoot, socket: socket}
}

// launch start de app zoals Docker of systemd dat zou doen: als eigen proces, met
// het adres van Stulp in zijn omgeving. De app-id staat er niet bij -- die leest
// de SDK uit app.json naast de binary.
func (f *attachFixture) launch(t *testing.T) *exec.Cmd {
	t.Helper()
	return f.launchWith(t, "STULP_ATTACH="+f.socket)
}

func (f *attachFixture) launchWith(t *testing.T, environment ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(filepath.Join(f.root, f.appID))
	command.Dir = f.root
	command.Env = append(os.Environ(), environment...)
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			command.Process.Kill()
			command.Wait()
		}
	})
	return command
}

func (f *attachFixture) waitForState(t *testing.T, want string) AppState {
	t.Helper()
	// Ruim: hier start een echt proces, en onder een volle testrun kost dat soms
	// seconden. Een test die faalt omdat de machine het druk had zegt niets.
	deadline := time.Now().Add(30 * time.Second)
	for {
		state := f.apps.State(f.appID)
		if state.State == want {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("app never reached %q: %#v", want, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// De hele weg: een app die Stulp niet start, die zich meldt, die daarna gewoon een
// app is, en die na het wegvallen niet door Stulp wordt overgenomen.
func TestAnExternalAppAttachesItselfAndIsNotRestartedByStulp(t *testing.T) {
	ctx := context.Background()
	fixture := newAttachFixture(t, true)

	// Stulp start hem niet, ook al staat de binary er. Geen crash en geen
	// herstartlus: er is niets mis, er is alleen nog niemand.
	if err := fixture.apps.StartAll(ctx); err != nil {
		t.Fatalf("an external app made StartAll fail: %v", err)
	}
	waiting := fixture.waitForState(t, "waiting")
	if waiting.RetryAt != "" {
		t.Fatalf("stulp scheduled a restart for an app it does not own: %#v", waiting)
	}

	// Nu meldt de app zich, en vanaf dat moment is hij een gewone app.
	command := fixture.launch(t)
	fixture.waitForState(t, "running")
	if _, err := fixture.apps.Registrations(ctx, fixture.appID); err != nil {
		t.Fatalf("an attached app does not answer like a spawned one: %v", err)
	}

	// Een tweede exemplaar hoort geweigerd te worden: welke van de twee de echte
	// is kan Stulp niet weten.
	if err := attachDirectly(fixture.socket, fixture.appID); err == nil {
		t.Fatal("a second instance was allowed to take over a running app")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal did not say why: %v", err)
	}

	// En nu valt hij weg. Stulp hoort dat te merken, en hij hoort de binary naast
	// app.json niet te gaan starten -- dat zou een tweede exemplaar zijn naast het
	// exemplaar dat de orkestrator zelf herstart.
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	gone := fixture.waitForState(t, "waiting")
	if gone.RetryAt != "" {
		t.Fatalf("stulp scheduled a restart after an attached app died: %#v", gone)
	}
	// Blijven kijken: een herstart die Stulp toch inplant, zou hier alsnog
	// "running" opleveren zonder dat er iemand aangemeld is.
	time.Sleep(200 * time.Millisecond)
	if state := fixture.apps.State(fixture.appID); state.State != "waiting" {
		t.Fatalf("stulp started the app on its own after all: %#v", state)
	}

	// Opnieuw aanmelden hoort te kunnen: dat is wat een container met "restart:
	// always" doet.
	fixture.launch(t)
	fixture.waitForState(t, "running")
}

// Een uitgezette app hoort niet binnen te komen, ook niet als hij zichzelf
// aanbiedt. Zonder dit blijft een container met "restart: always" zich melden en
// draait een app die uitgezet is.
func TestADisabledAppMayNotAttach(t *testing.T) {
	ctx := context.Background()
	fixture := newAttachFixture(t, true)
	if err := fixture.apps.Disable(ctx, fixture.appID); err != nil {
		t.Fatal(err)
	}
	err := attachDirectly(fixture.socket, fixture.appID)
	if err == nil {
		t.Fatal("a disabled app was allowed to attach")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("the refusal did not say why: %v", err)
	}
}

func TestAnUnknownAppMayNotAttach(t *testing.T) {
	fixture := newAttachFixture(t, true)
	err := attachDirectly(fixture.socket, "com.stulp.nooit")
	if err == nil {
		t.Fatal("an app that is not installed was allowed to attach")
	}
	if !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("the refusal did not say why: %v", err)
	}
}

// Een app die een ander protocol spreekt hoort bij het aanmelden te falen en niet
// halverwege een handshake.
func TestAProtocolMismatchIsRefusedAtTheGreeting(t *testing.T) {
	fixture := newAttachFixture(t, true)
	raw, err := appproto.Dial(fixture.socket)
	if err != nil {
		t.Fatal(err)
	}
	conn := appproto.NewConn(raw)
	defer conn.Close()
	err = appproto.SendAttach(conn, fixture.appID, "", plugin.ProtocolVersion+1, nil)
	if err == nil {
		t.Fatal("an app from another protocol version was allowed in")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("the refusal did not say why: %v", err)
	}
}

// Een app zonder "external" blijft van Stulp: die start hij zelf, en dan hoort een
// aanmelding op de bezette plek te stuiten.
func TestAppsStulpStartsItselfKeepTheirSlot(t *testing.T) {
	ctx := context.Background()
	fixture := newAttachFixture(t, false)
	if err := fixture.apps.Start(ctx, fixture.appID); err != nil {
		t.Fatalf("a normal app did not start: %v", err)
	}
	fixture.waitForState(t, "running")
	err := attachDirectly(fixture.socket, fixture.appID)
	if err == nil {
		t.Fatal("an attach took over an app that stulp started itself")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal did not say why: %v", err)
	}
}

// attachDirectly meldt zich aan zonder een app te zijn: genoeg om te zien wat
// Stulp van de aanmelding vindt.
func attachDirectly(socket, appID string) error {
	raw, err := appproto.Dial(socket)
	if err != nil {
		return err
	}
	conn := appproto.NewConn(raw)
	defer conn.Close()
	return appproto.SendAttach(conn, appID, "", plugin.ProtocolVersion, nil)
}

// ---------------------------------------------------------------------------
// Over een poort
// ---------------------------------------------------------------------------

// listenTLS hangt een poort met TLS aan dezelfde supervisor, en levert het adres
// en het certificaat waarmee een app het na kan rekenen.
func (f *attachFixture) listenTLS(t *testing.T) (address, caPath string) {
	t.Helper()
	certificate, pemBytes := selfSignedCertificate(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(raw, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
	})
	t.Cleanup(func() { listener.Close() })
	go f.apps.ServeAttach(listener)

	directory := t.TempDir()
	caPath = filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw.Addr().String(), caPath
}

// token levert het token van deze app, zoals `stulp attach-token` dat doet.
func (f *attachFixture) token(t *testing.T) string {
	t.Helper()
	secret, err := f.database.AttachSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return appproto.Token(secret, f.appID)
}

// Een app op een andere machine of in een eigen pod: over een poort, met TLS, en
// met een token in plaats van een uid.
func TestAnAppAttachesOverAPortWithItsToken(t *testing.T) {
	ctx := context.Background()
	fixture := newAttachFixture(t, true)
	address, caPath := fixture.listenTLS(t)
	if err := fixture.apps.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.waitForState(t, "waiting")

	fixture.launchWith(t,
		"STULP_ATTACH="+address,
		"STULP_ATTACH_TOKEN="+fixture.token(t),
		"STULP_ATTACH_CA="+caPath,
	)
	fixture.waitForState(t, "running")
	if _, err := fixture.apps.Registrations(ctx, fixture.appID); err != nil {
		t.Fatalf("an app attached over a port does not answer like a local one: %v", err)
	}
}

// Zonder token komt er niemand binnen over een poort. Dat is het hele verschil met
// de unix-socket, waar de kernel het antwoord al gaf.
func TestAPortRefusesAnAppWithoutAValidToken(t *testing.T) {
	fixture := newAttachFixture(t, true)
	address, caPath := fixture.listenTLS(t)
	valid := fixture.token(t)

	for _, testCase := range []struct{ name, token string }{
		{"no token at all", ""},
		{"a token that is simply wrong", "zomaar-iets"},
		{"the token of another app", appproto.Token(mustSecret(t, fixture), "com.stulp.iemandanders")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := attachOverPort(address, caPath, fixture.appID, testCase.token)
			if err == nil {
				t.Fatal("this attach was allowed in")
			}
			// Eén melding voor een onbekende app en een verkeerd token: een poort
			// hoort geen manier te zijn om te ontdekken welke apps hier staan.
			if !strings.Contains(err.Error(), "unknown app or wrong token") &&
				!strings.Contains(err.Error(), "token is required") {
				t.Fatalf("the refusal said too much or too little: %v", err)
			}
		})
	}

	// En met het juiste token wél, zodat deze test niet slaagt doordat de poort
	// helemaal niets doorlaat.
	if err := attachOverPort(address, caPath, fixture.appID, valid); err != nil {
		t.Fatalf("the right token was refused: %v", err)
	}
}

// Een poort zonder TLS. Dat is wat het bewijs met de nonce mogelijk maakt: het
// token gaat niet over de lijn, dus komt er nog steeds niemand binnen die het niet
// kent. Wat wegvalt is geheimhouding, en dat is een andere keuze dan wie erin mag.
func TestAnAppAttachesOverAPlaintextPort(t *testing.T) {
	ctx := context.Background()
	fixture := newAttachFixture(t, true)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	go fixture.apps.ServeAttach(raw)

	if err := fixture.apps.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.waitForState(t, "waiting")

	fixture.launchWith(t,
		"STULP_ATTACH="+raw.Addr().String(),
		"STULP_ATTACH_TOKEN="+fixture.token(t),
		"STULP_ATTACH_PLAINTEXT=1",
	)
	fixture.waitForState(t, "running")

	// En zonder token nog steeds niet, ook al staat er geen TLS omheen.
	if err := attachPlaintext(raw.Addr().String(), fixture.appID, "verkeerd"); err == nil {
		t.Fatal("a plaintext port let an app in without a valid token")
	}
}

func attachPlaintext(address, appID, token string) error {
	raw, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	conn := appproto.NewConn(raw)
	defer conn.Close()
	return appproto.SendAttach(conn, appID, token, plugin.ProtocolVersion, nil)
}

// Een app die het certificaat van Stulp niet kan narekenen hoort niet te
// verbinden. Anders leest wie ertussen gaat zitten alles mee.
func TestAnAppRefusesAStulpItCannotVerify(t *testing.T) {
	fixture := newAttachFixture(t, true)
	address, _ := fixture.listenTLS(t)
	// Geen CA meegegeven: het zelfgemaakte certificaat staat in geen systeemwinkel.
	err := attachOverPort(address, "", fixture.appID, fixture.token(t))
	if err == nil {
		t.Fatal("an app connected to a stulp it could not verify")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the failure was not about the certificate: %v", err)
	}
}

func mustSecret(t *testing.T, fixture *attachFixture) string {
	t.Helper()
	secret, err := fixture.database.AttachSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// attachOverPort doet wat de SDK doet, maar zonder een app te zijn: verbinden,
// zich melden, en het antwoord teruggeven.
func attachOverPort(address, caPath, appID, token string) error {
	settings := &tls.Config{MinVersion: tls.VersionTLS13}
	if caPath != "" {
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return errors.New("the ca file holds no certificate")
		}
		settings.RootCAs = pool
	}
	raw, err := tls.Dial("tcp", address, settings)
	if err != nil {
		return err
	}
	conn := appproto.NewConn(raw)
	defer conn.Close()
	return appproto.SendAttach(conn, appID, token, plugin.ProtocolVersion, nil)
}

// selfSignedCertificate maakt een certificaat voor 127.0.0.1, zodat een app het
// echt kan narekenen in plaats van dat de test het narekenen overslaat.
func selfSignedCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "stulp-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pemBytes
}

// Een onbekende app die zijn manifest meebrengt wordt AANGEBODEN in plaats van
// weggestuurd: HOP heeft hem geplaatst, hij heeft zich gemeld, en wat er nog
// rest is dat een mens hem installeert.
//
// Hij mag dan nog niets: aanbieden is zien, niet vertrouwen. Een gelekt token
// zou anders een sleutel tot het huis zijn.
func TestEenOnbekendeAppMetManifestWordtAangeboden(t *testing.T) {
	fixture := newAttachFixture(t, true)
	nieuw := "com.stulp.nieuw"
	appManifest := []byte(`{"id":"` + nieuw + `","version":"2.1.0","sdk":3,"name":{"en":"Nieuw"}}`)

	err := attachWithManifest(fixture.socket, nieuw, appManifest)
	if err == nil {
		t.Fatal("een aangeboden app mocht meteen draaien")
	}
	if !strings.Contains(err.Error(), "waiting to be installed") {
		t.Fatalf("de reden zegt niet dat hij wacht: %v", err)
	}

	// Wél opgeschreven, met wat hij over zichzelf zei.
	app, err := fixture.database.App(context.Background(), nieuw)
	if err != nil {
		t.Fatalf("de aangeboden app staat niet in het document: %v", err)
	}
	switch {
	case !app.Offered:
		t.Error("hij staat er niet als aangeboden in")
	case app.Enabled:
		t.Error("een aangeboden app hoort niet aan te staan")
	case app.Version != "2.1.0":
		t.Errorf("versie = %q, want 2.1.0 uit het manifest", app.Version)
	case app.Manifest == nil:
		t.Error("het manifest is niet bewaard, en er is geen bundel om het uit te lezen")
	}

	// Nog een keer aanmelden verandert niets: een container met restart:always
	// hoort het document niet elke paar seconden te herschrijven.
	if err := attachWithManifest(fixture.socket, nieuw, appManifest); err == nil {
		t.Fatal("tweede aanmelding werd toegelaten")
	}

	// En na accepteren mag hij wél.
	if _, err := fixture.database.AcceptApp(context.Background(), nieuw); err != nil {
		t.Fatalf("accepteren: %v", err)
	}
	after, err := fixture.database.App(context.Background(), nieuw)
	if err != nil {
		t.Fatal(err)
	}
	if after.Offered || !after.Enabled {
		t.Errorf("na accepteren: offered=%v enabled=%v", after.Offered, after.Enabled)
	}
}

// Een manifest voor een ándere app dan de begroeting zegt, hoort geweigerd te
// worden: anders schrijft een app zich op onder de naam van zijn buurman.
func TestManifestMoetBijDeBegroetingHoren(t *testing.T) {
	fixture := newAttachFixture(t, true)
	err := attachWithManifest(fixture.socket, "com.stulp.een",
		[]byte(`{"id":"com.stulp.twee","version":"1.0.0","sdk":3,"name":{"en":"Twee"}}`))
	if err == nil || !strings.Contains(err.Error(), "sent a manifest for") {
		t.Fatalf("err = %v", err)
	}
	if _, err := fixture.database.App(context.Background(), "com.stulp.twee"); err == nil {
		t.Error("de app schreef zich op onder een andere naam")
	}
}

// Een slot-image IS de release die er staat. Als een geaccepteerde rootless app
// zich na het plaatsen van een nieuw image meldt, moeten versie, drivers en de
// beschrijving van zijn ingebedde UI vóór het starten worden bijgewerkt. Anders
// draait de nieuwe plugin achter de catalogus van de oude.
func TestEenBekendeAangemeldeAppVernieuwtZijnManifest(t *testing.T) {
	fixture := newAttachFixture(t, true)
	ctx := context.Background()
	if _, _, err := fixture.database.UninstallApp(ctx, fixture.appID); err != nil {
		t.Fatal(err)
	}
	first, err := manifest.Parse([]byte(`{
  "id":"com.stulp.attach","version":"1.0.0","sdk":3,
  "drivers":[{"id":"oud","name":{"nl":"Oud"}}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.OfferApp(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.AcceptApp(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	second := []byte(`{
  "id":"com.stulp.attach","version":"2.0.0","sdk":3,
  "drivers":[{"id":"nieuw","name":{"nl":"Nieuw"}}],
  "ui":{"assets":["settings/index.html"]}
}`)
	if err := attachWithManifest(fixture.socket, fixture.appID, second); err != nil {
		t.Fatalf("de bijgewerkte app werd niet toegelaten: %v", err)
	}
	updated, err := fixture.database.App(ctx, fixture.appID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.FromRaw(updated.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != "2.0.0" {
		t.Fatalf("versie = %q", updated.Version)
	}
	if _, ok := parsed.Driver("nieuw"); !ok {
		t.Fatalf("nieuw driver-manifest ontbreekt: %#v", updated.Manifest)
	}
	ui, _ := updated.Manifest["ui"].(map[string]any)
	if assets, _ := ui["assets"].([]any); len(assets) != 1 || assets[0] != "settings/index.html" {
		t.Fatalf("UI-beschrijving ontbreekt: %#v", updated.Manifest)
	}
}

func attachWithManifest(socket, appID string, appManifest []byte) error {
	raw, err := appproto.Dial(socket)
	if err != nil {
		return err
	}
	conn := appproto.NewConn(raw)
	defer conn.Close()
	return appproto.SendAttach(conn, appID, "", plugin.ProtocolVersion, appManifest)
}

// TestAnAppWithoutABundleWaitsForAttach — een aangemelde-en-geaccepteerde app
// heeft geen bundel: zijn binary is niet van ons. Stulp hoort dan te wachten
// tot hij zich (opnieuw) meldt — precies als bij external — en niet de
// crashed-retry-molen in te gaan met "no binary at <id>" per poging. (De
// nil-deref-panic die hier ooit zat is al met het manifest-uit-document-werk
// verholpen; dit pint het kalme gedrag.)
func TestAnAppWithoutABundleWaitsForAttach(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	m, err := manifest.FromRaw(map[string]any{
		"id": "com.test.rootless", "version": "1.0.0", "sdk": 3,
		"name": map[string]any{"en": "Rootless"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.OfferApp(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AcceptApp(ctx, "com.test.rootless"); err != nil {
		t.Fatal(err)
	}
	apps := New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	if err := apps.Start(ctx, "com.test.rootless"); err != nil {
		t.Fatalf("Start van een bundelloze app gaf %v, wil kalm wachten", err)
	}
	if state := apps.State("com.test.rootless"); state.State != "waiting" {
		t.Fatalf("status %#v, wil waiting-op-aanmelding", state)
	}
}
