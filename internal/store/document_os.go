//go:build !tamago

package store

// document_os.go — the document on a filesystem.

import (
	"fmt"
	"os"
	"path/filepath"
)

func init() { files = osFiles{} }

// osFiles writes so that a crash can cost the last change but never the file.
//
// Losing the most recent write is indeed no worse than that write never having
// happened. Losing the file is a different matter entirely: writing in place
// truncates first, so an interruption halfway leaves a half-document and takes
// every app, device and Flow with it. Hence a complete temporary file, flushed
// to the platter, and then an atomic rename over the original.
type osFiles struct{}

func (osFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osFiles) WriteFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".stulp-*.json")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Without the sync the rename can land before the bytes do, which is the
	// same lost file with extra steps.
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Renames are metadata; the directory entry needs flushing too.
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
