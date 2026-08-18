//go:build tamago

package stulphttp

// shape_node.go — op een node IS dit leanhttp.
//
// Dezelfde namen, dezelfde aliastruc: de handlers weten van geen van beide iets.
// Wat hier anders is dan op een host is niet de vorm maar wat eronder ligt: geen
// crypto/tls in het image, en een router die de patronen van net/http naspreekt
// (leanhttp/mux.go) inclusief {wildcards} en {path...} — {$} bestaat daar niet (KAM).

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

type (
	Handler        = leanhttp.Handler
	ResponseWriter = leanhttp.ResponseWriter
	Request        = leanhttp.Request
	Header         = leanhttp.Header
)

// Mux: zie shape_host — zelfde wrapper, zelfde reden (het patroongeheugen
// voedt de node-mux-wacht die op de host draait).
type Mux struct {
	inner    *leanhttp.Mux
	patterns []string
}

// NewServeMux geeft leanhttp's router.
func NewServeMux() *Mux { return &Mux{inner: leanhttp.NewServeMux()} }

func (m *Mux) HandleFunc(pattern string, h Handler) {
	m.patterns = append(m.patterns, pattern)
	m.inner.HandleFunc(pattern, h)
}

func (m *Mux) ServeHTTP(w ResponseWriter, r *Request) { m.inner.ServeHTTP(w, r) }

// Patterns is de geregistreerde tabel, voor de node-mux-wacht.
func (m *Mux) Patterns() []string { return append([]string(nil), m.patterns...) }

// Query ontleedt de querystring (leanhttp cachet hem zelf).
func Query(r *Request) url.Values { return r.Query() }

// Path is het %-gedecodeerde pad.
func Path(r *Request) string { return r.Path }

// Flush duwt wat er gebufferd staat naar de client. Elke leanhttp-writer kan
// dat, dus is de vraag hier altijd ja — en dat is precies waarom Flush op die
// interface staat in plaats van achter een type-assertie: een stroom die stil
// buffert heeft geen einde waarop de buffer vrijkomt.
// RootPattern: zie shape_host. Op de node is "/" de wortel-subtree van de
// leanhttp-mux, en BEWUST zonder methode: een method-loze wortel is de
// superset van élke andere route (pad én methode), dus per subset-regel
// conflictvrij — "GET /" kruiste met elke method-loze route (wel het pad,
// niet de methode: panic op het ijzer, 14-08). Een onbekend pad (of methode)
// valt dus op index.html terug in plaats van op een 404; voor een PWA met
// deep links is dat gedrag, geen gebrek.
const RootPattern = "/"

func Flush(w ResponseWriter) bool { return w.Flush() == nil }

// Error antwoordt met een platte tekst en die status.
func Error(w ResponseWriter, msg string, status int) { leanhttp.Error(w, msg, status) }

// NotFound is het kale 404-antwoord.
func NotFound(w ResponseWriter, _ *Request) { leanhttp.Error(w, "not found", StatusNotFound) }

// ---- dezelfde vier, op een node ---------------------------------------------
//
// Hier is er geen net/http om achter te schuilen, dus staat het er zelf — en met
// opzet in de eenvoudigste vorm die het werk doet. Wat een Manage-pagina nodig
// heeft is een bestand met het juiste content-type; bereikbereiken zijn voor
// video, en die loopt via de mediastroom-weg die zijn Range zelf doorgeeft.

// contentTypes is klein en expliciet: dit zijn de soorten die in Manage
// voorkomen. Sniffen doen we niet — een verkeerd geraden type is een pagina die
// als tekst wordt getoond, en dat valt pas op in een browser.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".map":         "application/json",
}

// ServeFS levert één bestand uit een fs.FS.
func ServeFS(w ResponseWriter, _ *Request, fsys fs.FS, name string) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		Error(w, "not found", StatusNotFound)
		return
	}
	if w.Header().Get("Content-Type") == "" {
		if ct := contentTypes[strings.ToLower(path.Ext(name))]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(StatusOK)
	w.Write(data)
}

// ServeFSPrefix geeft een handler die een hele boom uitlevert, met prefix eraf.
//
// Een pad met ".." erin komt hier niet aan: de mux normaliseert vóór het
// matchen. De controle blijft staan omdat deze functie ook los aangeroepen kan
// worden, en omdat een fout hier een bestand buiten de boom uitlevert.
func ServeFSPrefix(prefix string, fsys fs.FS) Handler {
	return func(w ResponseWriter, r *Request) {
		name := strings.TrimPrefix(strings.TrimPrefix(Path(r), prefix), "/")
		if name == "" || strings.Contains(name, "..") {
			Error(w, "not found", StatusNotFound)
			return
		}
		ServeFS(w, r, fsys, name)
	}
}

// ServeContent levert inhoud uit. Zonder bereiken: wie een backup ophaalt wil
// het hele bestand, en een halve zip is geen backup.
func ServeContent(w ResponseWriter, r *Request, _ string, _ time.Time, content io.ReadSeeker) {
	size, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		Error(w, "cannot size this content", StatusInternalServerError)
		return
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		Error(w, "cannot rewind this content", StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(StatusOK)
	io.Copy(w, content)
}

// LimitBody begrenset wat een handler uit de body leest. leanhttp begrenst de
// body al bij het lezen van het verzoek; dit is de tweede, kleinere grens die
// een handler zelf wil.
func LimitBody(_ ResponseWriter, r *Request, n int64) io.Reader {
	return io.LimitReader(r.Body, n)
}

// streamClient poolt zijn verbindingen: een mediastroom vraagt dezelfde app
// telkens opnieuw.
var streamClient leanhttp.Client

// Fetch haalt een URL op namens een handler. Alleen http: een app op een slot
// serveert op het interne netwerk, en https daarheen zou een TLS-stapel in dit
// image betekenen voor een verbinding die de node zelf al isoleert.
func Fetch(_ *Request, url string, header map[string]string) (*Reply, error) {
	if !strings.HasPrefix(url, "http://") {
		return nil, errors.New("stulphttp: only http:// on a node -- an app on a slot serves on the internal network")
	}
	call := leanhttp.Call{Method: MethodGet, URL: url}
	if len(header) > 0 {
		call.Header = make(leanhttp.Header, len(header))
		for k, v := range header {
			call.Header.Set(k, v)
		}
	}
	answer, err := streamClient.Do(call)
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

// Context is de levensduur van het verzoek — en op een node is dat met opzet
// NIET de verbinding.
//
// leanhttp geeft die levensduur via Request.Done, en dat zet een wachthond op de
// verbinding: een goroutine die meeleest om te merken dat de client weggaat.
// Zo'n verbinding kan daarna niet meer hergebruikt worden. GEMETEN 12-08 op
// ijzer, in hop: door bij élk verzoek de levensduur op te vragen viel keep-alive
// voor élk verzoek weg, en omdat de antwoordkop al "keep-alive" had gezegd legde
// de client een dode verbinding in zijn pool — 200, 502, 200, 502.
//
// Een store-aanroep hoeft die wachthond niet: die duurt microseconden. Wie wél
// wil weten dat de client weg is, vraagt Done — en betaalt daar dan bewust die
// verbinding voor.
func Context(*Request) context.Context { return context.Background() }

// Done sluit als de client weg is. Dit is de plek die de wachthond aanzet.
func Done(r *Request) <-chan struct{} { return r.Done() }

// CloseBody doet hier niets: leanhttp's request-body is een lezer op de
// verbinding, en die is van de server. Sluiten zou de verbinding zelf raken, en
// die moet juist blijven staan voor het volgende verzoek.
func CloseBody(*Request) {}
