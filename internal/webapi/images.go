package webapi

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/stulphttp"
)

// UseImages hangt de gedeelde afbeeldingen aan de interface. Zonder is de route er
// wel en zegt hij dat er niets te halen valt -- duidelijker dan een adres dat
// bestaat in het ene huis en niet in het andere.
func (s *Server) UseImages(store *imageshare.Store) { s.images = store }

// Een gedeelde afbeelding uitleveren.
//
// Buiten /api/ met opzet: dit adres is er voor een service worker die een
// pushafbeelding ook zonder open browsersessie moet kunnen ophalen. Wat het
// adres beschermt staat in internal/imageshare: niet te raden en niet lang
// geldig.
func (s *Server) handleImages() {
	s.mux.HandleFunc("GET /image/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if s.images == nil {
			stulphttp.Error(response, "deze Stulp deelt geen afbeeldingen", stulphttp.StatusNotFound)
			return
		}
		image, ok := s.images.Get(request.PathValue("id"))
		if !ok {
			// Verlopen en nooit bestaan zijn hier hetzelfde: in beide gevallen is er
			// niets, en welke van de twee het was hoort niemand buiten Stulp te
			// kunnen aflezen.
			stulphttp.Error(response, "deze afbeelding bestaat niet meer", stulphttp.StatusNotFound)
			return
		}
		source, err := image.Resolve(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, fmt.Errorf("the app could not prepare this image: %w", err))
			return
		}
		target, err := checkedMediaURL(source.URL)
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, err)
			return
		}
		upstream, err := stulphttp.Fetch(request, target, nil, 20*time.Second)
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, fmt.Errorf("the app does not serve this image: %w", err))
			return
		}
		defer upstream.Body.Close()
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			writeError(response, stulphttp.StatusBadGateway,
				fmt.Errorf("the app answered %s for this image", upstream.Status))
			return
		}

		contentType := source.ContentType
		if contentType == "" {
			contentType = upstream.Header("Content-Type")
		}
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
		if !strings.HasPrefix(mediaType, "image/") {
			writeError(response, stulphttp.StatusBadGateway, errors.New("the app did not serve an image content type"))
			return
		}

		contentLength, knownLength, err := imageContentLength(upstream.Header("Content-Length"))
		if err != nil {
			writeError(response, stulphttp.StatusBadGateway, err)
			return
		}
		if contentLength > imageshare.MaxBytes {
			writeError(response, stulphttp.StatusBadGateway,
				fmt.Errorf("the image is larger than the %d byte limit", imageshare.MaxBytes))
			return
		}

		response.Header().Set("Content-Type", contentType)
		if knownLength {
			response.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
		}
		// Niet bewaren: de afbeelding hoort bij deze melding en bij geen volgende.
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(stulphttp.StatusOK)
		if err := copyImage(response, upstream.Body); err != nil && s.options.Logger != nil {
			s.options.Logger.Warn("shared image stream ended", "error", err)
		}
	})
}

func imageContentLength(value string) (length int64, known bool, err error) {
	if value == "" {
		return 0, false, nil
	}
	length, err = strconv.ParseInt(value, 10, 64)
	if err != nil || length < 0 {
		return 0, false, errors.New("the app gave an invalid image length")
	}
	return length, true, nil
}

// copyImage gebruikt één kleine vaste buffer. Bij een antwoord zonder lengte
// wordt na MaxBytes afgekapt; een volledige foto staat dus op geen enkel moment
// in de heap van Stulp.
func copyImage(response stulphttp.ResponseWriter, body io.Reader) error {
	buffer := make([]byte, 8<<10)
	written, err := io.CopyBuffer(response, io.LimitReader(body, imageshare.MaxBytes), buffer)
	if err != nil {
		return err
	}
	if written < imageshare.MaxBytes {
		return nil
	}
	var extra [1]byte
	if read, readErr := body.Read(extra[:]); read > 0 {
		return fmt.Errorf("the image is larger than the %d byte limit", imageshare.MaxBytes)
	} else if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return nil
}
