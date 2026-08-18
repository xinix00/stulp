package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// De koppeling met myUplink: één token en wat er nodig is om het geldig te
// houden.
//
// Het token staat in de eigen state van de plugin en niet in de app-instellingen.
// Dat is geen smaakkwestie: instellingen zijn van de gebruiker en komen langs
// Manage en de web-API, en een access- en refresh-token horen daar geen van
// beide te zijn. De registratie zelf -- client-id, geheim, redirect -- staat wel
// in de instellingen, want dat is wat de gebruiker invult.

// refreshMargin is hoe lang vóór het verlopen er ververst wordt.
//
// Een token van myUplink leeft een uur. Vijf minuten is ruim genoeg voor een
// trage verbinding en kort genoeg om niet elk uur twee keer te verversen. Het
// gaat er vooral om dat het gebeurt vóór een aanroep en niet nádat er een
// mislukt is: die mislukking valt midden in een poll en laat een gat achter.
const refreshMargin = 5 * time.Minute

// maintainInterval is hoe vaak er gekeken wordt of het token nog lang genoeg
// meegaat.
const maintainInterval = time.Minute

// session bewaart het token en zorgt dat het geldig blijft.
type session struct {
	mu     sync.Mutex
	config myuplink.Config
	tokens *myuplink.Tokens
	http   *http.Client

	// save legt het token vast buiten dit proces. notify meldt aan de gebruiker
	// wat hij zelf moet oplossen. Allebei als functie, zodat een test de sessie
	// kan aandrijven zonder een draaiende Stulp.
	save   func(*myuplink.Tokens) error
	notify func(string)

	lastErr string
}

// errNotLinked is wat elke aanroep krijgt zolang er geen token is. De tekst is
// de melding die de gebruiker op de tegel van zijn pomp te zien krijgt, dus hij
// zegt wat hij moet doen.
var errNotLinked = errors.New("deze app is nog niet gekoppeld met myUplink; open de instellingen van de app")

func newSession(save func(*myuplink.Tokens) error, notify func(string)) *session {
	return &session{http: myuplink.DefaultHTTP(), save: save, notify: notify}
}

// setConfig legt de registratie vast. Het token blijft staan: hoort het bij een
// andere registratie, dan zegt de tokenserver dat bij de eerste verversing en
// dat is duidelijker dan het hier stilletjes weggooien.
func (s *session) setConfig(config myuplink.Config) {
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
}

func (s *session) setTokens(tokens *myuplink.Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.lastErr = tokens, ""
	return s.save(tokens)
}

// token levert een geldig access-token aan de API-client.
//
// De lock blijft tijdens het verversen liggen. Dat houdt een tweede poll even
// op, en dat is precies de bedoeling: myUplink wisselt bij elke verversing het
// refresh-token om, dus twee verversingen tegelijk maken elkaar ongeldig.
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
//
// Zonder dit zou het verversen pas gebeuren bij de eerstvolgende poll, en die
// zou dan wachten op de tokenserver. Met dit gebeurt het ertussen door.
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
//
// Welke van de twee wegen dat is, staat in het token zelf: alleen de weg langs
// de browser levert een refresh-token op. Dat scheelt een tweede plek waar de
// keuze bewaard moet worden en die na een herstart uit de pas kan lopen met wat
// er werkelijk ligt.
func (s *session) refreshLocked(ctx context.Context) error {
	machine := s.tokens.RefreshToken == ""

	var tokens *myuplink.Tokens
	var err error
	if machine {
		tokens, err = s.config.ClientCredentials(ctx, s.http)
	} else {
		tokens, err = s.config.Refresh(ctx, s.http, s.tokens.RefreshToken)
	}
	if err != nil {
		s.lastErr = err.Error()

		// Een definitieve weigering is het einde van deze koppeling. Het token
		// bewaren zou de app elke minuut hetzelfde dode papiertje laten
		// aanbieden; weggooien maakt duidelijk dat er iets van de gebruiker
		// nodig is, en de melding zegt wat.
		//
		// Bij client_credentials ligt het nooit aan een verlopen koppeling maar
		// aan de registratie zelf, dus de melding wijst daarheen.
		var refused *myuplink.AuthError
		if errors.As(err, &refused) && refused.Final() {
			s.tokens = nil
			if saveErr := s.save(nil); saveErr != nil {
				return fmt.Errorf("%w (en het opruimen mislukte: %v)", err, saveErr)
			}
			if machine {
				s.notify("Nibe: myUplink weigert de client-id en het geheim. Controleer de registratie in de instellingen van de app.")
			} else {
				s.notify("Nibe: myUplink accepteert de koppeling niet meer. Koppel de app opnieuw in de instellingen.")
			}
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

// machine zegt of het huidige token op naam van de registratie staat in plaats
// van op naam van een persoon. Alleen de weg langs de browser levert een
// refresh-token op, dus dat ene veld is het antwoord.
func (s *session) machine() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens != nil && s.tokens.RefreshToken == ""
}

func (s *session) registration() myuplink.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// stored is de vorm waarin het token in de state van de app staat. Een omhulsel
// in plaats van het token zelf, zodat er later iets bij kan zonder dat een
// bestaande installatie zijn koppeling kwijtraakt.
type stored struct {
	Tokens *myuplink.Tokens `json:"tokens,omitempty"`
}

func readStored(raw json.RawMessage) (*myuplink.Tokens, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var state stored
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("de bewaarde myUplink-koppeling is onleesbaar: %w", err)
	}
	return state.Tokens, nil
}

func writeStored(tokens *myuplink.Tokens) (json.RawMessage, error) {
	if tokens == nil {
		return nil, nil
	}
	return json.Marshal(stored{Tokens: tokens})
}
