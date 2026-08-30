package main

import "testing"

func TestSpotifyPlayerComesFromTheFlowDeviceProperty(t *testing.T) {
	want := &player{}
	a := &app{devices: map[string]*player{"woonkamer": want}}

	got, err := a.playerFor(map[string]any{
		"device": map[string]any{"$device": "woonkamer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("de Flow-deviceverwijzing wees niet naar de Spotify-speler")
	}
}

// De kaart bewaart wat er gekozen is, en dat is het hele item -- niet de tekst
// die iemand typte. Wie wel typt maar niet kiest hoort een zin te krijgen die
// zegt wat er moet gebeuren, in plaats van een fout van Spotify over een uri die
// geen uri is.
func TestOnlyARealTrackGetsThrough(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		wantURI string
	}{
		{"gekozen uit de lijst", map[string]any{"id": "spotify:track:11dFghVXANMlKmJXsNCbNl", "name": "Blue Monday"},
			"spotify:track:11dFghVXANMlKmJXsNCbNl"},
		{"kaal id", "11dFghVXANMlKmJXsNCbNl", "spotify:track:11dFghVXANMlKmJXsNCbNl"},
		{"volledige uri als tekst", "spotify:track:11dFghVXANMlKmJXsNCbNl", "spotify:track:11dFghVXANMlKmJXsNCbNl"},
		{"alleen getypt, niet gekozen", "blue monday", ""},
		{"leeg", "", ""},
		{"item zonder id", map[string]any{"name": "Blue Monday"}, ""},
	} {
		uri, _ := trackArgument(test.value)
		if uri != test.wantURI {
			t.Errorf("%s: uri is %q, wil %q", test.name, uri, test.wantURI)
		}
	}
}

// De naam blijft bewaard ook als de uri geweigerd wordt: die staat in de
// melding, zodat de gebruiker ziet wát er dan wel in het veld stond.
func TestTheNameSurvivesARefusal(t *testing.T) {
	if _, name := trackArgument("blue monday"); name != "blue monday" {
		t.Errorf("de naam is %q", name)
	}
}

func TestOnlyARealPlaylistGetsThrough(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		wantURI string
	}{
		{"gekozen uit de lijst", map[string]any{"id": "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M", "name": "Today's Top Hits"},
			"spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"},
		{"kaal id", "37i9dQZF1DXcBWIGoYBM5M", "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"},
		{"volledige uri", "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M", "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"},
		{"gedeelde link", "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M?si=abc123", "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"},
		{"alleen zoektekst", "today's top hits", ""},
		{"track is geen playlist", "spotify:track:11dFghVXANMlKmJXsNCbNl", ""},
	} {
		uri, _ := playlistArgument(test.value)
		if uri != test.wantURI {
			t.Errorf("%s: uri is %q, wil %q", test.name, uri, test.wantURI)
		}
	}
}
