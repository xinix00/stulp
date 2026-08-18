package tahoma

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Een opdracht aan TaHoma is asynchroon, en dat is geen detail.
//
// POST /exec/apply antwoordt met een execId zodra de doos de opdracht heeft
// aangenomen. Op dat moment is er nog niets bewogen: een rolluik doet er daarna
// twintig seconden over. De echte stand komt pas terug via de standen in
// /setup, en dus via de poll.
//
// Wie de nieuwe stand meteen als feit meldt, meldt iets wat niet waar is -- en
// een half uur later staat de tegel op 50% terwijl het rolluik ergens anders
// hangt omdat de opdracht onderweg is misgegaan. Zie covering.go voor wat er
// hier daarom wél en niet bevestigd wordt.

// Command is één opdracht aan één apparaat, bijvoorbeeld setClosure met 40.
type Command struct {
	Name       string `json:"name"`
	Parameters []any  `json:"parameters"`
}

// Execution is wat een opdracht oplevert: een handvat om hem te stoppen.
type Execution struct {
	ID string `json:"execId"`
}

// Execute stuurt één opdracht naar één apparaat.
//
// label is de naam van het apparaat en komt terecht in de geschiedenis van de
// TaHoma-app zelf, zodat daar te zien is waar een beweging vandaan kwam. De
// bron zet er "Homey" achter; hier staat "Stulp", want dat is wie het doet.
func (c *Client) Execute(ctx context.Context, label, deviceURL string, command Command) (string, error) {
	if deviceURL == "" {
		return "", fmt.Errorf("een opdracht zonder deviceURL kan nergens heen")
	}
	if command.Name == "" {
		return "", fmt.Errorf("een opdracht zonder naam heeft TaHoma niets te zeggen")
	}
	if command.Parameters == nil {
		// TaHoma wil het veld zien; nil zou als null verstuurd worden.
		command.Parameters = []any{}
	}
	body := map[string]any{
		"label": strings.TrimSpace(label) + " - " + command.Name + " - Stulp",
		"actions": []any{map[string]any{
			"deviceURL": deviceURL,
			"commands":  []Command{command},
		}},
	}
	answer, err := c.do(ctx, http.MethodPost, "/exec/apply", nil, body)
	if err != nil {
		return "", err
	}
	var execution Execution
	if err := json.Unmarshal(answer, &execution); err != nil {
		return "", fmt.Errorf("exec/apply: TaHoma stuurde iets dat geen JSON is: %w", err)
	}
	if execution.ID == "" {
		// Zonder execId is er niets om later mee te stoppen. Dat stil laten
		// passeren betekent dat de stopknop het later zonder uitleg niet doet.
		return "", fmt.Errorf("TaHoma nam %s aan zonder uitvoer-id terug te geven", command.Name)
	}
	return execution.ID, nil
}

// Cancel stopt een lopende uitvoering.
//
// Dit is het enige wat de bron als "stop" kent: er is geen los stopcommando,
// alleen het intrekken van een opdracht die je zelf gegeven hebt
// (lib/Tahoma.js, cancelExecution).
func (c *Client) Cancel(ctx context.Context, executionID string) error {
	if executionID == "" {
		return fmt.Errorf("er is geen uitvoering om te stoppen")
	}
	_, err := c.do(ctx, http.MethodDelete, "/exec/current/setup/"+escapePath(executionID), nil, nil)
	return err
}

// Scenario is een scenario zoals het in de TaHoma-app heet.
type Scenario struct {
	OID   string `json:"oid"`
	Label string `json:"label"`
}

// Scenarios levert wat er te starten valt.
func (c *Client) Scenarios(ctx context.Context) ([]Scenario, error) {
	return getJSON[[]Scenario](ctx, c, "/actionGroups")
}

// RunScenario start een scenario.
func (c *Client) RunScenario(ctx context.Context, oid string) error {
	if oid == "" {
		return fmt.Errorf("er is geen scenario gekozen")
	}
	_, err := c.do(ctx, http.MethodPost, "/exec/"+escapePath(oid), nil, nil)
	return err
}

// escapePath maakt van een id iets dat veilig in een pad past.
//
// url.PathEscape laat de tekens door die in een padsegment mogen staan, en dat
// is precies wat hier nodig is: een oid of een execId is een uuid, maar wat
// TaHoma morgen als id verzint hoeft dat niet te zijn.
func escapePath(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		char := value[i]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_', char == '.', char == '~':
			out.WriteByte(char)
		default:
			fmt.Fprintf(&out, "%%%02X", char)
		}
	}
	return out.String()
}
