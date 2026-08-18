package appsdk

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Loggen vanuit een plugin.
//
// Stulp leest de uitvoer van het proces en zet elke regel in zijn eigen log. Om
// te weten hoe zwaar een regel is verwacht hij hem als "niveau<TAB>bericht" --
// anders is alles even hard, en dan valt een waarschuwing niet meer op tussen
// de rest.
//
// Een plugin die zijn eigen slog naar stderr laat schrijven levert dat niet:
// die regel draagt zijn eigen tijd en niveau, en komt bij Stulp binnen als één
// lange INFO-regel met de echte melding erin geplakt. Vandaar deze handler.

// Logger levert een slog.Logger waarvan de regels in Stulps log terechtkomen
// met het niveau dat de plugin bedoelde.
//
// Tijd staat er niet in: die zet Stulp erbij, en twee tijdstempels op één regel
// zijn er één te veel.
func (h *Stulp) Logger() *slog.Logger { return slog.New(&stulpHandler{host: h.host}) }

type stulpHandler struct {
	host   *Host
	attrs  []slog.Attr
	groups []string
}

func (s *stulpHandler) Enabled(context.Context, slog.Level) bool { return true }

func (s *stulpHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Message)
	for _, attr := range s.attrs {
		writeAttr(&line, s.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(&line, s.groups, attr)
		return true
	})
	s.host.Log(levelName(record.Level), line.String())
	return nil
}

func (s *stulpHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	combined := make([]slog.Attr, 0, len(s.attrs)+len(attrs))
	combined = append(combined, s.attrs...)
	combined = append(combined, attrs...)
	return &stulpHandler{host: s.host, attrs: combined, groups: s.groups}
}

func (s *stulpHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}
	groups := make([]string, 0, len(s.groups)+1)
	groups = append(groups, s.groups...)
	groups = append(groups, name)
	return &stulpHandler{host: s.host, attrs: s.attrs, groups: groups}
}

func writeAttr(line *strings.Builder, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, nested := range attr.Value.Group() {
			writeAttr(line, append(groups, attr.Key), nested)
		}
		return
	}
	line.WriteByte(' ')
	for _, group := range groups {
		line.WriteString(group)
		line.WriteByte('.')
	}
	line.WriteString(attr.Key)
	line.WriteByte('=')
	// Aanhalingstekens alleen waar ze nodig zijn: een foutmelding met spaties
	// wordt anders onleesbaar zodra hij tussen andere velden staat.
	text := attr.Value.String()
	if strings.ContainsAny(text, " \t\"") {
		line.WriteString(fmt.Sprintf("%q", text))
		return
	}
	line.WriteString(text)
}

// levelName houdt zich aan de vier woorden die Stulp herkent. Een eigen niveau
// -- slog staat elk getal toe -- valt terug op het dichtstbijzijnde, want een
// onbekend woord zou Stulp de hele regel als bericht laten lezen.
func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}
