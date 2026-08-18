package appsdk

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Een waarschuwing van een plugin hoort bij Stulp als waarschuwing binnen te
// komen. Dat lukte niet toen de Matter-plugin zijn slog rechtstreeks naar
// stderr liet schrijven: die regel droeg zijn eigen tijd en niveau, en Stulp --
// die "niveau<TAB>bericht" verwacht -- las het geheel als één INFO-regel met de
// echte melding erin geplakt.
func TestLoggerWritesTheLevelStulpReads(t *testing.T) {
	var written strings.Builder
	logger := slog.New(&stulpHandler{host: &Host{out: &written}})

	logger.Warn("Matter subscription stopped; retrying",
		"node", "000000000001000E",
		"error", errors.New("exceeded its maximum reporting interval"))

	line := strings.TrimRight(written.String(), "\n")
	level, message, split := strings.Cut(line, "\t")
	if !split {
		t.Fatalf("geen tab tussen niveau en bericht: %q", line)
	}
	if level != "warn" {
		t.Fatalf("niveau = %q, wil warn", level)
	}
	want := `Matter subscription stopped; retrying node=000000000001000E error="exceeded its maximum reporting interval"`
	if message != want {
		t.Fatalf("bericht =\n  %q\nwil\n  %q", message, want)
	}
	// Eén regel: een melding met een nieuwe regel erin zou bij Stulp als twee
	// losse regels aankomen, en de tweede zonder niveau.
	if strings.Count(written.String(), "\n") != 1 {
		t.Fatalf("meer dan één regel: %q", written.String())
	}
}

func TestLoggerKeepsAttrsAndGroups(t *testing.T) {
	var written strings.Builder
	logger := slog.New(&stulpHandler{host: &Host{out: &written}}).
		With("app", "com.stulp.matter").WithGroup("node")

	logger.Error("geen antwoord", "id", 7)

	line := strings.TrimRight(written.String(), "\n")
	if !strings.HasPrefix(line, "error\t") {
		t.Fatalf("niveau ontbreekt: %q", line)
	}
	for _, want := range []string{"app=com.stulp.matter", "node.id=7"} {
		if !strings.Contains(line, want) {
			t.Fatalf("regel mist %q: %q", want, line)
		}
	}
}

// Elk slog-niveau moet op een woord uitkomen dat Stulp herkent; een onbekend
// woord zou hij als deel van het bericht lezen.
func TestEveryLevelMapsToAWordStulpKnows(t *testing.T) {
	known := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	for _, level := range []slog.Level{
		slog.LevelDebug - 4, slog.LevelDebug, slog.LevelInfo, slog.LevelInfo + 1,
		slog.LevelWarn, slog.LevelError, slog.LevelError + 8,
	} {
		if name := levelName(level); !known[name] {
			t.Fatalf("niveau %v werd %q", level, name)
		}
	}
}
