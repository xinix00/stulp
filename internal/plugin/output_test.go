package plugin

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/xinix00/stulp/internal/store"
)

// Wat een app uitspuugt hoort in de log van Stulp te komen, met zijn naam erbij.
//
// De SDK schrijft "niveau\tbericht"; alles wat die vorm niet heeft komt van de
// app zelf of van een bibliotheek eronder, en dat is net zo goed het lezen waard
// -- een panic met stack trace komt zo binnen.
func TestAppOutputReachesTheLogWithItsLevel(t *testing.T) {
	var written bytes.Buffer
	process := &Process{
		app:     store.App{ID: "com.demo"},
		options: Options{Logger: slog.New(slog.NewTextHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug}))},
	}

	process.readOutput(readCloser{strings.NewReader(
		"info\tapp is er\n" +
			"error\tverbinding weg\n" +
			"panic: runtime error: index out of range\n" +
			"\n" +
			"goroutine 1 [running]:\n")})

	log := written.String()
	for _, want := range []string{
		`level=INFO msg="app is er" app=com.demo`,
		`level=ERROR msg="verbinding weg" app=com.demo`,
		`msg="panic: runtime error: index out of range"`,
		`msg="goroutine 1 [running]:"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log mist %q\n--- log ---\n%s", want, log)
		}
	}
	// Een lege regel is geen bericht.
	if strings.Count(log, "msg=") != 4 {
		t.Errorf("verwacht 4 regels, kreeg:\n%s", log)
	}
}

// Een laatste regel zonder afsluitende newline mag niet verdwijnen: dat is
// precies de regel waarmee een proces zegt waaraan het overleed.
func TestLastLineWithoutNewlineIsNotLost(t *testing.T) {
	var written bytes.Buffer
	process := &Process{
		app:     store.App{ID: "com.demo"},
		options: Options{Logger: slog.New(slog.NewTextHandler(&written, nil))},
	}
	process.readOutput(readCloser{strings.NewReader("error\tlaatste woorden")})
	if !strings.Contains(written.String(), `msg="laatste woorden"`) {
		t.Errorf("de laatste regel is verdwenen: %s", written.String())
	}
}

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }
