package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// De eigen state van een app.
//
// Settings zijn van de gebruiker: die staan in Manage, zijn te wijzigen en
// horen in een backup thuis. State is van de app zelf -- een token, een sessie,
// een fabric met sleutelmateriaal. Stulp bewaart hem en begrijpt hem niet.
//
// Waarom in hetzelfde document en niet in een bestand naast de app: backup en
// restore werken op dit document. Een app die zijn state ernaast zet raakt hem
// stilletjes kwijt bij een restore, en dat is precies het soort verlies waar je
// pas achter komt als je het nodig hebt.
//
// Er is met opzet geen API-route die dit uitleest. Settings gaan naar de
// browser, state niet: hier ligt het materiaal waarmee een app zich elders
// legitimeert.

// AppState levert de opgeslagen state van een app, of nil als die er niet is.
func (s *Store) AppState(_ context.Context, appID string) (json.RawMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.doc.AppState[appID]
	if !ok {
		return nil, nil
	}
	// Kopiëren: de aanroeper krijgt bytes die niemand anders onder hem
	// verandert.
	out := make(json.RawMessage, len(state))
	copy(out, state)
	return out, nil
}

// SetAppState vervangt de state van een app. Lege state wist hem.
func (s *Store) SetAppState(ctx context.Context, appID string, state json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if appID == "" {
		return fmt.Errorf("app state needs an app id")
	}
	if len(state) > 0 && !json.Valid(state) {
		// Ongeldige JSON zou het hele document onleesbaar maken bij de volgende
		// start. Hier weigeren is het enige moment waarop dat nog kan.
		return fmt.Errorf("app state for %s is not valid JSON", appID)
	}

	s.mu.Lock()
	if len(state) == 0 {
		delete(s.doc.AppState, appID)
	} else {
		if s.doc.AppState == nil {
			s.doc.AppState = make(map[string]json.RawMessage)
		}
		stored := make(json.RawMessage, len(state))
		copy(stored, state)
		s.doc.AppState[appID] = stored
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("set app state: %w", err)
	}
	// Geen event met de inhoud erin: dit gaat langs abonnees die het niet hoeven
	// te zien. Wie hem schreef weet al wat erin staat.
	s.publish(Event{Manager: "apps", Type: "app.state", ID: appID})
	return nil
}
