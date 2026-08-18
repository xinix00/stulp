package main

import "testing"

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
