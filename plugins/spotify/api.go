package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/spotify/internal/spotify"
)

// De configuratiepagina van deze app.
//
// Koppelen is drie stappen omdat er geen cloud-tussenpersoon is: registratie
// opslaan, toestemming geven in de browser, en het adres waar je uitkwam
// terugplakken. De code staat in dat adres; er hoeft op de redirect niets te
// luisteren.
//
// Anders dan bij Nibe is hier géén clientgeheim nodig -- Spotify accepteert
// PKCE voor publieke clients. Er staat dus niets in de instellingen wat je
// account opent.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		linked, expiry, lastErr := a.session.linked()
		config := a.session.registration()

		a.mu.RLock()
		devices, waiting := len(a.devices), a.pending != nil
		a.mu.RUnlock()

		answer := map[string]any{
			"clientId":    config.ClientID,
			"redirectUri": config.RedirectURI,
			"linked":      linked,
			"waiting":     waiting,
			"devices":     devices,
			"error":       lastErr,
		}
		if linked {
			answer["expiresAt"] = expiry.UTC().Format(time.RFC3339)
		}
		return answer, nil
	})

	// authorize begint een autorisatie met wat er in de instellingen staat. De
	// pagina slaat die eerst op; dit leest ze terug, zodat er maar één plek is
	// waar de registratie vandaan komt.
	stulp.OnRequest("authorize", func(map[string]any, map[string]any) (any, error) {
		a.readConfig()
		authorization, err := a.session.registration().Authorize()
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.pending = &authorization
		a.mu.Unlock()
		return map[string]any{"url": authorization.URL}, nil
	})

	// exchange wisselt om wat de gebruiker terugplakte.
	stulp.OnRequest("exchange", func(_, body map[string]any) (any, error) {
		pasted, _ := body["redirect"].(string)

		a.mu.RLock()
		pending := a.pending
		a.mu.RUnlock()
		if pending == nil {
			return nil, fmt.Errorf("er loopt geen autorisatie; begin met Autoriseren")
		}
		code, err := spotify.CodeFromRedirect(pasted, pending.State)
		if err != nil {
			return nil, err
		}

		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		tokens, err := a.session.registration().Exchange(ctx, a.session.http, code, pending.Verifier)
		if err != nil {
			return nil, err
		}
		if err := a.session.setTokens(tokens); err != nil {
			return nil, err
		}
		// De code is nu op: Spotify neemt hem geen tweede keer aan, en een
		// autorisatie laten staan die niets meer waard is nodigt uit tot
		// verwarring.
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()

		a.refreshAll()
		return a.describe(ctx)
	})

	// check kijkt of het token het nog doet en wat Spotify nu ziet.
	stulp.OnRequest("check", func(map[string]any, map[string]any) (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return a.describe(ctx)
	})

	// disconnect gooit het token weg. De registratie blijft staan: die hoort bij
	// de installatie en niet bij de inlog.
	stulp.OnRequest("disconnect", func(map[string]any, map[string]any) (any, error) {
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()
		if err := a.session.setTokens(nil); err != nil {
			return nil, err
		}
		return map[string]any{"linked": false}, nil
	})
}

// describe vertelt welke apparaten dit token ziet. Dat is meteen de proef of de
// koppeling werkt -- en de gebruiker herkent zijn eigen speakers, wat beter
// overtuigt dan het woord "verbonden".
func (a *app) describe(ctx context.Context) (any, error) {
	devices, err := cloud.Devices(ctx)
	if err != nil {
		return nil, err
	}
	found := []map[string]any{}
	for _, device := range devices {
		found = append(found, map[string]any{
			"name":       device.Name,
			"type":       device.Type,
			"active":     device.IsActive,
			"restricted": device.IsRestricted,
		})
	}
	return map[string]any{"linked": true, "players": found}, nil
}
