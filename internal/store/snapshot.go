package store

// snapshot.go — the safe boundary between a backup archive and the live store.
//
// Backup owns ZIP files and app bundles; Store owns the shape of stulp.json and
// the lock that protects the live copy. Keeping that split means a restore can
// validate an entire document before it takes the write lock, then replace the
// persisted and in-memory copies as one operation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Snapshot is a parsed, validated Stulp document. Its internals deliberately
// stay private: backup may inspect app identities and relocate bundle roots,
// but it cannot accidentally mutate the live store.
type Snapshot struct {
	doc *document
}

// ParseSnapshot validates the document stored in a backup without publishing
// any of it.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	if len(data) == 0 {
		return nil, errors.New("backup contains an empty stulp.json")
	}
	loaded, err := decodeDocument("backup stulp.json", data)
	if err != nil {
		return nil, err
	}
	return &Snapshot{doc: loaded}, nil
}

// Apps returns just enough of the snapshot's app records for archive
// validation. A bundled app is validated against app.json after extraction; a
// rootless app carries no manifest anywhere but in its own announcements, so
// the snapshot has only its identity — which is all a backup should hold.
func (s *Snapshot) Apps() []App {
	if s == nil || s.doc == nil {
		return nil
	}
	apps := make([]App, 0, len(s.doc.Apps))
	for _, record := range s.doc.Apps {
		apps = append(apps, App{
			ID: record.ID, Root: record.Root,
			Enabled: record.Enabled,
			Offered: record.Offered, Source: record.Source,
			UpdateVersion: record.UpdateVersion, UpdateCheckedAt: record.UpdateCheckedAt,
		})
	}
	return apps
}

// SetAppRoot relocates one bundled app before the snapshot is installed.
func (s *Snapshot) SetAppRoot(appID, root string) error {
	if s == nil || s.doc == nil {
		return errors.New("nil store snapshot")
	}
	for index := range s.doc.Apps {
		if s.doc.Apps[index].ID == appID {
			s.doc.Apps[index].Root = root
			return nil
		}
	}
	return fmt.Errorf("snapshot has no app %q", appID)
}

// SnapshotBytes returns a consistent document without involving a temporary
// filesystem. That matters on HopOS, where the document lives behind a volume
// RPC and there is no process-local disk.
func (s *Store) SnapshotBytes(ctx context.Context) ([]byte, error) {
	if s == nil || s.path == InMemoryPath {
		return nil, errors.New("snapshot needs a file-backed store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	encoded, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", s.path, err)
	}
	return append(encoded, '\n'), nil
}

// RestoreSnapshot persists and publishes a prepared document while holding the
// same lock as every ordinary mutation. The old document is retained beside it
// so a restore can itself be undone. App bundles are handled by backup before
// this call; roots in snapshot already point at their final location.
func (s *Store) RestoreSnapshot(ctx context.Context, snapshot *Snapshot) (string, error) {
	if s == nil || s.path == InMemoryPath {
		return "", errors.New("restore needs a file-backed store")
	}
	if snapshot == nil || snapshot.doc == nil {
		return "", errors.New("restore needs a validated snapshot")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	previous := s.path + ".pre-restore-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	s.mu.Lock()
	if err := saveDocument(previous, s.doc); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("preserve current document: %w", err)
	}
	restored := snapshot.doc
	if err := saveDocument(s.path, restored); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("publish restored document: %w", err)
	}
	// The swap shares the document lock with every mutation, so a mutation
	// lands wholly before or wholly after it. A flow edit that was computed
	// from the old document is caught by UpdateFlowIfUnchanged; work already
	// running against the old document (a Flow run, a plugin call) finishes
	// against the restored one, which is why a restore is followed by a reload.
	oldDocument := s.doc
	s.doc = restored
	// Consumed: callers cannot retain Snapshot and mutate roots behind the
	// store's lock after it has become the live document.
	snapshot.doc = nil
	s.state = make(map[string]map[string]any)
	deletedSceneDevices := make(map[string]struct{}, len(s.deletedSceneDevices)+len(oldDocument.Scenes))
	for deviceID := range s.deletedSceneDevices {
		deletedSceneDevices[deviceID] = struct{}{}
	}
	for _, scene := range oldDocument.Scenes {
		deletedSceneDevices[SceneDeviceID(scene.ID)] = struct{}{}
	}
	for _, scene := range restored.Scenes {
		delete(deletedSceneDevices, SceneDeviceID(scene.ID))
	}
	for _, record := range restored.Devices {
		if record.AppID != NativeSceneAppID {
			delete(deletedSceneDevices, record.ID)
		}
	}
	s.deletedSceneDevices = deletedSceneDevices
	s.seedSceneDeviceStates()
	// Manifesten horen niet bij het document: een gebundelde app heeft het op
	// schijf, een slot-app herhaalt het bij elke aanmelding. Ze hier weggooien
	// laat juist die slot-app -- die al verbonden is en zich dus niet opnieuw
	// meldt -- met een stub achter: geen instellingsvelden, geen drivers, geen
	// eigen koppelpagina's. Wat weg is uit het teruggezette document gaat eruit,
	// de rest wordt herlezen waar dat kan.
	installed := make(map[string]bool, len(s.doc.Apps))
	for _, app := range s.doc.Apps {
		installed[app.ID] = true
	}
	for id := range s.manifests {
		if !installed[id] {
			delete(s.manifests, id)
		}
	}
	s.reloadManifests()
	s.mu.Unlock()

	s.publish(Event{Manager: "store", Type: "store.reload"})
	return previous, nil
}
