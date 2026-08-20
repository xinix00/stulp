package appsdk

import "encoding/json"

// BuildVersion is de versie van het BINARY, door de release erin gezet
// (-ldflags -X .../appsdk.BuildVersion=vX.Y.Z). Leeg bij een kale go build:
// dan geldt wat de app.json zegt en verzint niemand iets.
var BuildVersion string

// announceManifest is wat een plugin bij zijn aanmelding over zichzelf zegt.
// Draagt het binary een buildversie, dan vervangt die de versie uit de
// ingebakken app.json: het bínary is wat er draait, en de release stempelt hem
// — een app.json die eeuwig "1.0.0" zegt is geen antwoord op "welke versie
// draait er?". Alleen de announce-kopie wordt geraakt; het manifest zelf
// blijft byte-voor-byte wat de bundel droeg.
func announceManifest(raw []byte) []byte {
	if BuildVersion == "" || len(raw) == 0 {
		return raw
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw // onleesbaar hier = onleesbaar bij stulp; die weigert luid
	}
	doc["version"] = BuildVersion
	patched, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return patched
}
