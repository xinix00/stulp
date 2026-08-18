package tahoma

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// De velden die deze plugin gebruikt, en niet meer.
//
// Een setup-antwoord draagt per apparaat een hele boom: welke doos hem
// aanstuurt, welke commando's hij kent, welke attributen erbij horen. Ze
// allemaal overnemen levert een tweede model op dat bijgehouden moet worden
// zonder dat iemand er iets aan heeft. Wat hier staat is wat er ergens in een
// capability of in het koppelscherm terechtkomt; PORTED.md zegt wat er verder in
// het antwoord zit.

// Setup is het antwoord van GET /setup.
type Setup struct {
	Devices []Device `json:"devices"`
}

// Device is één apparaat in de TaHoma-doos.
type Device struct {
	// DeviceURL is het adres waarmee je hem aanstuurt, bijvoorbeeld
	// io://1234-5678-9012/16744372. Dit is de sleutel die deze plugin gebruikt:
	// een commando gaat ernaartoe en een stand komt eruit.
	DeviceURL string `json:"deviceURL"`
	// OID is het object-id. De bron zoekt apparaten hierop terug
	// (lib/helper/device.js, isSameDevice); hier staat hij alleen in de
	// koppelgegevens, zodat een melding tegen de oorspronkelijke app hier terug
	// te vinden is.
	OID string `json:"oid"`
	// Label is de naam die de gebruiker in de TaHoma-app gaf.
	Label string `json:"label"`
	// ControllableName zegt wat voor apparaat het is, bijvoorbeeld
	// io:RollerShutterGenericIOComponent. Hierop kiest een driver zijn apparaten.
	ControllableName string `json:"controllableName"`

	States []State `json:"states"`
}

// State is één gemelde waarde, bijvoorbeeld core:ClosureState = 40.
//
// Value is any omdat het per stand van vorm verschilt: een sluiting is een
// getal, een open/dicht-stand is tekst. Er is geen reden om dat plat te slaan
// naar één type -- wie een stand leest weet welke hij bedoelt.
type State struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// Number leest een stand als getal. Ontbreekt hij, of is hij geen getal, dan is
// ok onwaar -- en dat is iets anders dan de waarde nul.
func (d Device) Number(name string) (float64, bool) {
	for _, state := range d.States {
		if state.Name != name {
			continue
		}
		switch value := state.Value.(type) {
		case float64:
			return value, true
		case json.Number:
			number, err := value.Float64()
			return number, err == nil
		}
		return 0, false
	}
	return 0, false
}

// Text leest een stand als tekst.
func (d Device) Text(name string) (string, bool) {
	for _, state := range d.States {
		if state.Name != name {
			continue
		}
		text, ok := state.Value.(string)
		return text, ok
	}
	return "", false
}

// Signature is een korte samenvatting van alle standen van dit apparaat.
//
// Dit is wat de poll vergelijkt om te weten of er iets veranderd is. De namen
// gaan op volgorde: TaHoma belooft geen vaste volgorde, en anders zou een
// omgewisselde lijst als wijziging tellen en elke ronde ruis opleveren.
func (d Device) Signature() string {
	parts := make([]string, 0, len(d.States))
	for _, state := range d.States {
		value, err := json.Marshal(state.Value)
		if err != nil {
			value = []byte(fmt.Sprint(state.Value))
		}
		parts = append(parts, state.Name+"="+string(value))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// Setup haalt de hele doos op: alle apparaten met al hun standen.
//
// Dit is het enige eindpunt waar standen vandaan komen. De bron kent geen
// gebeurtenisstroom en geen eindpunt per apparaat -- zie PORTED.md, dat is de
// belangrijkste vondst van deze port.
func (c *Client) Setup(ctx context.Context) (Setup, error) {
	return getJSON[Setup](ctx, c, "/setup")
}
