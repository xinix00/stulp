package webapi

import (
	"fmt"

	"github.com/xinix00/stulp/internal/stulphttp"

	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/units"
)

// De schakelaars van Stulp zelf.
//
// Ze staan in het document en niet in een vlag: een vlag vraagt een herstart met
// andere argumenten, en dat is geen keuze die je even maakt. Zo staat Stulp
// tussen de apps in Manage, met een instellingenpagina zoals elke app er een
// heeft.

func (s *Server) handleSystemSettings() {
	s.mux.HandleFunc("GET /api/stulp/system", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		system, err := s.store.System(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.systemAnswer(system))
	})

	s.mux.HandleFunc("PUT /api/stulp/system", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			Statistics *bool `json:"statistics"`
			// Units komt als grootheid-naar-eenheid binnen, zodat één keuze
			// verstuurd kan worden zonder de andere vier mee te sturen.
			Units map[string]string `json:"units"`
		}
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		system, err := s.store.System(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		// Alleen wat meegestuurd is verandert: een PUT met één veld hoort de
		// rest niet stilletjes terug te zetten.
		if body.Statistics != nil {
			system.Statistics = *body.Statistics
		}
		for quantity, unit := range body.Units {
			chosen, ok := system.Units.Choose(quantity, unit)
			if !ok {
				// Weigeren en niet negeren: een eenheid die Stulp niet kent zou
				// canoniek gaan lezen, en dan lijkt de instelling stuk.
				writeError(response, stulphttp.StatusBadRequest,
					fmt.Errorf("%q is geen eenheid voor %q", unit, quantity))
				return
			}
			system.Units = chosen
		}
		if err := s.store.SetSystem(stulphttp.Context(request), system); err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		s.applySystem(system)
		writeJSON(response, stulphttp.StatusOK, s.systemAnswer(system))
	})
}

// applySystem laat een nieuwe keuze meteen gelden, zonder herstart.
func (s *Server) applySystem(system store.System) {
	s.rememberUnits(system.Units)
	if s.stats == nil {
		return
	}
	switch {
	case system.Statistics && !s.stats.Running():
		s.stats.Start(s.store)
	case !system.Statistics && s.stats.Running():
		s.stats.Close()
		// Weggooien wat er ligt: het geheugen vasthouden van iets dat niet meer
		// bijgewerkt wordt kost evenveel en klopt niet meer.
		s.stats.Forget()
	}
}

func (s *Server) systemAnswer(system store.System) map[string]any {
	answer := map[string]any{
		"statistics": system.Statistics,
		// Filled zodat de pagina altijd een keuze kan aanwijzen, en de lijst
		// erbij: de keuzelijsten komen uit de kern en niet uit een tweede lijstje
		// in de browser, want twee lijsten lopen uit elkaar.
		"units":      system.Units.Filled(),
		"unitsOffer": units.Quantities(),
	}
	if s.stats != nil {
		answer["statisticsRunning"] = s.stats.Running()
		answer["statisticsBytes"] = s.stats.Bytes()
	}
	return answer
}
