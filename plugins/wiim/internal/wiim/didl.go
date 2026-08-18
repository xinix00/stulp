package wiim

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// De metadata van het nummer komt als DIDL-Lite mee in het antwoord van
// GetInfoEx: een stukje XML in een XML-veld. Zie
// `_docs/upnpSpecs/AVTransport/GetMediaInfo.json` in de bron voor een echt
// voorbeeld — dat is een radiozender, en dat geval bepaalt de vorm hieronder.
//
// Hier wordt niets ontsleuteld. Dat staat er omdat de vraag terugkomt: de
// HTTP-API van LinkPlay staat erom bekend dat `getPlayerStatus` titels
// hex-gecodeerd meestuurt, maar deze bron vraagt die stand niet op en decodeert
// dus ook nergens hex — niet in `device.js`, niet in `driver.js`, nergens in de
// geschiedenis van de bron. Wat hier binnenkomt is gewone XML met gewone
// entiteiten, en encoding/xml doet de rest. Zie PORTED.md.

// ParseTrackMetadata leest wat er speelt uit het DIDL-Lite-veld.
func ParseTrackMetadata(raw string) (Track, error) {
	var document struct {
		Items []struct {
			Title    string `xml:"http://purl.org/dc/elements/1.1/ title"`
			Subtitle string `xml:"http://purl.org/dc/elements/1.1/ subtitle"`
			Artist   string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ artist"`
			Album    string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ album"`
			ArtURI   string `xml:"urn:schemas-upnp-org:metadata-1-0/upnp/ albumArtURI"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(raw), &document); err != nil {
		return Track{}, fmt.Errorf("de metadata van het nummer is geen DIDL-Lite: %w", err)
	}
	if len(document.Items) == 0 {
		// Een wachtrij zonder item is geen fout: de speler zegt daarmee dat er
		// niets is om te tonen.
		return Track{}, nil
	}
	// De bron neemt `['DIDL-Lite'].item` en dus het eerste item. Wat er nu
	// speelt staat vooraan; de rest is de wachtrij.
	item := document.Items[0]

	title := strings.TrimSpace(item.Title)
	subtitle := strings.TrimSpace(item.Subtitle)
	artist := strings.TrimSpace(item.Artist)
	album := strings.TrimSpace(item.Album)

	track := Track{ArtURI: strings.TrimSpace(item.ArtURI), Present: true}
	if subtitle != "" {
		// Dit is de radio-vorm, en hij verklaart waarom een zender als artiest
		// in beeld staat: bij een stream is `dc:title` de zender en draagt
		// `dc:subtitle` wat er op dit moment klinkt ("David Guetta & Sia -
		// Floating through space"). `upnp:artist` en `upnp:album` zijn dan leeg.
		// De bron kiest daarom de zender voor artiest én album, en de ondertitel
		// als nummer. Overgenomen: het is de enige indeling waarin een
		// radiotegel iets zegt.
		track.Artist = title
		track.Album = title
		track.Title = subtitle
		return track, nil
	}
	// En dit is een gewoon nummer uit een bibliotheek.
	track.Artist = join(artist, album)
	track.Album = album
	track.Title = title
	return track, nil
}

// join plakt artiest en album aan elkaar zoals de bron dat doet
// (`${artist}, ${album}`), maar zonder de komma als er aan één kant niets
// staat. De bron levert in dat geval "Artiest, " of ", Album" op de tegel af.
func join(left, right string) string {
	switch {
	case left == "":
		return right
	case right == "":
		return left
	}
	return left + ", " + right
}
