package store

// files_ab.go — crash-safe document replacement for storage without rename.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"path/filepath"
	"sync"
)

const (
	slotSuffixA      = ".a"
	slotSuffixB      = ".b"
	slotHeaderSize   = 24
	slotChecksumAt   = 20
	slotFormatMarker = 1
)

var slotMagic = [8]byte{'S', 'T', 'U', 'L', 'P', 'A', 'B', slotFormatMarker}

// NewABFileStore protects documentPath with two generation-numbered slots.
// The backend may truncate and write a slot in chunks: until the new slot is
// complete and has a valid checksum, the other slot remains the newest valid
// copy. The unsuffixed path is read only when neither slot is valid, so an old
// installation migrates on its first subsequent write without deleting its
// legacy document.
//
// Other paths pass through unchanged. This matters for standalone snapshots,
// which are documents in their own right rather than copies of the live store.
func NewABFileStore(backend FileStore, documentPath string) FileStore {
	absolute := documentPath
	if resolved, err := filepath.Abs(documentPath); err == nil {
		absolute = resolved
	}
	return &abFileStore{backend: backend, path: documentPath, absolutePath: absolute}
}

type abFileStore struct {
	backend      FileStore
	path         string
	absolutePath string
	mu           sync.Mutex
	loaded       bool
	active       string
	generation   uint64
}

type documentSlot struct {
	generation uint64
	payload    []byte
	present    bool
	valid      bool
}

func (f *abFileStore) ReadFile(path string) ([]byte, error) {
	if !f.isDocument(path) {
		return f.backend.ReadFile(path)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	a, err := f.readSlot(path + slotSuffixA)
	if err != nil {
		return nil, err
	}
	b, err := f.readSlot(path + slotSuffixB)
	if err != nil {
		return nil, err
	}

	switch {
	case a.valid && b.valid && a.generation == b.generation:
		if !bytes.Equal(a.payload, b.payload) {
			return nil, fmt.Errorf("read %s: document slots have conflicting generation %d", path, a.generation)
		}
		f.remember(slotSuffixA, a.generation)
		return a.payload, nil
	case a.valid && (!b.valid || a.generation > b.generation):
		f.remember(slotSuffixA, a.generation)
		return a.payload, nil
	case b.valid:
		f.remember(slotSuffixB, b.generation)
		return b.payload, nil
	}

	legacy, legacyErr := f.backend.ReadFile(path)
	if legacyErr == nil {
		f.remember("", 0)
		return legacy, nil
	}
	if !errors.Is(legacyErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("read legacy document %s: %w", path, legacyErr)
	}
	if a.present || b.present {
		return nil, fmt.Errorf("read %s: no valid document slot and no legacy document", path)
	}
	f.remember("", 0)
	return nil, legacyErr
}

func (f *abFileStore) WriteFile(path string, data []byte) error {
	if !f.isDocument(path) {
		return f.backend.WriteFile(path, data)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.loaded {
		if err := f.discoverGeneration(path); err != nil {
			return err
		}
	}
	if f.generation == ^uint64(0) {
		return fmt.Errorf("write %s: document slot generation exhausted", path)
	}
	targetSuffix := slotSuffixA
	if f.active == slotSuffixA {
		targetSuffix = slotSuffixB
	}
	target := path + targetSuffix
	nextGeneration := f.generation + 1

	record, err := encodeDocumentSlot(nextGeneration, data)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.backend.WriteFile(target, record); err != nil {
		// A chunked RPC can lose its reply after the complete record reached the
		// volume. Resolve that indeterminate outcome by reading the target back:
		// returning an error while leaving a valid newer generation behind would
		// keep old RAM now but unexpectedly boot the new document later.
		written, readErr := f.readSlot(target)
		if readErr == nil && written.valid && written.generation == nextGeneration && bytes.Equal(written.payload, data) {
			f.remember(targetSuffix, nextGeneration)
			return nil
		}
		return fmt.Errorf("write document slot %s: %w", target, err)
	}
	f.remember(targetSuffix, nextGeneration)
	return nil
}

func (f *abFileStore) isDocument(path string) bool {
	return path == f.path || path == f.absolutePath
}

func (f *abFileStore) readSlot(path string) (documentSlot, error) {
	raw, err := f.backend.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return documentSlot{}, nil
	}
	if err != nil {
		return documentSlot{}, fmt.Errorf("read document slot %s: %w", path, err)
	}
	generation, payload, valid := decodeDocumentSlot(raw)
	return documentSlot{generation: generation, payload: payload, present: true, valid: valid}, nil
}

// discoverGeneration is only needed when a caller writes before reading. The
// live Store always reads during Open, after which saves need no extra volume
// reads and allocate only the new slot record.
func (f *abFileStore) discoverGeneration(path string) error {
	a, err := f.readSlot(path + slotSuffixA)
	if err != nil {
		return err
	}
	a.payload = nil
	b, err := f.readSlot(path + slotSuffixB)
	if err != nil {
		return err
	}
	b.payload = nil

	switch {
	case a.valid && b.valid && a.generation == b.generation:
		return fmt.Errorf("write %s: document slots have duplicate generation %d", path, a.generation)
	case a.valid && (!b.valid || a.generation > b.generation):
		f.remember(slotSuffixA, a.generation)
	case b.valid:
		f.remember(slotSuffixB, b.generation)
	default:
		f.remember("", 0)
	}
	return nil
}

func (f *abFileStore) remember(active string, generation uint64) {
	f.loaded = true
	f.active = active
	f.generation = generation
}

func encodeDocumentSlot(generation uint64, payload []byte) ([]byte, error) {
	if generation == 0 {
		return nil, errors.New("document slot generation must be positive")
	}
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return nil, errors.New("document is too large for a slot")
	}

	record := make([]byte, slotHeaderSize+len(payload))
	copy(record, slotMagic[:])
	binary.LittleEndian.PutUint64(record[8:16], generation)
	binary.LittleEndian.PutUint32(record[16:20], uint32(len(payload)))
	copy(record[slotHeaderSize:], payload)
	binary.LittleEndian.PutUint32(record[slotChecksumAt:slotHeaderSize], documentSlotChecksum(record))
	return record, nil
}

func decodeDocumentSlot(record []byte) (uint64, []byte, bool) {
	if len(record) < slotHeaderSize || !bytes.Equal(record[:len(slotMagic)], slotMagic[:]) {
		return 0, nil, false
	}
	generation := binary.LittleEndian.Uint64(record[8:16])
	length := binary.LittleEndian.Uint32(record[16:20])
	if generation == 0 || uint64(length) != uint64(len(record)-slotHeaderSize) {
		return 0, nil, false
	}
	want := binary.LittleEndian.Uint32(record[slotChecksumAt:slotHeaderSize])
	if want != documentSlotChecksum(record) {
		return 0, nil, false
	}
	return generation, record[slotHeaderSize:], true
}

func documentSlotChecksum(record []byte) uint32 {
	checksum := crc32.Update(0, crc32.IEEETable, record[:slotChecksumAt])
	return crc32.Update(checksum, crc32.IEEETable, record[slotHeaderSize:])
}
