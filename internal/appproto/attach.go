package appproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// De attach-socket is de tweede manier waarop een app en Stulp aan elkaar komen.
//
// De eerste is een socketpair als fd 3: Stulp start de binary en het bezit van
// die fd is het bewijs dat hij dat gedaan heeft. Er is geen adres, dus er is
// niets om te adresseren, en er valt niets te authenticeren.
//
// Attach is het omgekeerde: de app draait al -- gestart door Docker, systemd of
// een debugger -- en meldt zich bij Stulp. Dat kan niet zonder adres, en een
// adres kan iedereen bereiken die erbij kan. Vandaar drie sloten, in deze
// volgorde:
//
//  1. De socket staat in een map van 0700 die Stulp zelf maakt.
//  2. De socket zelf krijgt 0600.
//  3. Bij elke verbinding wordt de uid van de andere kant nagegaan (PeerUID).
//
// Alleen de derde is een echt slot. De eerste twee zijn er omdat een verkeerd
// gezette umask of een gedeelde map anders het enige zou zijn wat ertussen
// staat. Een uid die niet die van Stulp is komt er niet in -- ook niet als hij
// bij het pad kan.
//
// Geen TCP en geen HTTP. Een app die inbelt is volledig vertrouwd: hij schrijft
// apparaten, instellingen en state, en hij vuurt Flows af. Dat vertrouwen hoort
// niet aan een poort te hangen waar een token het enige verschil is, zolang de
// kernel het antwoord gratis geeft.

// De begroeting is drie berichten, en Stulp begint.
//
//	stulp -> app  {"protocol":1,"nonce":"..."}
//	app -> stulp  {"appId":"com.stulp.weather","protocol":1,"proof":"...","nonce":"..."}
//	stulp -> app  {"ok":true,"proof":"..."}
//
// Stulp begint omdat hij de eerste nonce moet stellen: de app bewijst zich met een
// nummer dat hij niet zelf gekozen heeft, en daarom is dat bewijs nergens anders
// bruikbaar. In hetzelfde bericht stelt de app zijn eigen nonce, en bewijst Stulp
// zich terug. Drie berichten, één rondgang meer dan een token opsturen, en het
// token blijft waar het hoort.
//
// Over een unix-socket blijven de bewijzen leeg: daar heeft de kernel de vraag al
// beantwoord en is er geen geheim in het spel.

// AttachHello is wat Stulp als eerste zegt.
type AttachHello struct {
	Protocol int `json:"protocol"`
	// Nonce is waarmee de app zich moet bewijzen. Leeg over een unix-socket.
	Nonce string `json:"nonce,omitempty"`
}

// Attach is de begroeting waarmee een al draaiende app zich meldt.
//
// Hij is er om één vraag te beantwoorden die bij een socketpair niet bestaat: wie
// ben je? Bij een gespawnde app weet Stulp dat, want hij startte hem. Hier zegt de
// app het, en daarom moet Stulp het narekenen -- de app-id komt uit het document en
// niet uit dit bericht.
type Attach struct {
	AppID    string `json:"appId"`
	Protocol int    `json:"protocol"`
	// Manifest is de app.json van deze app, zoals hij zelf hem draagt.
	//
	// Een app met een bundel op schijf hoeft hem niet te sturen: Stulp leest hem
	// daar. Een app die als image geplaatst is heeft geen map om hem uit te
	// lezen, en dan is dit de enige plek waar hij staat -- en precies wat Stulp
	// nodig heeft om een onbekende app te kunnen aanbieden in plaats van hem weg
	// te sturen.
	Manifest json.RawMessage `json:"manifest,omitempty"`
	// Proof bewijst dat deze app het token van deze app kent, zonder dat token te
	// sturen. Alleen nodig over een poort: daar is geen uid om na te rekenen.
	Proof string `json:"proof,omitempty"`
	// Nonce is waarmee Stulp zich op zijn beurt bewijst. Zonder dit kan iemand zich
	// als Stulp voordoen en een app apparaten en instellingen voorschotelen.
	Nonce string `json:"nonce,omitempty"`
}

// AttachReply is wat Stulp terugzegt. Een geweigerde attach krijgt een zin en
// niet een dichtgegooide verbinding: wie een app in een container zet leest
// alleen de log van die container, en "onbekende app com.stulp.wather" is daar
// het hele verschil met stilte.
type AttachReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Proof is het antwoord op de nonce van de app.
	Proof string `json:"proof,omitempty"`
}

// maxAttachSize begrenst de begroeting. Die draagt sinds het aanmelden ook het
// MANIFEST van de app (een slot-image heeft geen app.json op schijf), en dat
// is zijn hele kaartcatalogus mét locales — weather is 68KB. De oude 4KB was
// geschreven toen hier alleen een app-id en een getal in zaten, en weigerde
// op het ijzer precies de apps met een echte catalogus (gemeten 14-08:
// "greeting of 4978 bytes is too large"). 256KB draagt élk reëel manifest en
// blijft een grens waar een verbinding die iets ánders stuurt niets kost.
const maxAttachSize = 256 << 10

// SendAttach doet de hele begroeting vanaf de kant van de app: hij wacht op de
// nonce van Stulp, bewijst zich, en rekent na dat Stulp zich terugbewijst.
//
// Een leeg token betekent een unix-socket: dan blijven de bewijzen weg, want daar
// heeft de kernel de vraag al beantwoord.
func SendAttach(conn *Conn, appID, token string, protocol int, appManifest []byte) error {
	hello, err := readAttachHello(conn)
	if err != nil {
		return err
	}
	if hello.Protocol != 0 && hello.Protocol != protocol {
		return fmt.Errorf("attach: stulp speaks protocol %d, this binary speaks %d", hello.Protocol, protocol)
	}

	greeting := Attach{AppID: appID, Protocol: protocol, Manifest: appManifest}
	if token != "" {
		if hello.Nonce == "" {
			return errors.New("attach: stulp asked for no proof, so this connection is not the one this token is for")
		}
		greeting.Proof = AppProof(token, hello.Nonce, appID)
		// Onze eigen nonce, zodat Stulp zich ook moet bewijzen.
		nonce, err := Nonce()
		if err != nil {
			return err
		}
		greeting.Nonce = nonce
	}

	body, err := json.Marshal(greeting)
	if err != nil {
		return err
	}
	if err := conn.WriteRaw(body); err != nil {
		return fmt.Errorf("attach: greeting could not be sent: %w", err)
	}

	answer, err := conn.ReadRaw()
	if err != nil {
		return fmt.Errorf("attach: no answer from stulp: %w", err)
	}
	var reply AttachReply
	if err := json.Unmarshal(answer, &reply); err != nil {
		return fmt.Errorf("attach: unreadable answer from stulp: %w", err)
	}
	if !reply.OK {
		if reply.Error == "" {
			return errors.New("attach: refused without a reason")
		}
		return fmt.Errorf("attach refused: %s", reply.Error)
	}
	// Pas nu is de andere kant Stulp. Zonder deze regel is een goedgekeurde attach
	// alleen het woord van wie er opnam.
	if token != "" && !CheckStulpProof(token, appID, greeting.Nonce, reply.Proof) {
		return errors.New("attach: the other end accepted us but cannot prove it is stulp")
	}
	return nil
}

// Greet is het eerste bericht van Stulp: zijn protocolversie, en de nonce waarmee
// de app zich moet bewijzen. Een lege nonce vraagt geen bewijs.
func Greet(conn *Conn, protocol int, nonce string) error {
	body, err := json.Marshal(AttachHello{Protocol: protocol, Nonce: nonce})
	if err != nil {
		return err
	}
	return conn.WriteRaw(body)
}

func readAttachHello(conn *Conn) (AttachHello, error) {
	body, err := conn.ReadRaw()
	if err != nil {
		return AttachHello{}, fmt.Errorf("attach: stulp said nothing: %w", err)
	}
	if len(body) > maxAttachSize {
		return AttachHello{}, fmt.Errorf("attach: greeting of %d bytes is too large", len(body))
	}
	var hello AttachHello
	if err := json.Unmarshal(body, &hello); err != nil {
		return AttachHello{}, fmt.Errorf("attach: unreadable greeting from stulp: %w", err)
	}
	return hello, nil
}

// ReadAttach leest de begroeting van een app die zich meldt.
func ReadAttach(conn *Conn) (Attach, error) {
	body, err := conn.ReadRaw()
	if err != nil {
		return Attach{}, err
	}
	if len(body) > maxAttachSize {
		return Attach{}, fmt.Errorf("attach: greeting of %d bytes is too large", len(body))
	}
	var greeting Attach
	if err := json.Unmarshal(body, &greeting); err != nil {
		return Attach{}, fmt.Errorf("attach: unreadable greeting: %w", err)
	}
	if greeting.AppID == "" {
		return Attach{}, errors.New("attach: greeting without an app id")
	}
	return greeting, nil
}

// WriteAttachReply stuurt het antwoord op een begroeting.
func WriteAttachReply(conn *Conn, reply AttachReply) error {
	body, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return conn.WriteRaw(body)
}

// Listen opent de attach-socket.
//
// Een bestaande socket op dit pad wordt opgeruimd -- een Stulp die niet netjes
// gestopt is laat er een achter, en dan is de volgende start "address already in
// use". Alleen als het een socket is: een gewoon bestand op dit pad is een
// vergissing van degene die het pad koos, en die verwijderen we niet.
func Listen(path string) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("attach socket directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("attach: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("attach: stale socket at %s: %w", path, err)
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Na het binden, want de umask bepaalt anders wat de socket krijgt. Dit is
	// het tweede slot van de drie; het derde is PeerUID bij elke verbinding.
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("attach socket permissions: %w", err)
	}
	return listener, nil
}

// Dial verbindt met de attach-socket van Stulp.
func Dial(path string) (net.Conn, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("attach: cannot reach stulp at %s: %w", path, err)
	}
	return conn, nil
}

// CheckPeer gaat na dat de andere kant van de verbinding dezelfde gebruiker is
// als deze.
//
// Dit is het slot waar het echt op aankomt. De kernel weet wie er aan de andere
// kant zit en liegt daar niet over, dus is er geen token nodig -- en geen token
// betekent geen sleutel die in een image, een env-dump of een log kan
// belanden.
func CheckPeer(conn net.Conn) error {
	uid, err := PeerUID(conn)
	if err != nil {
		return err
	}
	if uid != uint32(os.Getuid()) {
		return fmt.Errorf("attach: refused: the other end runs as uid %d, stulp as %d", uid, os.Getuid())
	}
	return nil
}
