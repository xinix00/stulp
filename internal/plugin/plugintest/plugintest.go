// Package plugintest bouwt de testplugin voor tests die een echte app nodig
// hebben.
//
// Een app is een binary, dus een test die er een start moet er ook een hebben.
// Eén keer bouwen per pakket en daarna kopiëren scheelt een go build per test.
package plugintest

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	once   sync.Once
	binary string
	err    error
)

// Install zet de testplugin klaar als de app-binary in root.
//
// De naam is het app-id: dat is waar Stulp hem zoekt.
func Install(t testing.TB, root, appID string) {
	t.Helper()
	once.Do(build)
	if err != nil {
		t.Fatalf("build test plugin: %v", err)
	}
	data, readErr := os.ReadFile(binary)
	if readErr != nil {
		t.Fatalf("read test plugin: %v", readErr)
	}
	if writeErr := os.WriteFile(filepath.Join(root, appID), data, 0o755); writeErr != nil {
		t.Fatalf("install test plugin: %v", writeErr)
	}
}

// Bytes levert de testplugin, voor een test die hem in een archief moet stoppen.
func Bytes(t testing.TB) []byte {
	t.Helper()
	once.Do(build)
	if err != nil {
		t.Fatalf("build test plugin: %v", err)
	}
	data, readErr := os.ReadFile(binary)
	if readErr != nil {
		t.Fatalf("read test plugin: %v", readErr)
	}
	return data
}

func build() {
	dir, mkErr := os.MkdirTemp("", "stulp-testplugin-")
	if mkErr != nil {
		err = mkErr
		return
	}
	binary = filepath.Join(dir, "testplugin")
	command := exec.Command("go", "build", "-o", binary, "github.com/xinix00/stulp/internal/plugin/testplugin")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		err = &buildError{output: string(output), cause: buildErr}
	}
}

type buildError struct {
	output string
	cause  error
}

func (e *buildError) Error() string { return e.cause.Error() + "\n" + e.output }

// Example zet de voorbeeldplugin klaar in een eigen map en levert zijn root.
//
// Kopiëren in plaats van in de bron bouwen: een test hoort geen binary in de
// repo achter te laten, en twee tests naast elkaar horen elkaars app-map niet
// te delen.
func Example(t testing.TB, source string) string {
	t.Helper()
	root := t.TempDir()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatalf("copy example plugin: %v", err)
	}

	binary := filepath.Join(root, "com.stulp.virtual")
	command := exec.Command("go", "build", "-o", binary, "github.com/xinix00/stulp/examples/virtual")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		t.Fatalf("build example plugin: %v\n%s", buildErr, output)
	}
	// Het uitvoerbit expliciet zetten. Wie ooit in de bronmap bouwt laat daar een
	// binary achter; die wordt hierboven meegekopieerd als gewoon bestand, en dan
	// neemt go build de rechten van dat bestaande bestand over. Het gevolg is een
	// app die niet start met "permission denied", ver van de oorzaak.
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatalf("build example plugin: %v", err)
	}
	return root
}
