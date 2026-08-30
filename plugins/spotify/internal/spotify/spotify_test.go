package spotify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// De koppeling en de API worden getoetst tegen een nagebouwde Spotify. Wat een
// echt account anders doet hoort in PORTED.md zodra het gevonden is.

func TestAuthorizeAsksForNothingMoreThanItNeeds(t *testing.T) {
	config := Config{ClientID: "abc", RedirectURI: "http://127.0.0.1:8081/spotify"}
	authorization, err := config.Authorize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()

	// PKCE en geen clientgeheim: dat is de hele reden dat deze app niets
	// bewaart wat een account opent.
	if query.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method is %q", query.Get("code_challenge_method"))
	}
	if len(query.Get("code_challenge")) != 43 {
		t.Errorf("de uitdaging is %d tekens, wil 43 (base64url van een sha256)", len(query.Get("code_challenge")))
	}
	if query.Get("client_secret") != "" {
		t.Error("er gaat een clientgeheim mee, en dat hoort deze app niet te hebben")
	}

	// Alleen wat het doel vraagt: kijken welke apparaten er zijn, en zeggen wat
	// er moet spelen. Elke scope erbij is toegang die niemand nodig heeft.
	scopes := strings.Fields(query.Get("scope"))
	want := map[string]bool{"user-read-playback-state": false, "user-modify-playback-state": false}
	for _, scope := range scopes {
		if _, ok := want[scope]; !ok {
			t.Errorf("de app vraagt %q, en dat heeft hij niet nodig", scope)
			continue
		}
		want[scope] = true
	}
	for scope, found := range want {
		if !found {
			t.Errorf("de scope %q ontbreekt", scope)
		}
	}
	if authorization.State == "" || authorization.Verifier == "" {
		t.Error("state of verifier ontbreekt")
	}
}

func TestAuthorizeRefusesAnIncompleteRegistration(t *testing.T) {
	if _, err := (Config{RedirectURI: "http://127.0.0.1/x"}).Authorize(); err == nil {
		t.Error("een registratie zonder client-id werd geaccepteerd")
	}
	if _, err := (Config{ClientID: "abc"}).Authorize(); err == nil {
		t.Error("een registratie zonder redirect werd geaccepteerd")
	}
}

// De state is geen formaliteit: hij bewijst dat dit antwoord bij de autorisatie
// hoort die hier begon.
func TestCodeFromRedirect(t *testing.T) {
	for _, test := range []struct {
		name, pasted, state, want string
		fails                     bool
	}{
		{name: "heel adres", pasted: "http://127.0.0.1:8081/spotify?code=xyz&state=s1", state: "s1", want: "xyz"},
		{name: "alleen de query", pasted: "?code=xyz&state=s1", state: "s1", want: "xyz"},
		{name: "andere state", pasted: "http://x/?code=xyz&state=anders", state: "s1", fails: true},
		{name: "spotify weigerde", pasted: "http://x/?error=access_denied&state=s1", state: "s1", fails: true},
		{name: "niets geplakt", pasted: "   ", state: "s1", fails: true},
		{name: "geen code", pasted: "http://x/?state=s1", state: "s1", fails: true},
	} {
		got, err := CodeFromRedirect(test.pasted, test.state)
		if test.fails {
			if err == nil {
				t.Errorf("%s: werd geaccepteerd", test.name)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("%s: kreeg %q %v, wil %q", test.name, got, err, test.want)
		}
	}
}

func TestExchangeAndRefresh(t *testing.T) {
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		seen = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "rt", "expires_in": 3600,
			"scope": scopes,
		})
	}))
	defer server.Close()
	config := Config{ClientID: "abc", RedirectURI: "http://127.0.0.1/x", BaseURL: server.URL}

	tokens, err := config.Exchange(context.Background(), server.Client(), "code-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if seen.Get("code_verifier") != "verifier-1" {
		t.Errorf("de verifier ging niet mee: %v", seen)
	}
	if seen.Get("client_secret") != "" {
		t.Error("er ging een clientgeheim mee")
	}
	if tokens.Stale(0) {
		t.Error("een vers token geldt al niet meer")
	}

	// Spotify wisselt het refresh-token niet altijd om. Komt er geen mee, dan
	// blijft het oude gelden -- weggooien zou de koppeling verbreken.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "expires_in": 3600})
	})
	again, err := config.Refresh(context.Background(), server.Client(), "rt")
	if err != nil {
		t.Fatal(err)
	}
	if again.RefreshToken != "rt" {
		t.Errorf("het refresh-token werd %q, wil rt", again.RefreshToken)
	}
}

// Een definitieve weigering hoort de koppeling te beëindigen; een storing niet.
func TestOnlyAFinalRefusalEndsTheLink(t *testing.T) {
	for code, final := range map[string]bool{
		"invalid_grant": true, "invalid_client": true,
		"server_error": false, "temporarily_unavailable": false,
	} {
		err := &AuthError{Code: code}
		if err.Final() != final {
			t.Errorf("%s: Final() is %v, wil %v", code, err.Final(), final)
		}
	}
}

func fakeAPI(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	return &Client{
		Token:   func(context.Context) (string, error) { return "at", nil },
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}, server
}

func TestDevicesAndPlay(t *testing.T) {
	var played struct {
		path, device string
		body         map[string]any
	}
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("geen bearer-token: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/me/player/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"id": "d1", "name": "Woonkamer", "type": "Speaker", "is_active": true, "volume_percent": 40},
			}})
		case "/me/player/play":
			played.path = r.URL.Path
			played.device = r.URL.Query().Get("device_id")
			played.body = nil
			json.NewDecoder(r.Body).Decode(&played.body)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("onverwachte aanroep %s", r.URL.Path)
		}
	})
	defer server.Close()

	devices, err := client.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Name != "Woonkamer" || *devices[0].VolumePercent != 40 {
		t.Fatalf("apparaten: %+v", devices)
	}

	if err := client.Play(context.Background(), "d1", "spotify:track:xyz"); err != nil {
		t.Fatal(err)
	}
	if played.device != "d1" {
		t.Errorf("het apparaat ging niet mee: %q", played.device)
	}
	uris, _ := played.body["uris"].([]any)
	if len(uris) != 1 || uris[0] != "spotify:track:xyz" {
		t.Errorf("het nummer ging niet mee: %v", played.body)
	}

	if err := client.PlayContext(context.Background(), "d1", "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"); err != nil {
		t.Fatal(err)
	}
	if played.device != "d1" {
		t.Errorf("de playlist ging niet naar het apparaat: %q", played.device)
	}
	if played.body["context_uri"] != "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M" {
		t.Errorf("de playlist ging niet mee: %v", played.body)
	}
	if _, ok := played.body["uris"]; ok {
		t.Errorf("de playlist werd ook als nummer verstuurd: %v", played.body)
	}
}

// Niets aan het spelen is geen fout. Spotify antwoordt dan met 204 en een lege
// body; dat als fout behandelen zou elke tegel op onbereikbaar zetten terwijl
// er niets aan de hand is.
func TestNothingPlayingIsNotAnError(t *testing.T) {
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()

	_, playing, err := client.Playback(context.Background())
	if err != nil {
		t.Fatalf("204 gaf een fout: %v", err)
	}
	if playing {
		t.Error("er speelde iets, terwijl Spotify 204 gaf")
	}
}

// De twee fouten die een gebruiker werkelijk tegenkomt, in woorden die zeggen
// wat hij eraan kan doen.
func TestTheErrorsAUserWillActuallyMeet(t *testing.T) {
	premium := &Error{Status: http.StatusForbidden, Message: "Player command failed: Premium required"}
	if !strings.Contains(premium.Error(), "Premium") {
		t.Errorf("de premium-fout zegt: %q", premium.Error())
	}
	idle := &Error{Status: http.StatusNotFound, Reason: "NO_ACTIVE_DEVICE"}
	if !strings.Contains(idle.Error(), "apparaat") || !idle.Gone() {
		t.Errorf("de geen-apparaat-fout zegt: %q (Gone=%v)", idle.Error(), idle.Gone())
	}
}

func TestSearchBuildsTheQueryAndReadsTheTracks(t *testing.T) {
	var query url.Values
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"tracks": map[string]any{"items": []map[string]any{{
			"id": "t1", "name": "Blue Monday", "uri": "spotify:track:t1",
			"artists": []map[string]any{{"name": "New Order"}},
			"album": map[string]any{"name": "Substance", "images": []map[string]any{
				{"url": "groot", "width": 640}, {"url": "klein", "width": 64},
			}},
		}}}})
	})
	defer server.Close()

	tracks, err := client.Search(context.Background(), "blue monday", 0)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("type") != "track" || query.Get("q") != "blue monday" {
		t.Errorf("de zoekvraag was %v", query)
	}
	if len(tracks) != 1 {
		t.Fatalf("treffers: %+v", tracks)
	}
	if tracks[0].By() != "New Order" {
		t.Errorf("artiest is %q", tracks[0].By())
	}
	// Het kleinste plaatje, want dit gaat naar een keuzelijst.
	if tracks[0].Cover() != "klein" {
		t.Errorf("hoesje is %q, wil het kleinste", tracks[0].Cover())
	}
}

func TestSearchBuildsTheQueryAndReadsPlaylists(t *testing.T) {
	var query url.Values
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"playlists": map[string]any{"items": []any{
			nil,
			map[string]any{
				"id":          "37i9dQZF1DXcBWIGoYBM5M",
				"name":        "Today's Top Hits",
				"uri":         "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M",
				"description": "The biggest songs",
				"owner":       map[string]any{"display_name": "Spotify"},
				"images": []map[string]any{
					{"url": "groot", "width": 640},
					{"url": "klein", "width": 64},
				},
			},
		}}})
	})
	defer server.Close()

	playlists, err := client.SearchPlaylists(context.Background(), "top hits", 0)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("type") != "playlist" || query.Get("q") != "top hits" || query.Get("limit") != "10" {
		t.Errorf("de zoekvraag was %v", query)
	}
	if len(playlists) != 1 {
		t.Fatalf("treffers: %+v", playlists)
	}
	if playlists[0].URI != "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M" {
		t.Errorf("playlist-uri is %q", playlists[0].URI)
	}
	if playlists[0].By() != "Spotify" {
		t.Errorf("eigenaar is %q", playlists[0].By())
	}
	if playlists[0].Cover() != "klein" {
		t.Errorf("hoes is %q, wil het kleinste plaatje", playlists[0].Cover())
	}
}

// Zoeken zonder tekst hoort geen aanroep te doen: een leeg zoekveld is geen
// vraag, en Spotify zou er een fout op geven.
func TestAnEmptySearchAsksNothing(t *testing.T) {
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("er ging een aanroep uit voor een lege zoekopdracht")
	})
	defer server.Close()

	tracks, err := client.Search(context.Background(), "   ", 20)
	if err != nil || len(tracks) != 0 {
		t.Errorf("kreeg %v %v", tracks, err)
	}

	playlists, err := client.SearchPlaylists(context.Background(), "\t", 20)
	if err != nil || len(playlists) != 0 {
		t.Errorf("kreeg %v %v", playlists, err)
	}
}

func TestVolumeStaysWithinWhatSpotifyAccepts(t *testing.T) {
	var seen []string
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("volume_percent"))
		w.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()

	for _, percent := range []int{-10, 0, 55, 100, 140} {
		if err := client.SetVolume(context.Background(), "d1", percent); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"0", "0", "55", "100", "100"}
	for i, value := range want {
		if seen[i] != value {
			t.Errorf("volume %d werd %q, wil %q", i, seen[i], value)
		}
	}
}

func TestStaleUsesTheMargin(t *testing.T) {
	fresh := &Tokens{Expiry: time.Now().Add(30 * time.Minute)}
	if fresh.Stale(5 * time.Minute) {
		t.Error("een token van een half uur geldt al niet meer")
	}
	if !fresh.Stale(45 * time.Minute) {
		t.Error("de marge wordt niet meegerekend")
	}
	var missing *Tokens
	if !missing.Stale(0) {
		t.Error("geen token geldt als geldig")
	}
}

// Een API-fout hoort te zeggen wát er gevraagd is.
//
// "invalid limit (400)" is niet na te lopen zonder de code ernaast te leggen --
// en dat is precies het moment waarop je hem niet bij de hand hebt. Met het
// verzoek erbij zie je meteen of het lag aan wat wij stuurden.
func TestAnAPIErrorSaysWhatWasAsked(t *testing.T) {
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"status":400,"message":"invalid limit"}}`))
	})
	defer server.Close()

	_, err := client.Search(context.Background(), "blue monday", 20)
	if err == nil {
		t.Fatal("een 400 gaf geen fout")
	}
	for _, want := range []string{"invalid limit", "GET /search", "limit=10", "q=blue+monday", "type=track"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("de melding %q mist %q", err.Error(), want)
		}
	}

	// Het token zit in een header en hoort nooit in een melding te belanden.
	if strings.Contains(err.Error(), "at") && strings.Contains(err.Error(), "Bearer") {
		t.Errorf("er lekt iets van het token in de melding: %q", err.Error())
	}
}

// Spotify's documentatie noemt 0 tot 50 voor het zoeklimiet. Gemeten tegen een
// echt account is elf al te veel -- alles boven tien antwoordt met 400 en
// "Invalid limit". Vandaar dat een te hoge waarde naar beneden getrokken wordt
// en niet vervangen door een standaard: die standaard was 20 en werd geweigerd.
func TestTheSearchLimitStaysWithinWhatSpotifyReallyTakes(t *testing.T) {
	var seen []string
	client, server := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("limit"))
		json.NewEncoder(w).Encode(map[string]any{"tracks": map[string]any{"items": []map[string]any{}}})
	})
	defer server.Close()

	for _, ask := range []int{0, -5, 3, 10, 20, 50} {
		if _, err := client.Search(context.Background(), "new order", ask); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"10", "10", "3", "10", "10", "10"}
	for i, value := range want {
		if seen[i] != value {
			t.Errorf("limit %d werd %q, wil %q", i, seen[i], value)
		}
	}
}
