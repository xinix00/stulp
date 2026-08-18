//go:build !tamago

package stulphttp

// shape_host.go — op een host IS dit net/http.
//
// Aliassen en geen wrappers: een handler die hier tegen geschreven is, is
// dezelfde handler die net/http zelf zou aanroepen. Er zit dus geen regel van
// deze code tussen een browser en Stulp, en dat is de bedoeling — de host is
// waar het echte huis op draait.

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"time"
)

type (
	Handler        = http.HandlerFunc
	ResponseWriter = http.ResponseWriter
	Request        = http.Request
	Header         = http.Header
)

// Mux wikkelt de router van de gedaante, met één extra: hij onthoudt zijn
// patronen. Niet voor runtime maar voor de wacht in webapi/nodemux_test.go:
// de host-suite legt de VOLLEDIGE routetabel tegen leanhttp's node-mux, zodat
// een patroon dat dáár conflicteert of {$} draagt lokaal in go test knalt —
// en niet, zoals 14-08 twee keer op rij, als panic op het ijzer.
type Mux struct {
	inner    *http.ServeMux
	patterns []string
}

// NewServeMux geeft net/http's router, met zijn eigen patronen en precedentie.
func NewServeMux() *Mux { return &Mux{inner: http.NewServeMux()} }

func (m *Mux) HandleFunc(pattern string, h Handler) {
	m.patterns = append(m.patterns, pattern)
	m.inner.HandleFunc(pattern, h)
}

func (m *Mux) ServeHTTP(w ResponseWriter, r *Request) { m.inner.ServeHTTP(w, r) }

// Patterns is de geregistreerde tabel, voor de node-mux-wacht.
func (m *Mux) Patterns() []string { return append([]string(nil), m.patterns...) }

// Query is r.URL.Query(): leanhttp heeft geen URL-veld, dus vragen de handlers
// het hier.
func Query(r *Request) url.Values { return r.URL.Query() }

// Path is het %-gedecodeerde pad.
func Path(r *Request) string { return r.URL.Path }

// Flush duwt wat er gebufferd staat naar de client, voor SSE en mediastromen.
// Op een host kan niet elke writer dat (een middleware die hem inpakt kan het
// weglaten), dus is het hier een vraag met een antwoord in plaats van een
// aanname.
// RootPattern is het patroon voor exact "/" — de ene plek waar de twee
// gedaanten écht verschillen: net/http spelt dat {$} (zonder is "/" een
// vangnet voor alles), leanhttp kent geen {$} en zijn "/" is de wortel-subtree.
const RootPattern = "GET /{$}"

func Flush(w ResponseWriter) bool {
	f, ok := w.(http.Flusher)
	if ok {
		f.Flush()
	}
	return ok
}

// Error antwoordt met een platte tekst en die status.
func Error(w ResponseWriter, msg string, status int) { http.Error(w, msg, status) }

// NotFound is het kale 404-antwoord.
func NotFound(w ResponseWriter, r *Request) { http.NotFound(w, r) }

// ---- de vier plekken waar de twee kanten echt verschillen -------------------
//
// Op een host is dit steeds net/http's eigen implementatie: bereikbereiken,
// If-Modified-Since, content-sniffing en alle hoekgevallen die daar in dertien
// jaar in gegaan zijn. Er is geen reden om daar iets van mij tussen te zetten.

// ServeFS levert één bestand uit een fs.FS.
func ServeFS(w ResponseWriter, r *Request, fsys fs.FS, name string) {
	http.ServeFileFS(w, r, fsys, name)
}

// ServeFSPrefix geeft een handler die een hele boom uitlevert, met prefix eraf.
func ServeFSPrefix(prefix string, fsys fs.FS) Handler {
	handler := http.StripPrefix(prefix, http.FileServer(http.FS(fsys)))
	return handler.ServeHTTP
}

// ServeContent levert een leesbare, seekbare inhoud uit (dus met bereiken).
func ServeContent(w ResponseWriter, r *Request, name string, modtime time.Time, content io.ReadSeeker) {
	http.ServeContent(w, r, name, modtime, content)
}

// LimitBody begrenst wat een handler uit de body leest.
func LimitBody(w ResponseWriter, r *Request, n int64) io.Reader {
	return http.MaxBytesReader(w, r.Body, n)
}

// Fetch haalt een URL op namens een handler: de stroom van een app doorgeven.
// header mag nil zijn; Range is de enige die Stulp doorstuurt.
func Fetch(r *Request, url string, header map[string]string) (*Reply, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		request.Header.Set(k, v)
	}
	answer, err := (&http.Client{}).Do(request)
	if err != nil {
		return nil, err
	}
	return &Reply{
		StatusCode: answer.StatusCode,
		Status:     answer.Status,
		Body:       answer.Body,
		header:     answer.Header.Get,
	}, nil
}

// Context is de levensduur van het verzoek, om aan een store-aanroep mee te
// geven. Op een host is dat net/http's eigen context: hij eindigt als de client
// weggaat.
func Context(r *Request) context.Context { return r.Context() }

// Done sluit als de client weg is. Voor een handler die een stroom produceert en
// moet weten wanneer hij mag stoppen.
func Done(r *Request) <-chan struct{} { return r.Context().Done() }

// CloseBody sluit de body van het verzoek. Een handler die klaar is met lezen
// hoort dat te doen; wat het kost verschilt per doel.
func CloseBody(r *Request) { r.Body.Close() }
