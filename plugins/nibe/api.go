package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/nibe/internal/myuplink"
)

// De configuratiepagina van deze app.
//
// Hier zit de koppeling die de bron aan Homey uitbesteedde. Homey had een
// cloud-callback (callback.athom.com) waar myUplink de gebruiker na het inloggen
// naartoe stuurde; Stulp heeft die niet en krijgt hem niet. Dus doen we het in
// drie stappen die geen tussenpersoon nodig hebben:
//
//  1. de gebruiker vult zijn eigen registratie in (dev.myuplink.com);
//  2. de app bouwt de autorisatie-URL, hij logt in bij Nibe;
//  3. hij plakt het adres terug waar de browser uitkwam, en de app wisselt de
//     code daarin om voor een token.
//
// Op de redirect hoeft niets te luisteren -- de code staat in de adresbalk, en
// dat is alles wat er nodig is. Daarmee werkt elk adres dat de gebruiker zelf
// registreert, ook eentje dat nergens naartoe gaat.

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
			"hasSecret":   config.ClientSecret != "",
			"linked":      linked,
			"waiting":     waiting,
			"devices":     devices,
			"error":       lastErr,
		}
		if linked {
			answer["expiresAt"] = expiry.UTC().Format(time.RFC3339)
			answer["machine"] = a.session.machine()
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
		code, err := myuplink.CodeFromRedirect(pasted, pending.State)
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
		// De code is nu op: myUplink neemt hem geen tweede keer aan, en een
		// autorisatie laten staan die niets meer waard is nodigt uit tot
		// verwarring.
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()

		// De pompen die al gekoppeld waren stonden tot nu toe op onbereikbaar.
		// Wachten tot hun volgende ronde zou dat nog vijf minuten zo laten.
		a.refreshAll()

		return a.describe(ctx)
	})

	// connect is de andere weg: een token op naam van de registratie zelf. Geen
	// browser, niets om terug te plakken -- de knop is de hele koppeling.
	stulp.OnRequest("connect", func(map[string]any, map[string]any) (any, error) {
		a.readConfig()

		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		tokens, err := a.session.registration().ClientCredentials(ctx, a.session.http)
		if err != nil {
			return nil, err
		}
		if err := a.session.setTokens(tokens); err != nil {
			return nil, err
		}

		// Een halve autorisatie langs de browser is nu betekenisloos geworden.
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()

		a.refreshAll()
		return a.describe(ctx)
	})

	// check kijkt of het token het nog doet en wat myUplink te bieden heeft.
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

// describe vertelt wat er met dit token te vinden is. Dat is meteen de proef of
// de koppeling werkt -- en de gebruiker ziet de naam van zijn eigen huis, wat
// beter overtuigt dan het woord "verbonden".
func (a *app) describe(ctx context.Context) (any, error) {
	systems, err := cloud.Systems(ctx)
	if err != nil {
		return nil, err
	}
	found := []map[string]any{}
	for _, system := range systems {
		for _, device := range system.Devices {
			found = append(found, map[string]any{
				"system":    system.Name,
				"name":      device.Product.Name,
				"serial":    device.Product.SerialNumber,
				"connected": device.ConnectionState == "Connected",
			})
		}
	}
	return map[string]any{"linked": true, "pumps": found}, nil
}
