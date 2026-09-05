// Package plugin is de kant van Stulp die met een app praat.
//
// Een app is een eigen programma. Stulp start de binary, geeft hem één kant van
// een socketpair als fd 3, en spreekt er appproto tegen -- geen gedeelde heap,
// geen gedeelde event loop, en geen manier voor de ene app om bij de andere te
// komen. De kant van de app is internal/appsdk; het protocol staat in
// docs/app-processes.md en hoe je een plugin schrijft in docs/plugins.md.
package plugin

import (
	"context"

	"github.com/xinix00/stulp/internal/store"
)

// Runtime is de grens tussen Stulp en één draaiende app.
//
// Dit is de hele richting Stulp-naar-app. De andere kant -- wat een app aan
// Stulp vraagt -- is wat Process afhandelt, en samen zijn ze het contract dat
// over de procesgrens gaat.
//
// De interface bestaat apart zodat een test er zijn eigen implementatie voor in
// de plaats kan zetten zonder een proces te starten.
type Runtime interface {
	// Lifecycle.
	Start(ctx context.Context) error
	Close()

	// Pairing.
	PairListDevices(ctx context.Context, driverID string) ([]map[string]any, error)
	StartPairSession(ctx context.Context, driverID, sessionID string) ([]string, error)
	PairEmit(ctx context.Context, sessionID, event string, data any) (any, error)
	ClosePairSession(ctx context.Context, sessionID string) error
	AddPairedDevice(ctx context.Context, driverID string, candidate map[string]any) (store.Device, error)

	// The app's own HTTP endpoints.
	InvokeAPI(ctx context.Context, handler string, query, body map[string]any) (any, error)
	ReadUIAsset(ctx context.Context, path string) (UIAsset, error)

	// App settings.
	SetSetting(ctx context.Context, name string, value any) error
	UnsetSetting(ctx context.Context, name string) error

	// Devices. InvokeCapabilities carries every value for one device in a
	// single request, so the app can combine what the device combines; it
	// answers per capability what failed.
	InvokeCapability(ctx context.Context, deviceID, capabilityID string, value any, options map[string]any) error
	InvokeCapabilities(ctx context.Context, deviceID string, commands []CapabilityCommand, options map[string]any) (map[string]error, error)
	UpdateDeviceSettings(ctx context.Context, deviceID string, patch map[string]any) (store.Device, error)
	RenameDevice(ctx context.Context, deviceID, name string) (store.Device, error)
	DeleteDevice(ctx context.Context, deviceID string) error

	// Flow.
	InvokeFlow(ctx context.Context, cardType, cardID string, args, state map[string]any) (any, error)
	InvokeFlowAutocomplete(ctx context.Context, cardType, cardID, argument, query string, args map[string]any) (any, error)

	// What the app registered: flow cards, capability listeners, media.
	Registrations(ctx context.Context) (RegistrationSnapshot, error)
	DeviceMedia(ctx context.Context, deviceID string) ([]MediaRegistration, error)
	ResolveMedia(ctx context.Context, deviceID, slot, kind string) (VideoStream, error)
}

// CapabilityCommand is één waarde voor één capability, zoals een scene er
// meerdere tegelijk aan een apparaat geeft.
type CapabilityCommand struct {
	Capability string `json:"capability"`
	Value      any    `json:"value"`
}

// Een app is een eigen proces dat appproto spreekt. Zie docs/app-processes.md
// voor het protocol en docs/plugins.md voor de kant van de app.
var _ Runtime = (*Process)(nil)

// NewRuntime creates the runtime for an app.
//
// Welke implementatie dat is komt uit Options.NewRuntime, zodat een test er een
// eigen voor in de plaats kan zetten zonder een proces te starten.
func NewRuntime(ctx context.Context, database *store.Store, appID string, options Options) (Runtime, error) {
	if options.NewRuntime != nil {
		return options.NewRuntime(ctx, database, appID, options)
	}
	return NewPlugin(ctx, database, appID, options)
}
