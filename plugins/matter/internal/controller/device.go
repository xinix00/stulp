package controller

import "strings"

// De typen waarin de controller denkt.
//
// Bewust niet die van internal/store. De controller draait in het
// plugin-proces en komt daar niet bij het document: hij mag er alleen via de
// socketpair bij. Zolang hij store.Device gebruikte linkte elke plugin-binary
// de document-engine mee -- precies wat het procesmodel wil uitsluiten.
//
// Wat hier staat is dus geen kopie voor de vorm. Het is de lijst van wat de
// controller werkelijk van een apparaat nodig heeft, en de Backing vertaalt dat
// naar wat Stulp bewaart.

// Device is één apparaat zoals de controller het ziet.
//
// Geen AppID: een Backing hoort bij één app en levert alleen zijn eigen
// apparaten. Vragen naar het app-id was een controle op iets dat niet meer kan
// misgaan.
type Device struct {
	ID       string
	DriverID string
	Name     string
	// GroupID is waar de gebruiker het apparaat heeft neergezet. De controller
	// verzint hem niet, maar houdt hem vast: een modelupgrade mag een apparaat
	// niet uit zijn kamer tillen.
	GroupID string
	Class   string
	// Data is wat bij het koppelen is vastgelegd en daarna niet verandert.
	Data map[string]any
	// Settings ziet de gebruiker, Store niet: node-id, adres, certificaat.
	Settings     map[string]any
	Store        map[string]any
	Capabilities []string
	State        map[string]any
	Available    bool
	Message      string
}

// FabricRecord is de identiteit van deze controller op het netwerk: het
// fabric, zijn wortelcertificaat en de sleutel waarmee sessies worden opgezet.
//
// Dit is het enige wat écht niet verloren mag gaan. Zonder deze gegevens is elk
// gekoppeld apparaat onbereikbaar en moet alles opnieuw gekoppeld worden -- het
// apparaat kent dit fabric en niets anders.
type FabricRecord struct {
	FabricID          uint64 `json:"fabricId"`
	RootID            uint64 `json:"rootId"`
	ControllerNodeID  uint64 `json:"controllerNodeId"`
	IPK               []byte `json:"ipk"`
	RootKeyDER        []byte `json:"rootKeyDer"`
	RootCertDER       []byte `json:"rootCertDer"`
	ControllerKeyDER  []byte `json:"controllerKeyDer"`
	ControllerCertDER []byte `json:"controllerCertDer"`
	NextNodeID        uint64 `json:"nextNodeId"`
}

// DeviceReplacement zegt dat één apparaat door een ander vervangen is, en welke
// capability nu welke geworden is.
//
// Dit is alles wat de controller over Flows mag zeggen. Hij leest ze niet en
// schrijft ze niet; hij meldt een verandering en Stulp bepaalt wat dat voor de
// Flows van de gebruiker betekent.
type DeviceReplacement struct {
	DeviceID     string
	Capabilities map[string]string
}

// deviceHardwareNameKey moet gelijk zijn aan de sleutel die Stulp gebruikt.
// Het staat op twee plekken omdat de controller Stulps pakketten niet mag
// importeren; de afspraak is de string zelf, net als bij elke andere sleutel in
// de store van een apparaat.
const deviceHardwareNameKey = "__stulp.hardwareName"

// PreserveHardwareName legt de naam vast zoals het apparaat zichzelf noemt, één
// keer. Daarna hernoemt de gebruiker vrij: wie het apparaat "Lamp boven" noemt
// wil dat niet bij de volgende modelverversing terugveren naar de fabrieksnaam.
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

// HardwareName is hoe het apparaat zichzelf noemde bij het koppelen, of zijn
// huidige naam als dat nooit is vastgelegd.
func (d Device) HardwareName() string {
	if value, _ := d.Store[deviceHardwareNameKey].(string); strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(d.Name)
}
