//go:build tamago

package supervisor

// fetchimage_tamago.go — hetzelfde plaatje, maar op een node.
//
// leanhttp in plaats van net/http, en dat is geen smaakkwestie: net/http linkt
// crypto/tls onvoorwaardelijk mee, en dat is ~480 KB symbolen in een slot-image
// voor een GET van één plaatje.
//
// Daarmee valt https weg, en dat mag hier: een cameraplugin op een node is een
// slot met een eigen IP op het interne switch-netwerk, en dat netwerk is precies
// wat HOP per slot isoleert. Wie wél https wil, zegt dat luid in plaats van dat
// het stil niet werkt.

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/plugin"
)

// imageClient poolt zijn verbindingen: een interface die een camera in beeld
// houdt vraagt hetzelfde plaatje elke paar seconden opnieuw.
var imageClient leanhttp.Client

func fetchImage(_ context.Context, source plugin.VideoStream) ([]byte, string, error) {
	target, err := url.Parse(source.URL)
	if err != nil {
		return nil, "", fmt.Errorf("de app gaf een onbruikbaar adres: %w", err)
	}
	if target.Scheme != "http" {
		return nil, "", fmt.Errorf("op een node kan alleen http: dit adres is %q -- "+
			"een app op een slot serveert zijn beeld op het interne netwerk", target.Scheme)
	}
	answer, err := imageClient.Do(leanhttp.Call{
		Method: "GET", URL: target.String(), Timeout: 20 * time.Second,
	})
	if err != nil {
		return nil, "", err
	}
	defer answer.Body.Close()
	if answer.StatusCode >= leanhttp.StatusBadRequest {
		return nil, "", fmt.Errorf("de app antwoordde %s", answer.Status)
	}
	// Eén byte meer lezen dan er mag passen, zodat een te grote afbeelding te
	// onderscheiden is van een die precies past. Afkappen zou een half plaatje
	// opleveren.
	data, err := io.ReadAll(io.LimitReader(answer.Body, imageshare.MaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > imageshare.MaxBytes {
		return nil, "", fmt.Errorf("de afbeelding is groter dan %d bytes", imageshare.MaxBytes)
	}
	contentType := source.ContentType
	if contentType == "" {
		contentType = answer.Header.Get("Content-Type")
	}
	return data, contentType, nil
}
