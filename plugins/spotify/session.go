package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xinix00/stulp/plugins/spotify/internal/spotify"
)

// De koppeling met Spotify: één token en wat er nodig is om het geldig te
// houden.
//
// Het token staat in de eigen state van de plugin en niet in de
// app-instellingen. Instellingen zijn leesbaar via de web-API; een access- en
// refresh-token horen daar niet. De registratie zelf -- client-id en redirect --
// staat wel in de instellingen: dat is wat de gebruiker invult, en geen van
// beide opent zijn account.

// refreshMargin is hoe lang vóór het verlopen er ververst wordt. Een
// Spotify-token leeft een uur; vijf minuten is ruim voor een trage verbinding.
const refreshMargin = 5 * time.Minute

// maintainInterval is hoe vaak er gekeken wordt of het token nog meegaat.
const maintainInterval = time.Minute

type session struct {
	mu     sync.Mutex
	config spotify.Config
	tokens *spotify.Tokens
	http   *http.Client

	// save legt het token vast buiten dit proces, notify meldt aan de gebruiker
	// wat hij zelf moet oplossen. Allebei als functie, zodat een test de sessie
	// kan aandrijven zonder een draaiende Stulp.
	save   func(*spotify.Tokens) error
	notify func(string)

	lastErr string
}

// errNotLinked is wat elke aanroep krijgt zolang er geen token is. De tekst is
// wat de gebruiker op de tegel te zien krijgt, dus hij zegt wat hij moet doen.
var errNotLinked = errors.New("deze app is nog niet gekoppeld met Spotify; open de instellingen van de app")

func newSession(save func(*spotify.Tokens) error, notify func(string)) *session {
	return &session{http: spotify.DefaultHTTP(), save: save, notify: notify}
}

// setConfig legt de registratie vast. Het token blijft staan: hoort het bij een
// andere registratie, dan zegt de tokenserver dat bij de eerste verversing en
// dat is duidelijker dan het hier stilletjes weggooien.
func (s *session) setConfig(config spotify.Config) {
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
}

func (s *session) setTokens(tokens *spotify.Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.lastErr = tokens, ""
	return s.save(tokens)
}

// token levert een geldig access-token aan de API-client.
//
// De lock blijft tijdens het verversen liggen. Dat houdt een tweede aanroep even
// op, en dat is de bedoeling: twee verversingen tegelijk kunnen elkaars
// refresh-token ongeldig maken.
func (s *session) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return "", errNotLinked
	}
	if s.tokens.Stale(refreshMargin) {
		if err := s.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return s.tokens.AccessToken, nil
}

// maintain ververst het token voordat het verloopt, ook als er niemand kijkt.
func (s *session) maintain(ctx context.Context) {
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.tokens != nil && s.tokens.Stale(refreshMargin) {
				s.refreshLocked(ctx)
			}
			s.mu.Unlock()
		}
	}
}

// refreshLocked haalt een nieuw token op. De aanroeper houdt de lock vast.
func (s *session) refreshLocked(ctx context.Context) error {
	tokens, err := s.config.Refresh(ctx, s.http, s.tokens.RefreshToken)
	if err != nil {
		s.lastErr = err.Error()

		// Een definitieve weigering is het einde van deze koppeling. Het token
		// bewaren zou de app elke minuut hetzelfde dode papiertje laten
		// aanbieden; weggooien maakt duidelijk dat er iets van de gebruiker
		// nodig is, en de melding zegt wat.
		var refused *spotify.AuthError
		if errors.As(err, &refused) && refused.Final() {
			s.tokens = nil
			if saveErr := s.save(nil); saveErr != nil {
				return fmt.Errorf("%w (en het opruimen mislukte: %v)", err, saveErr)
			}
			s.notify("Spotify: de koppeling wordt niet meer geaccepteerd. Koppel de app opnieuw in de instellingen.")
		}
		return err
	}
	s.tokens, s.lastErr = tokens, ""
	return s.save(tokens)
}

// linked zegt of er een token is, wanneer het verloopt en wat er het laatst
// misging. Voor de configuratiepagina.
func (s *session) linked() (bool, time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return false, time.Time{}, s.lastErr
	}
	return true, s.tokens.Expiry, s.lastErr
}

func (s *session) registration() spotify.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// stored is de vorm waarin het token in de state van de app staat. Een omhulsel
// in plaats van het token zelf, zodat er later iets bij kan zonder dat een
// bestaande installatie zijn koppeling kwijtraakt.
type stored struct {
	Tokens *spotify.Tokens `json:"tokens,omitempty"`
}

func readStored(raw json.RawMessage) (*spotify.Tokens, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var state stored
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("de bewaarde Spotify-koppeling is onleesbaar: %w", err)
	}
	return state.Tokens, nil
}

func writeStored(tokens *spotify.Tokens) (json.RawMessage, error) {
	if tokens == nil {
		return nil, nil
	}
	return json.Marshal(stored{Tokens: tokens})
}
