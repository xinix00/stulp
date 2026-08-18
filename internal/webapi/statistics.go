package webapi

import (
	"errors"

	"strconv"
	"github.com/xinix00/stulp/internal/stulphttp"

	"github.com/xinix00/stulp/internal/stats"
)

// Wat het huis gedaan heeft, uit het geheugen.
//
// Er is geen bewaarplek en die komt er ook niet: stroom eruit is statistiek weg.
// Dat is de prijs voor een controller zonder database, en hij is bewust betaald
// -- wie een jaar aan grafieken wil, laat iets anders deze API uitlezen en zijn
// eigen historie bijhouden.

var (
	errNoStatistics = errors.New("statistics are not being collected")
	errNoSeries     = errors.New("nothing has been recorded for this device and capability")
)

func (s *Server) handleStatistics() {
	s.mux.HandleFunc("GET /api/stulp/statistics", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if s.stats == nil {
			writeJSON(response, stulphttp.StatusOK, map[string]any{"series": []any{}, "bytes": 0})
			return
		}
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"series": s.stats.List(),
			// Wat het kost, altijd op te vragen. De belofte dat dit goedkoop is
			// hoort na te rekenen te zijn.
			"bytes": s.stats.Bytes(),
		})
	})

	s.mux.HandleFunc("GET /api/stulp/statistics/{device}/{capability}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if s.stats == nil {
			writeError(response, stulphttp.StatusNotFound, errNoStatistics)
			return
		}
		series, ok := s.stats.Series(request.PathValue("device"), request.PathValue("capability"))
		if !ok {
			writeError(response, stulphttp.StatusNotFound, errNoSeries)
			return
		}
		tier := tierFor(stulphttp.Query(request).Get("window"))
		slots := series.Window(tier)
		out := make([]map[string]any, 0, len(slots))
		for _, slot := range slots {
			entry := map[string]any{
				"at":    slot.Start(tier).Format("2006-01-02T15:04:05Z07:00"),
				"count": slot.Count,
			}
			switch series.Kind {
			case stats.Counter:
				entry["used"] = slot.Delta()
			case stats.Fraction:
				entry["on"] = slot.Average()
			default:
				entry["average"] = slot.Average()
				entry["min"] = slot.Min
				entry["max"] = slot.Max
			}
			out = append(out, entry)
		}
		writeJSON(response, stulphttp.StatusOK, map[string]any{
			"kind":   series.Kind.String(),
			"window": stats.Tiers[tier].Every.String(),
			"slots":  out,
		})
	})
}

// tierFor kiest de schaal. Een onbekend woord valt terug op de dag: dat is wat
// iemand die niets opgeeft wil zien.
func tierFor(window string) int {
	switch window {
	case "week":
		return 1
	case "month":
		return 2
	}
	if index, err := strconv.Atoi(window); err == nil && index >= 0 && index < len(stats.Tiers) {
		return index
	}
	return 0
}
