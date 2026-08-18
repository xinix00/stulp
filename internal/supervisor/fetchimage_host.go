//go:build !tamago

package supervisor

// fetchimage_host.go — een cameraplaatje ophalen bij de luisteraar van een app.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/plugin"
)

// fetchImage haalt de bytes op bij de luisteraar van de plugin.
func fetchImage(ctx context.Context, source plugin.VideoStream) ([]byte, string, error) {
	target, err := url.Parse(source.URL)
	if err != nil {
		return nil, "", fmt.Errorf("de app gaf een onbruikbaar adres: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, "", fmt.Errorf("een adres moet http of https zijn, niet %q", target.Scheme)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	answer, err := (&http.Client{}).Do(request)
	if err != nil {
		return nil, "", err
	}
	defer answer.Body.Close()
	if answer.StatusCode >= http.StatusBadRequest {
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
