package appsdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/xinix00/stulp/internal/appproto"
)

// Een app die zich zelf meldt.
//
// De gewone weg is dat Stulp deze binary start en er een socketpair aan hangt. Dan
// is er niets te kiezen: de verbinding is er al voordat main begint.
//
// Deze weg is voor een app die al draait als Stulp hem nog niet kent. Draait de
// app in een container, dan bestaat er geen moment waarop Stulp hem had kunnen
// starten -- de orkestrator doet dat. En onder een debugger wil je hem juist zelf
// starten, met een breakpoint erin, zonder dat een herstart van Stulp hem
// onderuit haalt.
//
// Twee soorten adres, en dat verschil is niet cosmetisch:
//
//	STULP_ATTACH=/run/stulp/attach.sock       een unix-socket
//	STULP_ATTACH=stulp.default.svc:9443       een poort
//
// De socket is dezelfde machine of dezelfde pod, en dan rekent de kernel na wie er
// verbindt. Een poort kan van overal komen, en dan bewijst deze app zich met het
// token uit STULP_ATTACH_TOKEN -- niet door het op te sturen, maar door de HMAC te
// geven van een nonce die Stulp stelt. Het token blijft dus waar het staat, en wie
// meeleest heeft aan het antwoord de volgende keer niets.
//
// Dat regelt wie er binnenkomt, en niet wie er meeleest. Voor het tweede is er TLS,
// en dat is een aparte keuze -- zie STULP_ATTACH_PLAINTEXT.

// AttachConfig is waar Stulp te vinden is en waarmee deze app zich bewijst.
type AttachConfig struct {
	// Target is een absoluut pad naar een unix-socket, of host:poort.
	Target string
	// AppID is leeg als hij uit app.json in de werkmap gelezen mag worden.
	AppID string
	// Token hoort bij een poort en blijft leeg bij een unix-socket. Hij gaat niet
	// over de lijn; er wordt mee gerekend.
	Token string
	// CACert is het certificaat waarmee het certificaat van Stulp na te rekenen
	// valt. Leeg betekent de certificaten die het systeem vertrouwt, wat voor een
	// zelfgemaakt certificaat niet genoeg is.
	CACert string
	// Insecure slaat het narekenen van dat certificaat over. Om te ontwikkelen: wie
	// ertussen kan gaan zitten, leest dan alles mee -- inbreken kan hij nog niet,
	// want daar is het token voor nodig.
	Insecure bool
	// Manifest is de app.json van deze app. Een app met een bundel op schijf mag
	// hem leeg laten -- Stulp leest hem daar. Een app die als image geplaatst is
	// (een HopOS-slot, een pod met alleen de binary) heeft geen map om hem uit te
	// lezen, en dan is dit hoe Stulp weet wat hij is: bak hem mee met go:embed.
	Manifest []byte

	// Plaintext laat TLS helemaal weg. Het bewijs met de nonce blijft, dus er komt
	// nog steeds niemand binnen die het token niet kent, maar alles wat er daarna
	// over de lijn gaat ligt open: apparaatnamen, waarden, en de sleutels die deze
	// app met SetSetting bewaart.
	Plaintext bool
}

// AttachConfigFromEnv leest waar en hoe deze app zich moet melden.
func AttachConfigFromEnv() AttachConfig {
	return AttachConfig{
		Target:    os.Getenv("STULP_ATTACH"),
		AppID:     os.Getenv("STULP_APP_ID"),
		Token:     os.Getenv("STULP_ATTACH_TOKEN"),
		CACert:    os.Getenv("STULP_ATTACH_CA"),
		Insecure:  os.Getenv("STULP_ATTACH_INSECURE") == "1",
		Plaintext: os.Getenv("STULP_ATTACH_PLAINTEXT") == "1",
	}
}

// Attach meldt deze app bij Stulp en draait hem daarna zoals Serve dat doet.
func Attach(config AttachConfig, plugin Plugin) error {
	if config.Target == "" {
		return errors.New("attach: no address for stulp")
	}
	if config.AppID == "" {
		id, err := appIDFromManifest()
		if err != nil {
			return err
		}
		config.AppID = id
	}
	remote := !isSocketPath(config.Target)
	if remote && config.Token == "" {
		// Nu falen en niet straks: Stulp weigert een aanmelding zonder bewijs met
		// dezelfde zin als een verkeerd bewijs, en dan is niet te zien dat het
		// probleem hier zat.
		return errors.New("attach: a token is required to attach over a port; set STULP_ATTACH_TOKEN")
	}
	manifest, err := manifestWithUI(config.Manifest, plugin.UI)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	raw, err := dial(config)
	if err != nil {
		return err
	}
	conn := appproto.NewConn(raw)
	// Over een unix-socket blijft het token buiten de begroeting: daar heeft de
	// kernel de vraag al beantwoord.
	token := ""
	if remote {
		token = config.Token
	}
	// Een geweigerde aanmelding komt terug als de zin die Stulp gaf -- onbekende
	// app, uitgezet, of al bezet. Die zin is het enige wat in de log van deze
	// container staat, dus hij hoort er heel in te staan.
	if err := appproto.SendAttach(conn, config.AppID, token, ProtocolVersion, manifest); err != nil {
		conn.Close()
		return err
	}
	return serve(conn, plugin)
}

// dial opent de verbinding.
//
// Een unix-socket en een poort zonder TLS gaan hier langs; TLS staat in een eigen
// bestand, zodat een plugin die het niet nodig heeft crypto/tls niet meelinkt. Dat
// is bijna een megabyte aan een binary die het nooit aanroept.
func dial(config AttachConfig) (net.Conn, error) {
	if isSocketPath(config.Target) {
		return appproto.Dial(config.Target)
	}
	if config.Plaintext {
		conn, err := net.Dial("tcp", config.Target)
		if err != nil {
			return nil, fmt.Errorf("attach: cannot reach stulp at %s: %w", config.Target, err)
		}
		return conn, nil
	}
	return dialTLS(config)
}

// isSocketPath onderscheidt een pad van een adres.
//
// Een absoluut pad, of een dat met ./ begint. Alles anders is host:poort -- een
// los woord is een hostnaam en geen bestandsnaam, want een socket zonder map is
// niet iets wat je in een deployment zet.
func isSocketPath(target string) bool {
	return strings.HasPrefix(target, "/") || strings.HasPrefix(target, "./") ||
		strings.HasPrefix(target, "../")
}

// appIDFromManifest leest de id uit app.json in de werkmap.
//
// Alleen dat ene veld, met encoding/json dat er toch al in zit: het manifest komt
// bij de handshake compleet van Stulp, dus hier hoeft niets van gecontroleerd te
// worden behalve dat er een id staat om je mee te melden.
func appIDFromManifest() (string, error) {
	path := "app.json"
	if directory, err := os.Getwd(); err == nil {
		path = filepath.Join(directory, "app.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("attach: no app id given and %s could not be read: %w", path, err)
	}
	var appManifest struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &appManifest); err != nil {
		return "", fmt.Errorf("attach: %s is unreadable: %w", path, err)
	}
	if appManifest.ID == "" {
		return "", fmt.Errorf("attach: %s has no id; set STULP_APP_ID instead", path)
	}
	return appManifest.ID, nil
}
