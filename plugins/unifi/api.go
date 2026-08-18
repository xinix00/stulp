package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/unifi/internal/protect"
)

// De configuratiepagina van deze app.
//
// Twee vragen: waar staat de console, en wat is de API-key. Alles daarna komt
// daaruit -- welke camera's er zijn, waar hun stream staat, hoe ze heten. Er is
// met opzet geen veld voor een RTSP-adres: de console weet dat zelf, en een
// adres dat iemand een keer heeft overgetypt is een adres dat op een dag niet
// meer klopt.

func (a *app) registerAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("status", func(map[string]any, map[string]any) (any, error) {
		a.mu.RLock()
		connected := a.client != nil
		lastErr := a.lastErr
		devices := len(a.devices)
		a.mu.RUnlock()
		return map[string]any{
			"host":      stulp.SettingText("host"),
			"port":      stulp.SettingNumber("port", 443),
			"hasKey":    stulp.SettingText("apiKey") != "",
			"connected": connected && lastErr == "",
			"error":     lastErr,
			"devices":   devices,
		}, nil
	})

	// test probeert de opgegeven gegevens meteen uit, zonder ze te bewaren.
	// Zo weet iemand vóór het opslaan of het klopt, en waaraan het lag.
	stulp.OnRequest("test", func(_, body map[string]any) (any, error) {
		host, _ := body["host"].(string)
		token, _ := body["apiKey"].(string)
		port := 443
		if value, ok := body["port"].(float64); ok && value > 0 {
			port = int(value)
		}
		if host == "" || token == "" {
			return nil, fmt.Errorf("vul een adres en een API-key in")
		}
		client := protect.New(host, port, token)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		consoles, err := client.NVRs(ctx)
		if err != nil {
			return nil, err
		}
		cameras, err := client.Cameras(ctx)
		if err != nil {
			return nil, err
		}
		lights, _ := client.Lights(ctx)
		sensors, _ := client.Sensors(ctx)
		chimes, _ := client.Chimes(ctx)
		relays, _ := client.Relays(ctx)

		answer := map[string]any{
			"cameras": len(cameras), "lights": len(lights), "sensors": len(sensors),
			"chimes": len(chimes), "relays": len(relays),
		}
		if len(consoles) > 0 {
			answer["console"] = consoles[0].Name
		}
		return answer, nil
	})
}
