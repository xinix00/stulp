// Package store keeps everything Stulp must remember in one JSON document.
//
// The data is small and the questions asked of it are simple: give me all of
// something, or give me one by its identifier. There are no joins, no
// aggregates and deliberately no history, so a database engine would be a large
// dependency bought for nothing. The whole document is held in memory and
// written back atomically whenever it changes.
//
// Two things are kept out of it on purpose. An app's manifest lives in the
// app's own bundle and is read from there, so a second copy cannot drift when
// the app is updated. A device's capability values are what the house is doing
// at this instant rather than how it is configured, so they live in memory
// only; persisting them would rewrite this file every time a sensor reports a
// temperature.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/manifest"
)

// InMemoryPath asks for a store that never touches disk, which tests use.
const InMemoryPath = ":memory:"

type Store struct {
	path string

	mu  sync.RWMutex
	doc *document
	// state holds capability values. It is rebuilt from the running apps and
	// from Matter subscriptions after every restart, and is never written.
	state map[string]map[string]any
	// manifests are read from each app's bundle at open and on install.
	manifests map[string]map[string]any

	eventsMu       sync.RWMutex
	subscribers    map[uint64]chan Event
	nextSubscriber uint64
}

// Event is the internal seam shared by the plugin-compatible API and Stulp's
// UI. Manager and Type deliberately follow the SDK's manager/object vocabulary.
type Event struct {
	Manager string    `json:"manager"`
	Type    string    `json:"type"`
	ID      string    `json:"id,omitempty"`
	Data    any       `json:"data,omitempty"`
	At      time.Time `json:"at"`
}

type App struct {
	ID       string
	Version  string
	Root     string
	Manifest map[string]any
	Enabled  bool

	// Offered: deze app meldde zich aan en wacht op acceptatie. Hij draait niet.
	Offered bool

	// Source is the GitHub link an app was installed from, empty for an app
	// installed from a local directory. Remembering it is what makes a later
	// update check possible without asking for the link again.
	Source string
	// UpdateVersion and UpdateCheckedAt hold the answer to the last manual
	// check, so the Apps page still knows about a waiting update after a
	// reload. Stulp never checks on its own.
	UpdateVersion   string
	UpdateCheckedAt string
}

type Device struct {
	ID           string         `json:"id"`
	AppID        string         `json:"appId"`
	DriverID     string         `json:"driverId"`
	GroupID      string         `json:"groupId,omitempty"`
	SortOrder    int            `json:"sortOrder,omitempty"`
	Name         string         `json:"name"`
	Class        string         `json:"class"`
	Data         map[string]any `json:"data"`
	Settings     map[string]any `json:"settings"`
	Store        map[string]any `json:"store"`
	Capabilities []string       `json:"capabilities"`
	State        map[string]any `json:"state"`
	Available    bool           `json:"available"`
	Message      string         `json:"unavailableMessage,omitempty"`
}

const deviceHardwareNameKey = "__stulp.hardwareName"

// HardwareName is the stable name under which an integration first presented
// the product. Name is the user-owned alias and may change freely.
func (d Device) HardwareName() string {
	if value, _ := d.Store[deviceHardwareNameKey].(string); strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(d.Name)
}

// PreserveHardwareName captures the current name once and never overwrites it.
func (d *Device) PreserveHardwareName() {
	if d.Store == nil {
		d.Store = make(map[string]any)
	}
	if value, _ := d.Store[deviceHardwareNameKey].(string); strings.TrimSpace(value) != "" {
		return
	}
	if name := strings.TrimSpace(d.Name); name != "" {
		d.Store[deviceHardwareNameKey] = name
	}
}

func Open(path string) (*Store, error) {
	if path != InMemoryPath {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve store path: %w", err)
		}
		if directory, evalErr := filepath.EvalSymlinks(filepath.Dir(absolute)); evalErr == nil {
			absolute = filepath.Join(directory, filepath.Base(absolute))
		}
		path = absolute
	}
	s := &Store{
		path: path, state: make(map[string]map[string]any),
		manifests: make(map[string]map[string]any), subscribers: make(map[uint64]chan Event),
	}
	loaded := &document{Version: documentVersion}
	if path != InMemoryPath {
		var err error
		if loaded, err = loadDocument(path); err != nil {
			return nil, err
		}
	}
	s.doc = loaded
	if s.doc.Settings == nil {
		s.doc.Settings = make(map[string]map[string]any)
	}
	s.reloadManifests()
	return s, nil
}

// Close exists because callers defer it. Every change is already on disk by the
// time its method returns, so there is nothing left to flush.
func (s *Store) Close() error { return nil }

func (s *Store) Path() string { return s.path }

// Snapshot writes a standalone copy of the document. It is a plain file rather
// than a database dump, so a backup can be read without Stulp.
func (s *Store) Snapshot(_ context.Context, destination string) error {
	if s.path == InMemoryPath {
		return errors.New("snapshot needs a file-backed store")
	}
	if destination == "" {
		return errors.New("snapshot destination is required")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("snapshot destination %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return saveDocument(destination, s.doc)
}

// AppsRoot is the persistent directory used for apps installed from archives.
// Keeping it beside the document makes a Stulp instance self-contained.
func (s *Store) AppsRoot() (string, error) {
	if s.path == InMemoryPath {
		return "", errors.New("archive installation needs a file-backed store")
	}
	return s.path + ".apps", nil
}

// saveLocked persists the document. Callers hold the write lock.
func (s *Store) saveLocked() error {
	if s.path == InMemoryPath {
		return nil
	}
	return saveDocument(s.path, s.doc)
}

// ensureMatterApp gives the native controller an app entry so its devices and
// Flow cards behave like any other app's. Its manifest is synthesised: there is
// no bundle to read one from.

// reloadManifests restores the manifest cache from the source that owns it.
// Bundled apps are read from app.json. An app that announced itself carries
// its manifest ITSELF and repeats it at every attach — the document does not
// store application knowledge — so it opens as a placeholder and fills in at
// its first announce, seconds later.
//
// An app whose bundle is unreadable keeps its entry with a placeholder, so the
// failure surfaces where it belongs — the app will not start and Manage says
// why — instead of the whole store refusing to open.
func (s *Store) reloadManifests() {
	for _, app := range s.doc.Apps {
		s.loadManifest(app)
	}
}

func (s *Store) loadManifest(app appRecord) {
	if app.Root == "" {
		if s.manifests[app.ID] == nil {
			s.manifests[app.ID] = map[string]any{"id": app.ID}
		}
		return
	}
	loaded, _, err := manifest.Load(app.Root)
	if err != nil {
		// Onleesbaar is GEEN reden om te vergeten wat we al wisten. Een
		// teruggezet document draagt de roots van de machine waar de backup
		// gemaakt is (gemeten 19-08: alle zeven wijzen naar /Users/derek/hopy/
		// plugins/...), dus op een node mislukt élke lezing. Dat overschreef hier
		// het manifest dat de app zélf had aangemeld met een romp: geen
		// instellingsvelden, geen drivers, geen eigen koppelpagina's — terwijl de
		// apparaten bleven werken, want de plugin draaide al. De romp is er voor
		// een app waarvan we NIETS hebben, niet als straf voor een fout pad.
		if s.manifests[app.ID] == nil {
			s.manifests[app.ID] = map[string]any{"id": app.ID}
		}
		return
	}
	s.manifests[app.ID] = loaded.Raw
}

// Subscribe returns a stream of mutations. If a consumer falls so far behind
// that its queue fills, the queue is emptied and one store.reload event takes
// its place: the missed values cannot be derived, so the only honest thing to
// report is that whatever the consumer holds is now stale.
func (s *Store) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	s.eventsMu.Lock()
	id := s.nextSubscriber
	s.nextSubscriber++
	channel := make(chan Event, buffer)
	s.subscribers[id] = channel
	s.eventsMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.eventsMu.Lock()
			delete(s.subscribers, id)
			close(channel)
			s.eventsMu.Unlock()
		})
	}
	return channel, cancel
}

func (s *Store) publish(event Event) {
	event.At = time.Now().UTC()
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	for _, subscriber := range s.subscribers {
		select {
		case subscriber <- event:
		default:
			// The document remains authoritative, so a slow consumer must never
			// block a write. Empty its obsolete queue and put one guaranteed
			// marker in its place. The channel is buffered and cancellation
			// cannot close it while eventsMu is held.
			draining := true
			for draining {
				select {
				case <-subscriber:
				default:
					draining = false
				}
			}
			subscriber <- Event{Manager: "store", Type: "store.reload", At: event.At}
		}
	}
}

// PublishAppRuntime reports supervisor-only state without making the JavaScript
// runtime a second persistence authority. Apps remain durable in the document;
// this event only lets Manage update crash/retry status immediately.
func (s *Store) PublishAppRuntime(appID string, state any) {
	s.publish(Event{Manager: "apps", Type: "app.runtime", ID: appID, Data: state})
}

// ---- apps -------------------------------------------------------------------

// InstallApp registers an app release. Source is the GitHub link it came from,
// or empty for a local directory; it is stored as given so a later update
// installs from the same place the user chose. Installing always clears the
// result of an earlier update check, which the new release has answered.
func (s *Store) InstallApp(ctx context.Context, m *manifest.Manifest, root, source string) error {
	s.mu.Lock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := appRecord{
		ID: m.ID, Root: root, Enabled: true, Source: source,
		InstalledAt: now, UpdatedAt: now,
	}
	replaced := false
	for index := range s.doc.Apps {
		if s.doc.Apps[index].ID != m.ID {
			continue
		}
		record.InstalledAt = s.doc.Apps[index].InstalledAt
		s.doc.Apps[index] = record
		replaced = true
		break
	}
	if !replaced {
		s.doc.Apps = append(s.doc.Apps, record)
	}
	// The manifest is read from the bundle that was just published, never from
	// whatever the caller happened to hand over. A rootless app has no bundle:
	// there the handed manifest IS the announcement (or, for the native matter
	// app, the synthesised one) and it goes to the cache — never the document.
	if root == "" {
		s.manifests[m.ID] = m.Raw
	} else {
		s.loadManifest(record)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("install app %q: %w", m.ID, err)
	}
	s.publish(Event{Manager: "apps", Type: "app.update", ID: m.ID})
	return nil
}

// RecordUpdateCheck stores what a manual check found. An available version is
// the remote one; an app that is already current records an empty version so
// the Apps page stops offering an update it cannot perform.
func (s *Store) RecordUpdateCheck(ctx context.Context, id, availableVersion string, checkedAt time.Time) error {
	if err := s.mutateApp(ctx, id, func(app *appRecord) {
		app.UpdateVersion = availableVersion
		app.UpdateCheckedAt = checkedAt.UTC().Format(time.RFC3339Nano)
	}); err != nil {
		return err
	}
	s.publish(Event{Manager: "apps", Type: "app.update", ID: id})
	return nil
}

// SetAppRoot relocates a validated app bundle during backup restoration.
func (s *Store) SetAppRoot(ctx context.Context, appID, root string) error {
	return s.mutateApp(ctx, appID, func(app *appRecord) { app.Root = root })
}

func (s *Store) SetAppEnabled(ctx context.Context, id string, enabled bool) error {
	if err := s.mutateApp(ctx, id, func(app *appRecord) { app.Enabled = enabled }); err != nil {
		return err
	}
	s.publish(Event{Manager: "apps", Type: "app.update", ID: id, Data: map[string]any{"enabled": enabled}})
	return nil
}

func (s *Store) mutateApp(ctx context.Context, id string, change func(*appRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.doc.Apps {
		if s.doc.Apps[index].ID != id {
			continue
		}
		change(&s.doc.Apps[index])
		s.doc.Apps[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		s.loadManifest(s.doc.Apps[index])
		if err := s.saveLocked(); err != nil {
			return fmt.Errorf("update app %q: %w", id, err)
		}
		return nil
	}
	return fmt.Errorf("app %q is not installed", id)
}

func (s *Store) App(_ context.Context, id string) (App, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.doc.Apps {
		if record.ID == id {
			return s.appLocked(record), nil
		}
	}
	return App{}, fmt.Errorf("app %q is not installed", id)
}

func (s *Store) Apps(_ context.Context) ([]App, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	apps := make([]App, 0, len(s.doc.Apps))
	for _, record := range s.doc.Apps {
		apps = append(apps, s.appLocked(record))
	}
	sort.Slice(apps, func(left, right int) bool { return apps[left].ID < apps[right].ID })
	return apps, nil
}

func (s *Store) appLocked(record appRecord) App {
	// Het manifest komt uit de cache: van de bundel gelezen als die er is, en
	// anders door de app zelf verteld bij zijn laatste announce. Nog niets
	// gehoord (net herstart)? Dan een placeholder — de announce komt zo.
	appManifest := s.manifests[record.ID]
	if appManifest == nil {
		appManifest = map[string]any{"id": record.ID}
	}
	// De versie komt uit het manifest — wat de app zégt, niet wat wij ooit
	// opschreven. Bundel: app.json van schijf; aangemeld: de laatste announce.
	version, _ := appManifest["version"].(string)
	return App{
		ID: record.ID, Version: version, Root: record.Root, Enabled: record.Enabled,
		Offered:  record.Offered,
		Manifest: appManifest, Source: record.Source,
		UpdateVersion: record.UpdateVersion, UpdateCheckedAt: record.UpdateCheckedAt,
	}
}

// OfferApp records an app that announced itself: its manifest, and nothing more.
//
// It does not run. Announcing proves someone put this app here with a valid
// token, which is enough to be listed and not enough to be trusted — a leaked
// token would otherwise be a key to the house. Accepting is a separate act; see
// AcceptApp.
//
// An app that is already known is left alone. That includes one that is already
// offered: a container with restart:always would otherwise rewrite the same
// record every few seconds.
func (s *Store) OfferApp(ctx context.Context, m *manifest.Manifest) (bool, error) {
	if m == nil || m.ID == "" {
		return false, errors.New("an offered app needs a manifest with an id")
	}
	s.mu.Lock()
	for index := range s.doc.Apps {
		if s.doc.Apps[index].ID == m.ID {
			s.mu.Unlock()
			return false, nil
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.doc.Apps = append(s.doc.Apps, appRecord{
		ID: m.ID, Offered: true, Enabled: false,
		InstalledAt: now, UpdatedAt: now,
	})
	// Het manifest gaat de cache in, niet het document: de app herhaalt het
	// bij elke aanmelding toch.
	s.manifests[m.ID] = m.Raw
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()
	s.publish(Event{Manager: "apps", Type: "app.offered", ID: m.ID})
	return true, nil
}

// AcceptApp turns an offered app into one that may run. This is the act the
// offer deliberately withholds.
func (s *Store) AcceptApp(ctx context.Context, id string) (App, error) {
	s.mu.Lock()
	for index := range s.doc.Apps {
		if s.doc.Apps[index].ID != id {
			continue
		}
		if !s.doc.Apps[index].Offered {
			app := s.appLocked(s.doc.Apps[index])
			s.mu.Unlock()
			return app, nil // al geaccepteerd; niets te doen
		}
		s.doc.Apps[index].Offered = false
		s.doc.Apps[index].Enabled = true
		s.doc.Apps[index].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		app := s.appLocked(s.doc.Apps[index])
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return App{}, err
		}
		s.mu.Unlock()
		s.publish(Event{Manager: "apps", Type: "app.update", ID: id})
		return app, nil
	}
	s.mu.Unlock()
	return App{}, fmt.Errorf("no app %q offered itself", id)
}

// UpdateAnnouncedApp records the manifest carried by a known rootless app when
// it attaches. Placing a new slot image is how such an app is updated, so its
// version, drivers and UI description must move with the image that is actually
// connecting. A bundled app keeps app.json on disk as its authority and ignores
// this path.
//
// The manifest itself only touches the cache: it is application knowledge the
// app repeats at every attach, so persisting it would be both redundant and
// stale after the next image. Only the version — what the Apps page and update
// checks reason about — is Stulp's to remember. Equal announcements do not
// rewrite the document: slot apps retry while they wait for Stulp, so making
// the common case a no-op is important on flash-backed storage.
func (s *Store) UpdateAnnouncedApp(ctx context.Context, m *manifest.Manifest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m == nil || m.ID == "" {
		return false, errors.New("an announced app needs a manifest with an id")
	}
	s.mu.Lock()
	for index := range s.doc.Apps {
		record := &s.doc.Apps[index]
		if record.ID != m.ID {
			continue
		}
		if record.Root != "" {
			s.mu.Unlock()
			return false, nil
		}
		// Alleen de cache: het manifest (mét versie) is wat de app zegt, geen
		// document-kennis — er valt hier dus niets te bewaren, en op
		// flash-gebackte opslag is die gespaarde schrijf zelf ook winst.
		if reflect.DeepEqual(s.manifests[m.ID], m.Raw) {
			s.mu.Unlock()
			return false, nil
		}
		s.manifests[m.ID] = m.Raw
		s.mu.Unlock()
		s.publish(Event{Manager: "apps", Type: "app.update", ID: m.ID})
		return true, nil
	}
	s.mu.Unlock()
	return false, fmt.Errorf("app %q is not installed", m.ID)
}

// UninstallApp removes an app and everything that belongs to it: its devices,
// its settings and its notifications. The removed app and devices are returned
// so the caller can clean up what the document does not own, such as the bundle
// on disk and the Flows that used them.
func (s *Store) UninstallApp(ctx context.Context, id string) (App, []Device, error) {
	app, err := s.App(ctx, id)
	if err != nil {
		return App{}, nil, err
	}
	devices, err := s.Devices(ctx, id)
	if err != nil {
		return App{}, nil, err
	}

	s.mu.Lock()
	s.doc.Apps = removeWhere(s.doc.Apps, func(record appRecord) bool { return record.ID == id })
	s.doc.Devices = removeWhere(s.doc.Devices, func(record deviceRecord) bool { return record.AppID == id })
	s.doc.Notifications = removeWhere(s.doc.Notifications, func(record Notification) bool { return record.AppID == id })
	delete(s.doc.Settings, id)
	delete(s.doc.AppState, id)
	delete(s.manifests, id)
	for _, device := range devices {
		delete(s.state, device.ID)
	}
	saveErr := s.saveLocked()
	s.mu.Unlock()
	if saveErr != nil {
		return App{}, nil, fmt.Errorf("uninstall app %q: %w", id, saveErr)
	}

	// One change removed many things, so every subscriber that watches for
	// individual deletions needs each of them spelled out.
	for _, device := range devices {
		s.publish(Event{Manager: "devices", Type: "device.delete", ID: device.ID})
	}
	s.publish(Event{Manager: "apps", Type: "app.delete", ID: id})
	return app, devices, nil
}

func removeWhere[T any](items []T, remove func(T) bool) []T {
	kept := make([]T, 0, len(items))
	for _, item := range items {
		if !remove(item) {
			kept = append(kept, item)
		}
	}
	return kept
}

// ---- devices ----------------------------------------------------------------

func (s *Store) AddDevice(ctx context.Context, device Device) (Device, error) {
	if err := ctx.Err(); err != nil {
		return Device{}, err
	}
	if device.ID == "" {
		device.ID = newID()
	}
	if device.Data == nil {
		device.Data = make(map[string]any)
	}
	if device.Settings == nil {
		device.Settings = make(map[string]any)
	}
	if device.Store == nil {
		device.Store = make(map[string]any)
	}
	if device.Capabilities == nil {
		device.Capabilities = []string{}
	}
	if device.State == nil {
		device.State = make(map[string]any)
	}
	device.Available = true
	device.PreserveHardwareName()

	s.mu.Lock()
	installed := false
	for _, app := range s.doc.Apps {
		if app.ID == device.AppID {
			installed = true
			break
		}
	}
	if !installed {
		s.mu.Unlock()
		return Device{}, fmt.Errorf("app %q is not installed", device.AppID)
	}
	// One physical accessory pairs once. The old schema enforced this with a
	// unique index; here it is a scan, which is the same guarantee at this size.
	for _, existing := range s.doc.Devices {
		if existing.AppID == device.AppID && existing.DriverID == device.DriverID &&
			reflect.DeepEqual(existing.Data, device.Data) {
			s.mu.Unlock()
			return Device{}, fmt.Errorf("device %q of app %q is already paired", device.Name, device.AppID)
		}
	}
	if device.SortOrder <= 0 {
		device.SortOrder = s.nextDeviceOrderLocked(device.GroupID, "")
	}
	s.doc.Devices = append(s.doc.Devices, newDeviceRecord(device))
	s.state[device.ID] = cloneMap(device.State)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Device{}, fmt.Errorf("add device: %w", err)
	}
	s.publish(Event{Manager: "devices", Type: "device.create", ID: device.ID, Data: device})
	return device, nil
}

func (s *Store) Device(_ context.Context, id string) (Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.doc.Devices {
		if record.ID == id {
			return record.device(cloneMap(s.state[id])), nil
		}
	}
	return Device{}, fmt.Errorf("device %q does not exist", id)
}

// Devices lists every device, or only one app's when appID is given.
func (s *Store) Devices(_ context.Context, appID string) ([]Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	devices := make([]Device, 0, len(s.doc.Devices))
	for _, record := range s.doc.Devices {
		if appID != "" && record.AppID != appID {
			continue
		}
		devices = append(devices, record.device(cloneMap(s.state[record.ID])))
	}
	sort.SliceStable(devices, func(left, right int) bool {
		if devices[left].AppID != devices[right].AppID {
			return devices[left].AppID < devices[right].AppID
		}
		return devices[left].ID < devices[right].ID
	})
	return devices, nil
}

// UpdateDevice stores a device's configuration and its current values. Only the
// configuration reaches the file: a device reporting a new temperature changes
// nothing that needs writing, so the common case costs no disk at all.
func (s *Store) UpdateDevice(ctx context.Context, device Device) error {
	// A caller whose context is done has been told to stop. The SQL driver used
	// to refuse the write for free; here it is explicit, because a shutting-down
	// component must not resurrect state on its way out.
	if err := ctx.Err(); err != nil {
		return err
	}
	device.PreserveHardwareName()
	s.mu.Lock()
	index := -1
	for position, record := range s.doc.Devices {
		if record.ID == device.ID {
			index = position
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return fmt.Errorf("device %q does not exist", device.ID)
	}
	existing := s.doc.Devices[index]
	updated := newDeviceRecord(device)
	updated.CreatedAt, updated.UpdatedAt = existing.CreatedAt, existing.UpdatedAt

	stateChanged := !reflect.DeepEqual(s.state[device.ID], device.State)
	configurationChanged := !reflect.DeepEqual(existing, updated)
	s.state[device.ID] = cloneMap(device.State)
	var err error
	if configurationChanged {
		updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		s.doc.Devices[index] = updated
		err = s.saveLocked()
	}
	if err == nil && (stateChanged || configurationChanged) {
		// Publish the store-owned snapshot while the write lock still defines
		// update order. Consumers never observe a caller mutating this Device
		// after UpdateDevice returns, and rapid updates cannot swap places.
		published := s.doc.Devices[index].device(cloneMap(s.state[device.ID]))
		s.publish(Event{Manager: "devices", Type: "device.update", ID: device.ID, Data: published})
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("update device %q: %w", device.ID, err)
	}
	return nil
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	before := len(s.doc.Devices)
	s.doc.Devices = removeWhere(s.doc.Devices, func(record deviceRecord) bool { return record.ID == id })
	if len(s.doc.Devices) == before {
		s.mu.Unlock()
		return fmt.Errorf("device %q does not exist", id)
	}
	delete(s.state, id)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete device %q: %w", id, err)
	}
	s.publish(Event{Manager: "devices", Type: "device.delete", ID: id})
	return nil
}

// ---- app settings -----------------------------------------------------------

func (s *Store) Setting(_ context.Context, appID, key string) (any, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, exists := s.doc.Settings[appID][key]
	return value, exists, nil
}

func (s *Store) Settings(_ context.Context, appID string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.doc.Settings[appID]), nil
}

func (s *Store) SetSetting(ctx context.Context, appID, key string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.doc.Settings == nil {
		s.doc.Settings = make(map[string]map[string]any)
	}
	if s.doc.Settings[appID] == nil {
		s.doc.Settings[appID] = make(map[string]any)
	}
	s.doc.Settings[appID][key] = value
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("set app setting: %w", err)
	}
	s.publish(Event{Manager: "apps", Type: "app.settings", ID: appID, Data: map[string]any{"key": key}})
	return nil
}

func (s *Store) UnsetSetting(ctx context.Context, appID, key string) error {
	s.mu.Lock()
	delete(s.doc.Settings[appID], key)
	if len(s.doc.Settings[appID]) == 0 {
		delete(s.doc.Settings, appID)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("unset app setting: %w", err)
	}
	s.publish(Event{Manager: "apps", Type: "app.settings", ID: appID, Data: map[string]any{"key": key}})
	return nil
}

// ---- flow card triggers -----------------------------------------------------

// RecordFlowEvent announces that a card fired. Nothing is stored: the previous
// implementation kept an append-only table that nothing ever read, and the
// event bus is the actual mechanism.
func (s *Store) RecordFlowEvent(ctx context.Context, appID, cardType, cardID string, tokens, state any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.publishCardTrigger(appID, cardType, cardID, tokens, state)
	return nil
}

// RecordSystemFlowEvent announces a built-in Stulp trigger under the logical
// "stulp" card owner the Flow editor uses.
func (s *Store) RecordSystemFlowEvent(ctx context.Context, cardType, cardID string, tokens, state any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.publishCardTrigger("stulp", cardType, cardID, tokens, state)
	return nil
}

func (s *Store) publishCardTrigger(appID, cardType, cardID string, tokens, state any) {
	s.publish(Event{Manager: "flow", Type: "card.trigger", ID: cardID, Data: map[string]any{
		"appId": appID, "cardType": cardType, "tokens": tokens, "state": state,
	}})
}

func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return hex.EncodeToString(bytes[0:4]) + "-" + hex.EncodeToString(bytes[4:6]) + "-" +
		hex.EncodeToString(bytes[6:8]) + "-" + hex.EncodeToString(bytes[8:10]) + "-" + hex.EncodeToString(bytes[10:16])
}
