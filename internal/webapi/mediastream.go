package webapi

import (
	"errors"
	"fmt"
	"io"

	"net/url"
	"github.com/xinix00/stulp/internal/stulphttp"
	"time"

	"github.com/xinix00/stulp/internal/plugin"
)

// Camerabeeld doorgeven.
//
// Stulp pakt geen video om en decodeert niets. Wat een camera spreekt weet
// alleen de plugin die hem kent, en die bedient de stream op zijn eigen
// luisteraar. Hier worden alleen de bytes doorgegeven.
//
// Dat scheelde 6,7 MB en zestien modules aan RTSP- en WebRTC-code in een binary
// die ook draait in huizen zonder camera. Nu betaalt alleen de plugin die het
// nodig heeft, en die kiest zelf wat hij aan boord neemt.

// streamIdleTimeout is hoe lang er niets binnen mag komen voordat Stulp de
// verbinding opgeeft. Een livestream die stilvalt is stuk; de kijker hoort dat
// te merken in plaats van naar een bevroren beeld te blijven staren.
const streamIdleTimeout = 30 * time.Second

func (s *Server) pipeStream(response stulphttp.ResponseWriter, request *stulphttp.Request, stream plugin.VideoStream) {
	target, err := checkedStreamURL(stream.URL)
	if err != nil {
		writeError(response, stulphttp.StatusBadGateway, err)
		return
	}

	// Geen eigen timeout op de client: een livestream duurt zo lang als iemand
	// kijkt. Wat wél bewaakt wordt is stilte, hieronder.
	var forwarded map[string]string
	if forward := request.Header.Get("Range"); forward != "" {
		forwarded = map[string]string{"Range": forward}
	}
	source, err := stulphttp.Fetch(request, target, forwarded)
	if err != nil {
		writeError(response, stulphttp.StatusBadGateway, fmt.Errorf("the app does not serve this stream: %w", err))
		return
	}
	defer source.Body.Close()
	if source.StatusCode >= stulphttp.StatusBadRequest {
		writeError(response, stulphttp.StatusBadGateway,
			fmt.Errorf("the app answered %s for this stream", source.Status))
		return
	}

	contentType := stream.ContentType
	if contentType == "" {
		contentType = source.Header("Content-Type")
	}
	if contentType == "" {
		// Raden zou de browser iets laten proberen wat het niet is. Een lege
		// Content-Type is eerlijker dan een verkeerde.
		writeError(response, stulphttp.StatusBadGateway, errors.New("the app did not say what this stream is"))
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "no-store")
	if length := source.Header("Content-Length"); length != "" {
		response.Header().Set("Content-Length", length)
	}
	response.WriteHeader(source.StatusCode)
	copyStream(response, source.Body)
}

// copyStream duwt door zodra er iets is. Zonder de flush blijft een livestream
// in de buffer hangen tot die vol is, en dan kijk je naar een beeld van een
// halve minuut geleden.
func copyStream(response stulphttp.ResponseWriter, body io.ReadCloser) {
	activity := watchForSilence(body)
	// De wachter stopt zodra het kopiëren stopt, want anders blijft hij tot de
	// stiltegrens staan voor een stream die allang klaar is.
	defer close(activity)
	buffer := make([]byte, 32*1024)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			select {
			case activity <- struct{}{}:
			default:
			}
			if _, writeErr := response.Write(buffer[:read]); writeErr != nil {
				return
			}
			stulphttp.Flush(response)
		}
		if err != nil {
			return
		}
	}
}

// watchForSilence sluit de bron als er te lang niets komt.
//
// Read op een dode verbinding komt uit zichzelf niet terug: TCP merkt een
// camera die zwijgt zonder de verbinding te sluiten niet op. Zonder deze wachter
// blijft de goroutine staan zolang het proces leeft, en de kijker staart naar
// een bevroren beeld zonder te weten dat er niets meer komt.
//
// Het kanaal is gebufferd zodat de lezer nooit blokkeert op het melden.
func watchForSilence(body io.Closer) chan<- struct{} {
	activity := make(chan struct{}, 1)
	go func() {
		timer := time.NewTimer(streamIdleTimeout)
		defer timer.Stop()
		for {
			select {
			case _, open := <-activity:
				if !open {
					return
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(streamIdleTimeout)
			case <-timer.C:
				body.Close()
				return
			}
		}
	}()
	return activity
}

// checkedStreamURL laat alleen gewone HTTP door.
//
// De plugin geeft dit adres, en een plugin is code die de gebruiker heeft
// geïnstalleerd -- maar file:// of een ander schema zou van Stulp een middel
// maken om bij dingen te komen die niets met een camera te maken hebben.
func checkedStreamURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the app gave an unusable stream address: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("a stream address must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("a stream address needs a host")
	}
	return parsed.String(), nil
}
