package controller

import "context"

// Backing is alles wat de controller buiten zichzelf nodig heeft.
//
// Hij draait in het proces van de Matter-plugin en praat hier via de SDK met
// Stulp. De interface is smal gehouden op wat er echt langs moet, want elke
// methode die erbij komt is iets dat een app over de rest van het huis te weten
// komt.
//
// Wat er níet in staat is net zo belangrijk. Er is geen toegang tot Flows: een
// app hoort de Flows van de gebruiker niet te lezen of te herschrijven. Wat
// daarvoor nodig was -- na een modelwijziging de verwijzingen laten kloppen --
// is teruggebracht tot één mededeling: dit apparaat is dat geworden.
type Backing interface {
	// Apparaten. Aanmaken doet Stulp bij het koppelen; een app werkt bij.
	AddDevice(ctx context.Context, device Device) (Device, error)
	Device(ctx context.Context, id string) (Device, error)
	Devices(ctx context.Context) ([]Device, error)
	UpdateDevice(ctx context.Context, device Device) error
	DeleteDevice(ctx context.Context, id string) error

	// De fabric hoort bij de plugin en niet bij Stulp: het is de identiteit van
	// deze controller, niet iets van het platform.
	Fabric(ctx context.Context) (FabricRecord, bool, error)
	SaveFabric(ctx context.Context, record FabricRecord) error
	AllocateNodeID(ctx context.Context) (uint64, error)

	// Een Flow-kaart afvuren als een node iets meldt.
	RecordSystemFlowEvent(ctx context.Context, cardType, cardID string, tokens, state any) error

	// Melden dat een apparaat door een ander vervangen is.
	ReplaceDeviceReferences(ctx context.Context, replacements map[string]DeviceReplacement) error
}
