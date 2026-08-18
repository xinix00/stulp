// Package stulphttp is de HTTP-vorm waar Stulp's handlers tegen geschreven zijn:
// één set namen, met net/http eronder op een host en leanhttp op een HopOS-node.
//
// Waarom dit bestaat: net/http linkt crypto/tls onvoorwaardelijk mee, ook als er
// nooit een https-URL langskomt. In een slot-image is dat gemeten ~1,5 MB voor
// webapi en appinstall samen, op een image van 7,6 MB. Op een host kost het
// niets, want daar staat de stdlib er al.
//
// De vorm is met OPZET geen adapter. Op een host zijn deze namen ALIASSEN van
// net/http (zie shape_host.go), dus daar verandert er letterlijk niets aan het
// gedrag: dezelfde server, dezelfde parser, dezelfde router. Een adapter zou een
// laag van mijn code tussen een browser en Stulp zetten, en dat is precies de
// plek waar je geen nieuwe laag wilt. Op een node zijn het aliassen van leanhttp
// (shape_node.go), en dáár is de nieuwe laag onvermijdelijk — maar daar staat
// ook nog niets dat er stuk van kan.
//
// Wat de handlers hierdoor NIET meer mogen doen, en de vervanging:
//
//	r.URL.Query()      → stulphttp.Query(r)
//	r.URL.Path         → stulphttp.Path(r)
//	w.(http.Flusher)   → stulphttp.Flush(w)
//	http.ServeFileFS   → alleen op een host; zie webapi's appui (bestanden van
//	                     een app-bundel bestaan niet op een node)
//
// Die vier zijn de enige plekken waar de twee kanten echt uiteenlopen; al het
// andere (PathValue, Header.Get/Set, WriteHeader, Write, statuscodes, methodes)
// heet aan beide kanten hetzelfde.
package stulphttp

import "io"

// Statuscodes en methodes staan hier en niet als alias, zodat één lijst voor
// beide doelen geldt. De waarden zijn de getallen uit de HTTP-specificatie; er
// is niets platformspecifieks aan.
const (
	StatusOK                    = 200
	StatusCreated               = 201
	StatusNoContent             = 204
	StatusBadRequest            = 400
	StatusUnauthorized          = 401
	StatusForbidden             = 403
	StatusNotFound              = 404
	StatusMethodNotAllowed      = 405
	StatusConflict              = 409
	StatusUnprocessableEntity   = 422
	StatusRequestEntityTooLarge = 413
	StatusUnsupportedMediaType  = 415
	StatusInternalServerError   = 500
	StatusBadGateway            = 502
	StatusServiceUnavailable    = 503
	StatusGatewayTimeout        = 504
)

const (
	MethodGet     = "GET"
	MethodPost    = "POST"
	MethodPut     = "PUT"
	MethodPatch   = "PATCH"
	MethodDelete  = "DELETE"
	MethodOptions = "OPTIONS"
)

// Reply is het antwoord op een Fetch: net genoeg om een stroom door te geven.
// Sluit de Body.
type Reply struct {
	StatusCode int
	Status     string
	Body       io.ReadCloser

	header func(string) string
}

// Header geeft een header van het antwoord ("" als hij ontbreekt).
func (r *Reply) Header(key string) string {
	if r.header == nil {
		return ""
	}
	return r.header(key)
}
