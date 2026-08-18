// Package spotify praat met de Spotify Web API: welke Connect-apparaten er zijn,
// wat er speelt, en wat er moet gaan spelen.
//
// Bewust klein gehouden. Deze app doet één ding -- een nummer op een apparaat
// afspelen en dat apparaat bedienen -- en alles wat daar niet voor nodig is
// staat er niet in. Wat er wél is, is afgelezen van de API en niet geraden;
// PORTED.md zegt per eindpunt wat er getoetst is.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBody begrenst wat er van een antwoord gelezen wordt. Een zoekopdracht met
// vijftig treffers blijft daar ruim onder; een antwoord dat dat overschrijdt is
// geen antwoord dat deze app verwacht.
const maxBody = 4 << 20

const apiBase = "https://api.spotify.com/v1"

// DefaultHTTP is de client die deze app gebruikt: een deadline op het geheel,
// zodat een aanroep die blijft hangen niet een hele poll ophoudt.
func DefaultHTTP() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Client is de API-kant. Token levert een geldig access-token; hoe dat geldig
// blijft is de zorg van de aanroeper.
type Client struct {
	Token   func(ctx context.Context) (string, error)
	HTTP    *http.Client
	BaseURL string // leeg is de echte API; een test zet er zijn eigen server neer
}

// Device is één Spotify Connect-apparaat.
//
// Alleen wat deze app gebruikt. Wat er verder in het antwoord zit --
// is_private_session, supports_volume -- staat in PORTED.md.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	IsActive bool   `json:"is_active"`
	// IsRestricted zegt dat dit apparaat geen opdrachten van buiten aanneemt.
	// Zo'n apparaat aanbieden is een knop die altijd faalt.
	IsRestricted  bool `json:"is_restricted"`
	VolumePercent *int `json:"volume_percent"`
}

// Devices levert de apparaten die dit account nu ziet.
//
// De lijst is vluchtig: een telefoon staat erin zolang de Spotify-app open is,
// een speaker zolang hij aan staat. Dat is geen tekortkoming van de API maar
// hoe Connect werkt, en het is de reden dat deze app een gekoppeld apparaat op
// zijn naam terugvindt en niet alleen op zijn id.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var answer struct {
		Devices []Device `json:"devices"`
	}
	if err := c.getJSON(ctx, "/me/player/devices", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Devices, nil
}

// Track is één nummer, zo klein als de kaart het nodig heeft.
type Track struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URI     string `json:"uri"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name   string `json:"name"`
		Images []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"images"`
	} `json:"album"`
	DurationMs int `json:"duration_ms"`
}

// By is de artiest, of de artiesten samen. Voor in een keuzelijst.
func (t Track) By() string {
	names := make([]string, 0, len(t.Artists))
	for _, artist := range t.Artists {
		names = append(names, artist.Name)
	}
	return strings.Join(names, ", ")
}

// Cover is het kleinste plaatje dat Spotify van dit album heeft, of leeg.
//
// Het kleinste, want dit gaat naar een keuzelijst in een Flow-kaart: daar past
// geen hoesje van 640 bij 640.
func (t Track) Cover() string {
	best := ""
	size := 0
	for _, image := range t.Album.Images {
		if best == "" || (image.Width > 0 && image.Width < size) {
			best, size = image.URL, image.Width
		}
	}
	return best
}

// maxSearchLimit is hoeveel treffers Spotify werkelijk teruggeeft.
//
// De documentatie noemt 0 tot 50. Gemeten tegen een echt account op 2026-08-10
// is elf al te veel: 1 tot 10 antwoordt met 200, alles daarboven met 400 en
// "Invalid limit". Zonder limiet komen er vijf, niet de gedocumenteerde twintig.
// Dat wijst op een grens die bij de registratie hoort en niet bij het eindpunt,
// dus hij staat hier als wat hij is: gemeten, met de datum erbij.
const maxSearchLimit = 10

// Search zoekt nummers. limit wordt binnen het bereik getrokken dat Spotify
// werkelijk aanneemt.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	// Binnen bereik trekken en niet vervangen door een standaard: die standaard
	// was 20, en dat is precies de waarde die geweigerd wordt.
	if limit <= 0 || limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	var answer struct {
		Tracks struct {
			Items []Track `json:"items"`
		} `json:"tracks"`
	}
	err := c.getJSON(ctx, "/search", url.Values{
		"q": {query}, "type": {"track"}, "limit": {strconv.Itoa(limit)},
	}, &answer)
	if err != nil {
		return nil, err
	}
	return answer.Tracks.Items, nil
}

// Playback is wat er nu speelt, op welk apparaat.
type Playback struct {
	Device     Device `json:"device"`
	IsPlaying  bool   `json:"is_playing"`
	ProgressMs int    `json:"progress_ms"`
	ShuffleOn  bool   `json:"shuffle_state"`
	RepeatMode string `json:"repeat_state"`
	Item       *Track `json:"item"`
}

// Playback levert wat er speelt, of ok=false als er niets speelt.
//
// Spotify antwoordt dan met 204 en een lege body, en dat is geen fout: het
// betekent gewoon dat er niets aan staat.
func (c *Client) Playback(ctx context.Context) (Playback, bool, error) {
	body, err := c.do(ctx, http.MethodGet, "/me/player", nil, nil)
	if err != nil {
		return Playback{}, false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return Playback{}, false, nil
	}
	var state Playback
	if err := json.Unmarshal(body, &state); err != nil {
		return Playback{}, false, fmt.Errorf("Spotify stuurde geen bruikbare JSON over wat er speelt: %w", err)
	}
	return state, true, nil
}

// Play zet een nummer op een apparaat.
//
// Dit is waar de app om draait. deviceID mag leeg zijn -- dan gaat het naar
// waar Spotify nu speelt -- maar de Flow-kaart vult hem altijd, want "speel dit
// af" zonder te zeggen wáár is precies de onduidelijkheid die je wilt vermijden.
func (c *Client) Play(ctx context.Context, deviceID string, uris ...string) error {
	body := map[string]any{}
	if len(uris) > 0 {
		body["uris"] = uris
	}
	_, err := c.do(ctx, http.MethodPut, "/me/player/play", deviceQuery(deviceID), body)
	return err
}

// Resume hervat wat er stond, zonder iets nieuws te kiezen.
func (c *Client) Resume(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPut, "/me/player/play", deviceQuery(deviceID), map[string]any{})
	return err
}

func (c *Client) Pause(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPut, "/me/player/pause", deviceQuery(deviceID), nil)
	return err
}

func (c *Client) Next(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/next", deviceQuery(deviceID), nil)
	return err
}

func (c *Client) Previous(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/previous", deviceQuery(deviceID), nil)
	return err
}

// SetVolume zet het volume in hele procenten, want dat is wat Spotify aanneemt.
func (c *Client) SetVolume(ctx context.Context, deviceID string, percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	query := deviceQuery(deviceID)
	if query == nil {
		query = url.Values{}
	}
	query.Set("volume_percent", strconv.Itoa(percent))
	_, err := c.do(ctx, http.MethodPut, "/me/player/volume", query, nil)
	return err
}

// Transfer verplaatst wat er speelt naar een ander apparaat.
func (c *Client) Transfer(ctx context.Context, deviceID string, play bool) error {
	if deviceID == "" {
		return fmt.Errorf("er is geen apparaat om naartoe te verplaatsen")
	}
	_, err := c.do(ctx, http.MethodPut, "/me/player", nil, map[string]any{
		"device_ids": []string{deviceID}, "play": play,
	})
	return err
}

func deviceQuery(deviceID string) url.Values {
	if deviceID == "" {
		return nil
	}
	return url.Values{"device_id": {deviceID}}
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	body, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("GET %s: Spotify stuurde geen bruikbare JSON: %w", path, err)
	}
	return nil
}

// do voert één aanroep uit en levert de rauwe body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if c.Token == nil {
		return nil, fmt.Errorf("deze client heeft geen tokenbron")
	}
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}

	address := c.base() + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	client := c.HTTP
	if client == nil {
		client = DefaultHTTP()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("%s %s: Spotify stuurde meer dan %d bytes", method, path, maxBody)
	}
	if response.StatusCode >= 400 {
		// Het verzoek erbij. Een API-fout die niet zegt wat er gevraagd is, is
		// niet na te lopen zonder de code ernaast te leggen -- en dat is precies
		// het moment waarop je hem niet bij de hand hebt.
		return nil, apiError(response.StatusCode, raw, method+" "+path+queryNote(query))
	}
	return raw, nil
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return apiBase
}

// queryNote maakt de vraag leesbaar voor in een foutmelding. Alleen de namen en
// waarden die we zelf meesturen -- het token zit in een header en komt hier dus
// nooit langs.
func queryNote(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}

// Error is wat de API zelf van een aanroep vond.
type Error struct {
	Status  int
	Message string
	Reason  string
	// Request is wat er gevraagd werd, bijvoorbeeld GET /search?limit=20&q=…
	Request string
}

func (e *Error) Error() string {
	switch {
	// De twee die een gebruiker werkelijk gaat tegenkomen, in woorden die
	// zeggen wat hij eraan kan doen.
	case e.Status == http.StatusForbidden && strings.Contains(strings.ToLower(e.Message), "premium"):
		return "Spotify staat dit alleen toe met Premium; bedienen op afstand zit niet in het gratis abonnement"
	case e.Reason == "NO_ACTIVE_DEVICE":
		return "er speelt niets en er is geen actief apparaat; kies een apparaat om op af te spelen"
	case e.Status == http.StatusNotFound:
		return "Spotify kent dit apparaat niet (meer); Connect-apparaten verdwijnen als ze uit staan"
	case e.Message != "":
		return fmt.Sprintf("Spotify: %s (http %d op %s)", e.Message, e.Status, e.Request)
	}
	return fmt.Sprintf("Spotify antwoordde met http %d op %s", e.Status, e.Request)
}

// Gone zegt of dit apparaat er niet meer is. Dan hoort een tegel op onbereikbaar
// en niet op fout: er is niets stuk, de speaker staat uit.
func (e *Error) Gone() bool {
	return e.Status == http.StatusNotFound || e.Reason == "NO_ACTIVE_DEVICE"
}

func apiError(status int, body []byte, request string) error {
	var answer struct {
		Error struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &answer)
	return &Error{Status: status, Message: answer.Error.Message, Reason: answer.Error.Reason, Request: request}
}
