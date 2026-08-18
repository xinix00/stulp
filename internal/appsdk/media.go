package appsdk

import "sort"

// Camerabeeld en -video.
//
// Een device meldt welke beelden het heeft; de bron wordt pas opgehaald als er
// echt iemand kijkt. Dat is niet alleen zuinig: het adres van een camera draagt
// vaak een token dat verloopt, dus het bij het registreren vastleggen zou een
// adres opleveren dat het later niet meer doet.
//
// De registratie draagt de URL daarom niet. Wat Stulp doorgeeft aan de
// interface is de plek en de titel, en niets waarmee je buiten Stulp om bij de
// camera kunt.
//
// Wat er niet in staat is de kern: geen RTSP, geen demuxer, geen codec. Een
// camera spreekt wat hij spreekt, en dat weet alleen de plugin die hem kent.
// Zou dat hier staan, dan zat elke Stulp-installatie met de bibliotheken van
// elke camera opgescheept -- inclusief de installaties zonder camera. De plugin
// bedient de stream zelf en zegt alleen waar hij staat.

// VideoStream is een stream die de plugin zelf bedient, en wat de browser
// ervan te zien krijgt.
//
// URL is gewoon HTTP: Stulp haalt hem op en geeft de bytes door. Hij hoeft dus
// niet vanaf de browser bereikbaar te zijn -- localhost volstaat, en dat is
// meteen de reden dat het geen redirect is.
type MediaSource struct {
	URL string `json:"url"`
	// ContentType is wat de browser krijgt, bijvoorbeeld "video/mp4" of
	// "image/jpeg".
	ContentType string `json:"contentType,omitempty"`
}

// VideoStream en ImageSource zijn hetzelfde ding onder de naam die bij de plek
// past: een bewegend beeld en een stilstaand beeld komen allebei van een adres
// dat de plugin zelf bedient.
type (
	VideoStream = MediaSource
	ImageSource = MediaSource
)

type mediaSlot struct {
	slot    string
	title   string
	kind    string
	resolve func() (MediaSource, error)
}

// SetCameraVideo meldt dat dit apparaat een videostream heeft.
//
// resolve draait pas als iemand kijkt, en mag dus rustig eerst inloggen, een
// sessie opzetten of een omzetting starten.
func (d *Device) SetCameraVideo(slot, title string, resolve func() (VideoStream, error)) error {
	d.mu.Lock()
	d.media[slot+":video"] = mediaSlot{slot: slot, title: title, kind: "video", resolve: resolve}
	d.mu.Unlock()
	return d.publishMedia()
}

// SetCameraImage meldt dat dit apparaat een stilstaand beeld heeft.
//
// resolve werkt precies als bij video: hij draait pas als iemand kijkt en zegt
// waar het beeld staat, niet wat het is. Zonder resolve was dit een melding
// waar niets achter zat -- de interface bood de plek aan en Stulp kon er niets
// ophalen.
func (d *Device) SetCameraImage(slot, title string, resolve func() (ImageSource, error)) error {
	d.mu.Lock()
	d.media[slot+":image"] = mediaSlot{slot: slot, title: title, kind: "image", resolve: resolve}
	d.mu.Unlock()
	return d.publishMedia()
}

// publishMedia vertelt Stulp wat dit apparaat te bieden heeft.
//
// Duwen en niet wachten tot Stulp het vraagt, en dat is geen optimalisatie maar
// noodzaak. Het afbeeldingsregister loopt over alle apparaten in huis, dus als
// Stulp het bij elke vraag zou ophalen zou hij ook de app aanroepen die op dat
// moment zelf om het register vroeg -- en die staat te wachten op het antwoord.
// Dat liep vast. Nu houdt Stulp de lijst bij en hoeft hij niemand te storen.
func (d *Device) publishMedia() error {
	return d.host.write("media.register", map[string]any{
		"deviceId": d.id, "media": d.registrations(),
	})
}

// mediaRegistration is wat Stulp te horen krijgt: waar het beeld zit, niet hoe
// je erbij komt.
type mediaRegistration struct {
	DeviceID   string `json:"deviceId"`
	Slot       string `json:"slot"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resourceId"`
}

func (d *Device) registrations() []mediaRegistration {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.media))
	for key := range d.media {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]mediaRegistration, 0, len(keys))
	for _, key := range keys {
		entry := d.media[key]
		out = append(out, mediaRegistration{
			DeviceID: d.id, Slot: entry.slot, Title: entry.title,
			Kind: entry.kind, ResourceID: d.id + ":" + key,
		})
	}
	return out
}

// resolveMedia zoekt de plek op van wat er gevraagd wordt.
//
// Eén weg voor beeld en video: wat Stulp doet is in beide gevallen hetzelfde --
// de bytes ophalen en doorgeven -- en het scheelt een tweede protocolmethode die
// precies zo werkt als deze.
//
// Het soort staat er wel bij, en dat is niet vrijblijvend. Een plugin mag een
// stilstaand beeld en een stream onder dezelfde naam aanmelden (examples/virtual
// doet dat), en zonder soort was de video altijd het antwoord: de foto was dan
// onbereikbaar en een pushbericht zou videobytes als afbeelding meesturen. Een
// leeg soort betekent nog steeds "video, en anders het beeld", want dat is wat de
// interface vraagt als ze een adres wil zonder te kiezen.
func (d *Device) resolveMedia(slot, kind string) (MediaSource, bool, error) {
	d.mu.Lock()
	entry, ok := d.media[slot+":"+kind]
	if kind == "" {
		entry, ok = d.media[slot+":video"]
		if !ok {
			entry, ok = d.media[slot+":image"]
		}
	}
	d.mu.Unlock()
	if !ok || entry.resolve == nil {
		return MediaSource{}, false, nil
	}
	source, err := entry.resolve()
	return source, true, err
}

// mediaKindName maakt een foutmelding leesbaar: "has no image in slot" leest
// beter dan "has no  in slot".
func mediaKindName(kind string) string {
	switch kind {
	case "image":
		return "image"
	case "video":
		return "video"
	}
	return "media"
}
