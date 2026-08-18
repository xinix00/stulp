package wiim

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// AVTransport is de dienst waar de standen vandaan komen.
const AVTransport = "urn:schemas-upnp-org:service:AVTransport:1"

// getInfoEx is geen actie uit de UPnP-standaard maar een uitbreiding van
// LinkPlay. De bron gebruikt hem (`callAction('AVTransport', 'GetInfoEx')`) en
// dat is hier de reden om hem ook te gebruiken: hij levert in één aanroep alles
// wat de tegel nodig heeft — stand, loopmode, tijd, volume, demping en de
// metadata van het nummer. De standaardacties doen dat niet; daar zijn er drie
// of vier voor nodig, en dat is drie of vier keer per ronde per speler.
const getInfoEx = "GetInfoEx"

// Description is wat een speler over zichzelf vertelt.
//
// Alleen de velden die hier ergens landen. Een echte beschrijving draagt er
// meer (iconen, presentationURL, serialNumber); die staan in PORTED.md.
type Description struct {
	// Location is waar deze beschrijving vandaan kwam. Relatieve adressen in de
	// beschrijving worden hiertegen opgelost.
	Location     string
	URLBase      string
	DeviceType   string
	FriendlyName string
	Manufacturer string
	ModelName    string
	ModelNumber  string
	UDN          string
	Services     []Service
}

// Service is één dienst uit de beschrijving.
type Service struct {
	Type       string
	ID         string
	ControlURL string
}

// UUID is de identiteit van de speler: de UDN zonder het voorvoegsel `uuid:`.
//
// Dat is precies wat de bron als `data.id` bewaart — daar komt hij uit een
// mDNS-veld (`device.id.split(':')[1]`), hier uit de beschrijving. Zelfde
// waarde, dus een speler die eerder gekoppeld was blijft dezelfde.
func (d Description) UUID() string {
	return strings.TrimPrefix(strings.TrimSpace(d.UDN), "uuid:")
}

// IsPlayer zegt of dit een speler is waar deze app iets mee kan.
//
// Een MediaRenderer is in een gemiddeld huis ook een tv, een soundbar of een
// printer met muziekpretenties, dus het merk moet erbij. Waar dat aan te zien
// is komt uit de bron: `_docs/upnpSpecs/_DeviceDescription.json` van een echte
// WiiM Pro noemt zich `Linkplay Technology Inc.` en draagt een eigen dienst
// `urn:schemas-wiimu-com:service:PlayQueue:1`, en de ontdekking in
// `.homeycompose/discovery/player.json` zoekt op de mDNS-naam `linkplay`.
//
// Zonder AVTransport valt er niets te bedienen, dus die eis staat er los bij.
func (d Description) IsPlayer() bool {
	if _, ok := d.Control(AVTransport); !ok {
		return false
	}
	if strings.Contains(strings.ToLower(d.Manufacturer), "linkplay") {
		return true
	}
	if strings.Contains(strings.ToLower(d.ModelName), "wiim") {
		return true
	}
	for _, service := range d.Services {
		if strings.Contains(strings.ToLower(service.Type), "wiimu-com") {
			return true
		}
	}
	return false
}

// Control levert het adres waar een dienst zijn opdrachten aanneemt, opgelost
// tegen de plek waar de beschrijving stond.
//
// Opgelost en niet zelf samengesteld: een beschrijving mag een relatief pad
// geven (`/upnp/control/rendertransport1`), een volledig adres, of een
// `URLBase` waar alles tegenaan hoort. Zelf een pad verzinnen werkt tot de
// eerste speler die het anders doet.
func (d Description) Control(serviceType string) (string, bool) {
	for _, service := range d.Services {
		if !strings.EqualFold(service.Type, serviceType) || service.ControlURL == "" {
			continue
		}
		resolved, err := resolve(d.Location, d.URLBase, service.ControlURL)
		if err != nil {
			return "", false
		}
		return resolved, true
	}
	return "", false
}

func resolve(location, base, reference string) (string, error) {
	against := base
	if against == "" {
		against = location
	}
	root, err := url.Parse(against)
	if err != nil {
		return "", fmt.Errorf("%q is geen adres: %w", against, err)
	}
	target, err := url.Parse(reference)
	if err != nil {
		return "", fmt.Errorf("%q is geen adres: %w", reference, err)
	}
	return root.ResolveReference(target).String(), nil
}

// DescriptionURL is waar de beschrijving van deze speler staat.
func (c *Client) DescriptionURL() string {
	return "http://" + net.JoinHostPort(c.Address, strconv.Itoa(c.port())) + DescriptionPath
}

// Describe haalt de beschrijving op.
func (c *Client) Describe(ctx context.Context) (Description, error) {
	return describe(ctx, c.client(), c.DescriptionURL())
}

func describe(ctx context.Context, client *http.Client, location string) (Description, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return Description{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Description{}, fmt.Errorf("%s: geen antwoord: %w", location, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return Description{}, fmt.Errorf("%s: %w", location, err)
	}
	if len(body) > maxBody {
		return Description{}, fmt.Errorf("%s: de beschrijving is groter dan %d bytes", location, maxBody)
	}
	if response.StatusCode >= 400 {
		return Description{}, fmt.Errorf("%s: antwoordde %d", location, response.StatusCode)
	}
	return ParseDescription(location, body)
}

// ParseDescription leest de device-beschrijving van UPnP.
func ParseDescription(location string, body []byte) (Description, error) {
	var document struct {
		URLBase string `xml:"URLBase"`
		Device  struct {
			DeviceType   string `xml:"deviceType"`
			FriendlyName string `xml:"friendlyName"`
			Manufacturer string `xml:"manufacturer"`
			ModelName    string `xml:"modelName"`
			ModelNumber  string `xml:"modelNumber"`
			UDN          string `xml:"UDN"`
			Services     []struct {
				Type       string `xml:"serviceType"`
				ID         string `xml:"serviceId"`
				ControlURL string `xml:"controlURL"`
			} `xml:"serviceList>service"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal(body, &document); err != nil {
		return Description{}, fmt.Errorf("%s: dit is geen UPnP-beschrijving: %w", location, err)
	}
	found := Description{
		Location:     location,
		URLBase:      strings.TrimSpace(document.URLBase),
		DeviceType:   strings.TrimSpace(document.Device.DeviceType),
		FriendlyName: strings.TrimSpace(document.Device.FriendlyName),
		Manufacturer: strings.TrimSpace(document.Device.Manufacturer),
		ModelName:    strings.TrimSpace(document.Device.ModelName),
		ModelNumber:  strings.TrimSpace(document.Device.ModelNumber),
		UDN:          strings.TrimSpace(document.Device.UDN),
	}
	for _, service := range document.Device.Services {
		found.Services = append(found.Services, Service{
			Type:       strings.TrimSpace(service.Type),
			ID:         strings.TrimSpace(service.ID),
			ControlURL: strings.TrimSpace(service.ControlURL),
		})
	}
	if found.UDN == "" {
		return Description{}, fmt.Errorf("%s: deze beschrijving heeft geen UDN", location)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// De stand
// ---------------------------------------------------------------------------

// Status is wat GetInfoEx over de speler vertelt, vertaald naar wat de tegel
// laat zien.
type Status struct {
	// State is CurrentTransportState: PLAYING, PAUSED_PLAYBACK, STOPPED,
	// NO_MEDIA_PRESENT, TRANSITIONING.
	State string
	// Volume is 0..1, zoals Stulp het kent.
	Volume float64
	Muted  bool
	Loop   Loop
	// Duration en Position komen van TrackDuration en RelTime.
	Duration Seconds
	Position Seconds
	Track    Track
}

// Playing volgt de bron: alleen PLAYING telt als spelen. TRANSITIONING is de
// speler die aan het inladen is, en dat is nog geen geluid.
func (s Status) Playing() bool { return s.State == "PLAYING" }

// Seconds is een tijd die de speler ook niet hoeft te weten. Radio meldt
// geregeld NOT_IMPLEMENTED, en dat is iets anders dan nul seconden.
type Seconds struct {
	Value float64
	Known bool
}

// Loop is shuffle en herhalen, die bij WiiM in één getal zitten.
type Loop struct {
	// Raw is het getal zoals de speler het gaf, ook als het onbekend is — dan
	// staat het tenminste in de melding.
	Raw     string
	Shuffle bool
	Repeat  string
	Known   bool
}

// Track is wat er nu speelt.
type Track struct {
	Artist string
	Album  string
	Title  string
	ArtURI string
	// Present is onwaar als de speler geen metadata meestuurde. Dan hoort er
	// niets te staan in plaats van wat er de vorige keer stond.
	Present bool
}

// Status vraagt de speler hoe hij ervoor staat.
func (c *Client) Status(ctx context.Context) (Status, error) {
	control, err := c.controlURL(ctx)
	if err != nil {
		return Status{}, err
	}
	fields, err := c.soap(ctx, control, AVTransport, getInfoEx, map[string]string{"InstanceID": "0"})
	if err != nil {
		// Het onthouden adres loslaten: de volgende ronde vraagt de speler
		// opnieuw waar zijn diensten staan. Een speler die na een update iets
		// verhangt herstelt zich zo vanzelf, en het kost alleen iets in het
		// geval dat toch al misgaat.
		c.mu.Lock()
		c.control = ""
		c.mu.Unlock()
		return Status{}, err
	}
	return ParseStatus(fields)
}

// controlURL levert het adres van AVTransport, en haalt de beschrijving op als
// het nog niet bekend is.
func (c *Client) controlURL(ctx context.Context) (string, error) {
	c.mu.Lock()
	known := c.control
	c.mu.Unlock()
	if known != "" {
		return known, nil
	}

	description, err := c.Describe(ctx)
	if err != nil {
		return "", err
	}
	control, ok := description.Control(AVTransport)
	if !ok {
		return "", fmt.Errorf("%s biedt geen AVTransport aan; is dit een WiiM?", c.Address)
	}
	c.mu.Lock()
	c.control = control
	c.mu.Unlock()
	return control, nil
}

// soap voert één UPnP-actie uit en levert de velden uit het antwoord.
func (c *Client) soap(ctx context.Context, control, service, action string, arguments map[string]string) (map[string]string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u=%q>`, action, service)
	for name, value := range arguments {
		escaped := &strings.Builder{}
		xml.EscapeText(escaped, []byte(value))
		fmt.Fprintf(&body, "<%s>%s</%s>", name, escaped.String(), name)
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, control, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	request.Header.Set("SOAPACTION", `"`+service+"#"+action+`"`)

	response, err := c.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: de speler antwoordt niet: %w", action, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if len(answer) > maxBody {
		return nil, fmt.Errorf("%s: de speler stuurde meer dan %d bytes", action, maxBody)
	}
	// Een SOAP-fout komt met 500 en staat in de body; die is bruikbaarder dan
	// de code, dus eerst lezen en dan pas klagen.
	fields, parseErr := parseSOAP(action, answer)
	if parseErr != nil {
		if response.StatusCode >= 400 {
			return nil, fmt.Errorf("%s: de speler antwoordde %d: %w", action, response.StatusCode, parseErr)
		}
		return nil, parseErr
	}
	return fields, nil
}

// parseSOAP haalt de velden uit het antwoord op een actie.
//
// Veld voor veld en niet in een struct: welke velden GetInfoEx precies stuurt
// staat nergens vast — de bron leest er zeven van, en een speler die er meer
// stuurt hoort daar niet op te struikelen.
func parseSOAP(action string, body []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: het antwoord is geen XML: %w", action, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case action + "Response":
			return soapFields(decoder, start)
		case "Fault":
			return nil, soapFault(action, decoder, start)
		}
	}
	return nil, fmt.Errorf("%s: het antwoord bevat geen %sResponse", action, action)
}

func soapFields(decoder *xml.Decoder, start xml.StartElement) (map[string]string, error) {
	fields := map[string]string{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			var text string
			if err := decoder.DecodeElement(&text, &element); err != nil {
				return nil, err
			}
			fields[element.Name.Local] = text
		case xml.EndElement:
			if element.Name == start.Name {
				return fields, nil
			}
		}
	}
}

// soapFault maakt van een UPnP-fout een melding waar iemand iets aan heeft.
func soapFault(action string, decoder *xml.Decoder, start xml.StartElement) error {
	var fault struct {
		String string `xml:"faultstring"`
		Detail struct {
			Code        string `xml:"UPnPError>errorCode"`
			Description string `xml:"UPnPError>errorDescription"`
		} `xml:"detail"`
	}
	if err := decoder.DecodeElement(&fault, &start); err != nil {
		return fmt.Errorf("%s: de speler meldde een fout die niet te lezen is: %w", action, err)
	}
	switch {
	case fault.Detail.Description != "":
		return fmt.Errorf("%s: de speler weigerde: %s (%s)", action, fault.Detail.Description, fault.Detail.Code)
	case fault.String != "":
		return fmt.Errorf("%s: de speler weigerde: %s", action, fault.String)
	}
	return fmt.Errorf("%s: de speler weigerde zonder te zeggen waarom", action)
}

// ParseStatus vertaalt de velden van GetInfoEx.
//
// De zeven velden die hieronder gehaald worden zijn precies de zeven die de
// bron gebruikt (`onDeviceGetInfoEx`). Ontbreekt er één, dan is dit niet het
// antwoord waar deze app op rekent, en dat hoort te vallen met de naam van het
// veld erbij — een tegel die stil op nul blijft staan kost meer tijd.
func ParseStatus(fields map[string]string) (Status, error) {
	need := func(name string) (string, error) {
		value, ok := fields[name]
		if !ok {
			return "", fmt.Errorf("het antwoord van de speler mist %s", name)
		}
		return strings.TrimSpace(value), nil
	}

	state, err := need("CurrentTransportState")
	if err != nil {
		return Status{}, err
	}
	loopRaw, err := need("LoopMode")
	if err != nil {
		return Status{}, err
	}
	durationRaw, err := need("TrackDuration")
	if err != nil {
		return Status{}, err
	}
	positionRaw, err := need("RelTime")
	if err != nil {
		return Status{}, err
	}
	volumeRaw, err := need("CurrentVolume")
	if err != nil {
		return Status{}, err
	}
	muteRaw, err := need("CurrentMute")
	if err != nil {
		return Status{}, err
	}
	metadata, err := need("TrackMetaData")
	if err != nil {
		return Status{}, err
	}

	volume, err := strconv.ParseFloat(volumeRaw, 64)
	if err != nil {
		return Status{}, fmt.Errorf("CurrentVolume %q is geen getal", volumeRaw)
	}
	duration, err := ParseTime(durationRaw)
	if err != nil {
		return Status{}, err
	}
	position, err := ParseTime(positionRaw)
	if err != nil {
		return Status{}, err
	}

	status := Status{
		State:    state,
		Volume:   Volume(volume),
		Muted:    muteRaw == "1",
		Duration: duration,
		Position: position,
	}
	status.Loop.Raw = loopRaw
	status.Loop.Shuffle, status.Loop.Repeat, status.Loop.Known = ParseLoopMode(loopRaw)

	// Zonder medium is er geen nummer. De bron kijkt naar dezelfde stand en
	// wist dan artiest, album en titel — beter leeg dan wat er tien minuten
	// geleden speelde.
	if state != "NO_MEDIA_PRESENT" && metadata != "" {
		track, err := ParseTrackMetadata(metadata)
		if err != nil {
			return Status{}, err
		}
		status.Track = track
	}
	return status, nil
}

// ParseTime leest een tijd als 00:03:21 uit.
//
// De bron rekent hier mis: `#convertTimeToNumber` doet
// `uren*216000 + minuten*3600 + seconden*60`, en dat is elke term zestig keer
// te groot — speaker_duration en speaker_position tellen in seconden. Hier
// staat de gewone omrekening; zie PORTED.md.
//
// NOT_IMPLEMENTED en een lege waarde zijn geen fout maar een tijd die de speler
// niet weet: dat gebeurt bij radio en bij een lege wachtrij.
func ParseTime(raw string) (Seconds, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.EqualFold(value, "NOT_IMPLEMENTED") {
		return Seconds{}, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return Seconds{}, fmt.Errorf("%q is geen tijd van de vorm uu:mm:ss", raw)
	}
	total := 0.0
	for index, part := range parts {
		// De seconden mogen een breuk dragen; sommige spelers sturen
		// 00:03:21.500. De uren en minuten niet.
		number, err := strconv.ParseFloat(part, 64)
		if err != nil || number < 0 {
			return Seconds{}, fmt.Errorf("%q is geen tijd van de vorm uu:mm:ss", raw)
		}
		switch index {
		case 0:
			total += number * 3600
		case 1:
			total += number * 60
		default:
			total += number
		}
	}
	return Seconds{Value: total, Known: true}, nil
}
