package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/somfy/internal/tahoma"
)

// De configuratiepagina van deze app.
//
// Twee vragen: met welk Somfy-account, en hoe vaak er gekeken mag worden. Alles
// daarna komt daaruit -- welke rolluiken er zijn, hoe ze heten, welk type ze
// zijn. Er is geen veld voor een adres of een doos-id: het account weet zelf
// welke TaHoma erbij hoort.
//
// Het wachtwoord gaat één kant op. Het wordt opgeslagen in de state van de
// plugin en komt nooit terug naar de pagina; wie het kwijt is vult een nieuw in.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		stored := a.account()
		a.mu.RLock()
		connected := a.client != nil
		lastErr := a.lastErr
		devices := len(a.devices)
		lastOK := a.lastOK
		a.mu.RUnlock()

		answer := map[string]any{
			"username":    stored.Username,
			"hasPassword": stored.Password != "",
			"interval":    int(stored.interval() / time.Second),
			"connected":   connected && lastErr == "",
			"error":       lastErr,
			"devices":     devices,
		}
		if !lastOK.IsZero() {
			answer["secondsAgo"] = int(time.Since(lastOK) / time.Second)
		}
		return answer, nil
	})

	// test probeert de opgegeven gegevens meteen uit, zonder ze te bewaren. Zo
	// weet iemand vóór het opslaan of het klopt, en waaraan het lag.
	stulp.OnRequest("test", func(_, body map[string]any) (any, error) {
		username, _ := body["username"].(string)
		password, _ := body["password"].(string)
		if password == "" {
			// Leeg betekent "houd wat er staat"; anders zou proberen altijd
			// mislukken voor wie alleen zijn poll-tijd wil aanpassen.
			password = a.account().Password
		}
		if username == "" || password == "" {
			return nil, fmt.Errorf("vul een gebruikersnaam en een wachtwoord in")
		}
		client := tahoma.New(username, password)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := client.Login(ctx); err != nil {
			return nil, err
		}
		setup, err := client.Setup(ctx)
		if err != nil {
			return nil, err
		}
		scenarios, err := client.Scenarios(ctx)
		if err != nil {
			// Scenario's zijn een extraatje; wie ze niet heeft hoort geen fout
			// te zien bij het uitproberen van zijn wachtwoord.
			scenarios = nil
		}
		return map[string]any{
			"devices":   len(setup.Devices),
			"scenarios": len(scenarios),
			"supported": supported(setup.Devices),
			"unknown":   unknown(setup.Devices),
		}, nil
	})

	// save bewaart en verbindt meteen opnieuw. Wachten op een herstart zou
	// betekenen dat iemand die net zijn wachtwoord veranderd heeft niet ziet of
	// het werkte.
	stulp.OnRequest("save", func(_, body map[string]any) (any, error) {
		stored := a.account()
		if username, ok := body["username"].(string); ok && username != "" {
			stored.Username = username
		}
		if password, ok := body["password"].(string); ok && password != "" {
			stored.Password = password
		}
		if seconds, ok := body["interval"].(float64); ok && seconds > 0 {
			stored.Seconds = int(seconds)
		}
		if stored.Username == "" || stored.Password == "" {
			return nil, fmt.Errorf("vul een gebruikersnaam en een wachtwoord in")
		}
		if err := a.saveAccount(stored); err != nil {
			return nil, err
		}
		a.connect()
		return map[string]any{"interval": int(stored.interval() / time.Second)}, nil
	})

	// forget maakt de gegevens leeg en zet de poll stil. Dat is wat de logout
	// van de bron deed, plus het weggooien van wat hier bewaard was.
	stulp.OnRequest("forget", func(map[string]any, map[string]any) (any, error) {
		if client, err := a.api(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// Een uitlog die mislukt mag het vergeten niet tegenhouden: de
			// gebruiker wil van zijn gegevens af, niet van een foutmelding.
			_ = client.Logout(ctx)
		}
		if err := a.saveAccount(account{}); err != nil {
			return nil, err
		}
		a.connect()
		return map[string]any{"ok": true}, nil
	})
}

// supported telt per driver wat er te koppelen valt. Dat is wat iemand op de
// pagina wil zien: niet dat er 34 apparaten zijn, maar dat er 6 rolluiken bij
// zitten.
func supported(devices []tahoma.Device) map[string]int {
	counts := map[string]int{}
	for _, device := range devices {
		for id, kind := range coverings {
			if kind.handles(device.ControllableName) {
				counts[id]++
			}
		}
	}
	return counts
}

// unknown levert de typen die deze app niet aankan, op naam.
//
// Dat is geen foutmelding maar de nuttigste lijst op de pagina: het is precies
// wat er nog te porten valt, en het komt van de doos van de gebruiker zelf in
// plaats van uit een aanname hier.
func unknown(devices []tahoma.Device) []string {
	seen := map[string]bool{}
	for _, device := range devices {
		if device.ControllableName == "" {
			continue
		}
		handled := false
		for _, kind := range coverings {
			if kind.handles(device.ControllableName) {
				handled = true
				break
			}
		}
		if !handled {
			seen[device.ControllableName] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
