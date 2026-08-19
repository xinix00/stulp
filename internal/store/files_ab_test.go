package store

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

const abTestDocument = "/volume/stulp.json"

func TestABFileStoreChoosesHighestValidGeneration(t *testing.T) {
	backend := newABTestFiles()
	backend.putSlot(t, abTestDocument+slotSuffixA, 7, "older")
	backend.putSlot(t, abTestDocument+slotSuffixB, 9, "newer")
	files := NewABFileStore(backend, abTestDocument)

	assertABRead(t, files, "newer")
}

func TestABFileStoreIgnoresEmptyAndCorruptSlots(t *testing.T) {
	tests := []struct {
		name   string
		breakA func(*testing.T, *abTestFiles)
	}{
		{
			name: "empty",
			breakA: func(_ *testing.T, backend *abTestFiles) {
				backend.files[abTestDocument+slotSuffixA] = []byte{}
			},
		},
		{
			name: "bad checksum",
			breakA: func(t *testing.T, backend *abTestFiles) {
				backend.putSlot(t, abTestDocument+slotSuffixA, 8, "apparently newer")
				backend.files[abTestDocument+slotSuffixA][slotHeaderSize] ^= 0xff
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newABTestFiles()
			test.breakA(t, backend)
			backend.putSlot(t, abTestDocument+slotSuffixB, 4, "intact")
			assertABRead(t, NewABFileStore(backend, abTestDocument), "intact")
		})
	}
}

func TestABFileStoreRejectsOnlyEmptyOrCorruptSlots(t *testing.T) {
	backend := newABTestFiles()
	backend.files[abTestDocument+slotSuffixA] = []byte{}
	backend.files[abTestDocument+slotSuffixB] = []byte("not a slot")
	files := NewABFileStore(backend, abTestDocument)

	_, err := files.ReadFile(abTestDocument)
	if err == nil {
		t.Fatal("ReadFile succeeded with no valid document copy")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt slots were mistaken for a first run: %v", err)
	}
}

func TestABFileStoreSurvivesTornWrite(t *testing.T) {
	backend := newABTestFiles()
	files := NewABFileStore(backend, abTestDocument)
	if err := files.WriteFile(abTestDocument, []byte("known good")); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}

	backend.write = func(path string, data []byte) error {
		backend.files[path] = append([]byte(nil), data[:len(data)/2]...)
		return errors.New("simulated power loss")
	}
	if err := files.WriteFile(abTestDocument, []byte("unfinished replacement")); err == nil {
		t.Fatal("torn WriteFile succeeded")
	}
	backend.write = nil

	assertABRead(t, files, "known good")
}

func TestABFileStoreResolvesLostReplyAfterCompleteWrite(t *testing.T) {
	backend := newABTestFiles()
	files := NewABFileStore(backend, abTestDocument)
	backend.write = func(path string, data []byte) error {
		backend.files[path] = append([]byte(nil), data...)
		return errors.New("simulated lost RPC reply")
	}

	if err := files.WriteFile(abTestDocument, []byte("complete replacement")); err != nil {
		t.Fatalf("complete verified write remained indeterminate: %v", err)
	}
	backend.write = nil
	assertABRead(t, files, "complete replacement")
}

func TestABFileStoreFallsBackAndMigratesWithoutDeletingLegacy(t *testing.T) {
	backend := newABTestFiles()
	backend.files[abTestDocument] = []byte("legacy")
	files := NewABFileStore(backend, abTestDocument)

	assertABRead(t, files, "legacy")
	if err := files.WriteFile(abTestDocument, []byte("slotted")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertABRead(t, files, "slotted")
	if got := string(backend.files[abTestDocument]); got != "legacy" {
		t.Fatalf("legacy document changed during migration: %q", got)
	}
}

func TestABFileStoreAlternatesSlots(t *testing.T) {
	backend := newABTestFiles()
	files := NewABFileStore(backend, abTestDocument)
	for _, value := range []string{"one", "two", "three"} {
		if err := files.WriteFile(abTestDocument, []byte(value)); err != nil {
			t.Fatalf("WriteFile(%q): %v", value, err)
		}
	}

	genA, payloadA, validA := decodeDocumentSlot(backend.files[abTestDocument+slotSuffixA])
	genB, payloadB, validB := decodeDocumentSlot(backend.files[abTestDocument+slotSuffixB])
	if !validA || genA != 3 || string(payloadA) != "three" {
		t.Fatalf("slot A = valid %t, generation %d, payload %q", validA, genA, payloadA)
	}
	if !validB || genB != 2 || string(payloadB) != "two" {
		t.Fatalf("slot B = valid %t, generation %d, payload %q", validB, genB, payloadB)
	}
}

func TestABFileStoreMatchesStoreAbsolutePath(t *testing.T) {
	backend := newABTestFiles()
	files := NewABFileStore(backend, "stulp.json")
	absolute, err := filepath.Abs("stulp.json")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	if err := files.WriteFile(absolute, []byte("document")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, ok := backend.files[absolute+slotSuffixA]; !ok {
		t.Fatalf("relative configured path did not protect absolute Store path %q", absolute)
	}
}

func assertABRead(t *testing.T, files FileStore, want string) {
	t.Helper()
	got, err := files.ReadFile(abTestDocument)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != want {
		t.Fatalf("ReadFile = %q, want %q", got, want)
	}
}

type abTestFiles struct {
	files map[string][]byte
	write func(path string, data []byte) error
}

func newABTestFiles() *abTestFiles {
	return &abTestFiles{files: make(map[string][]byte)}
}

func (f *abTestFiles) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (f *abTestFiles) WriteFile(path string, data []byte) error {
	if f.write != nil {
		return f.write(path, data)
	}
	f.files[path] = append([]byte(nil), data...)
	return nil
}

func (f *abTestFiles) putSlot(t *testing.T, path string, generation uint64, payload string) {
	t.Helper()
	record, err := encodeDocumentSlot(generation, []byte(payload))
	if err != nil {
		t.Fatalf("encodeDocumentSlot: %v", err)
	}
	f.files[path] = record
}
