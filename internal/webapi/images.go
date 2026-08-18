package webapi

import (
	"strconv"
	"github.com/xinix00/stulp/internal/stulphttp"

	"github.com/xinix00/stulp/internal/imageshare"
)

// UseImages hangt de gedeelde afbeeldingen aan de interface. Zonder is de route er
// wel en zegt hij dat er niets te halen valt -- duidelijker dan een adres dat
// bestaat in het ene huis en niet in het andere.
func (s *Server) UseImages(store *imageshare.Store) { s.images = store }

// Een gedeelde afbeelding uitleveren.
//
// Buiten /api/ met opzet: dit adres is er voor de service worker van de browser,
// en die heeft geen API-sleutel -- die ligt in localStorage, waar hij niet bij kan.
// Wat het adres beschermt staat in internal/imageshare: niet te raden, en niet
// lang geldig.
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
		response.Header().Set("Content-Type", image.ContentType)
		response.Header().Set("Content-Length", strconv.Itoa(len(image.Data)))
		// Niet bewaren: de afbeelding hoort bij deze melding en bij geen volgende.
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write(image.Data)
	})
}
