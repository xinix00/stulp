package webapi

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/stulphttp"
	"github.com/xinix00/stulp/internal/supervisor"
)

// TestRoutetabelPastOpDeNodeMux — de node draait dezelfde routetabel op
// leanhttp's mux, en die is strenger dan net/http: geen {$}, en kruisende
// dekkingen (pad-subset zonder methode-subset, of andersom) panicken bij
// registratie. De host-suite zag daar niets van, dus knalde het twee keer op
// rij pas als taskpanic op het ijzer (14-08: eerst {$}, toen "GET /" tegen
// een method-loze route). Deze wacht legt de VOLLEDIGE tabel hier, lokaal,
// tegen de node-mux.
func TestRoutetabelPastOpDeNodeMux(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "stulp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	apps := supervisor.New(database, plugin.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer apps.Close()
	server := New(database, apps, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer server.Close()

	nodeMux := leanhttp.NewServeMux()
	for _, pattern := range server.mux.Patterns() {
		p := pattern
		if p == stulphttp.RootPattern {
			// De ene bewuste gedaante-divergentie: de node-wortel is
			// method-loos "/" (zie shape_node.RootPattern en zijn uitleg).
			p = "/"
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("patroon %q past niet op de node-mux: %v", pattern, r)
				}
			}()
			nodeMux.HandleFunc(p, func(leanhttp.ResponseWriter, *leanhttp.Request) {})
		}()
	}
}
