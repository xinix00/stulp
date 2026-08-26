// Package bridge exposes explicitly selected Stulp devices as stable Matter
// bridged endpoints. It owns the Matter-facing identity and endpoint mapping;
// the original app remains the sole owner of each physical device.
package bridge

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
)

const (
	RecordVersion       = 1
	AggregatorEndpoint  = 1
	FirstDeviceEndpoint = 2
)

type Kind string

const (
	KindOnOff          Kind = "onoff"
	KindDimmableLight  Kind = "dimmable_light"
	KindWindowCovering Kind = "window_covering"
	KindContact        Kind = "contact_sensor"
	KindOccupancy      Kind = "occupancy_sensor"
	KindTemperature    Kind = "temperature_sensor"
	KindHumidity       Kind = "humidity_sensor"
)

type EndpointRecord struct {
	Endpoint     uint16    `json:"endpoint"`
	DeviceID     string    `json:"deviceId"`
	Kind         Kind      `json:"kind"`
	Name         string    `json:"name"`
	Enabled      bool      `json:"enabled"`
	Capabilities []string  `json:"capabilities"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Record is persisted inside the Matter plugin's private app state. Endpoint
// numbers are never reused: a receiving ecosystem treats endpoint identity as
// durable, so recycling a removed curtain's number for a lamp would mutate an
// existing accessory behind its back.
type Record struct {
	Version      int              `json:"version"`
	NextEndpoint uint16           `json:"nextEndpoint"`
	Endpoints    []EndpointRecord `json:"endpoints,omitempty"`
	Server       ServerRecord     `json:"server,omitempty"`
}

type ServerRecord struct {
	Port          uint16            `json:"port,omitempty"`
	Discriminator uint16            `json:"discriminator,omitempty"`
	Passcode      uint32            `json:"passcode,omitempty"`
	Iterations    uint32            `json:"iterations,omitempty"`
	Salt          []byte            `json:"salt,omitempty"`
	Attestation   AttestationRecord `json:"attestation,omitempty"`
	Fabrics       []FabricRecord    `json:"fabrics,omitempty"`
}

type AttestationRecord struct {
	PAICertificate []byte `json:"paiCertificate,omitempty"`
	DACCertificate []byte `json:"dacCertificate,omitempty"`
	DACPrivateKey  []byte `json:"dacPrivateKey,omitempty"`
}

type FabricRecord struct {
	Index         uint8  `json:"index"`
	FabricID      uint64 `json:"fabricId"`
	NodeID        uint64 `json:"nodeId"`
	AdminNodeID   uint64 `json:"adminNodeId"`
	AdminVendorID uint16 `json:"adminVendorId"`
	IPK           []byte `json:"ipk"`
	Root          []byte `json:"root"`
	NOC           []byte `json:"noc"`
	ICAC          []byte `json:"icac,omitempty"`
	PrivateKey    []byte `json:"privateKey"`
}

type SaveFunc func(Record) error
type InvokeFunc func(deviceID, capability string, value any) error

type Manager struct {
	mu      sync.RWMutex
	record  Record
	devices map[string]appsdk.HomeDevice
	save    SaveFunc
	invoke  InvokeFunc
	changed chan EndpointRecord
}

func NewManager(record Record, devices []appsdk.HomeDevice, save SaveFunc, invoke InvokeFunc) (*Manager, error) {
	if record.Version == 0 {
		record.Version = RecordVersion
	}
	if record.Version != RecordVersion {
		return nil, fmt.Errorf("unsupported Matter bridge record version %d", record.Version)
	}
	if record.NextEndpoint < FirstDeviceEndpoint {
		record.NextEndpoint = FirstDeviceEndpoint
	}
	manager := &Manager{record: record, devices: map[string]appsdk.HomeDevice{}, save: save, invoke: invoke, changed: make(chan EndpointRecord, 64)}
	for _, device := range devices {
		manager.devices[device.ID] = device
	}
	if err := manager.validate(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) validate() error {
	seenEndpoints := map[uint16]bool{}
	seenDevices := map[string]bool{}
	for _, endpoint := range m.record.Endpoints {
		if endpoint.Endpoint < FirstDeviceEndpoint || seenEndpoints[endpoint.Endpoint] {
			return fmt.Errorf("invalid or duplicate Matter bridge endpoint %d", endpoint.Endpoint)
		}
		if endpoint.DeviceID == "" || seenDevices[endpoint.DeviceID] {
			return fmt.Errorf("empty or duplicate Matter bridge device %q", endpoint.DeviceID)
		}
		seenEndpoints[endpoint.Endpoint], seenDevices[endpoint.DeviceID] = true, true
		if endpoint.Endpoint >= m.record.NextEndpoint {
			m.record.NextEndpoint = endpoint.Endpoint + 1
		}
	}
	return nil
}

func (m *Manager) Record() Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneRecord(m.record)
}

func (m *Manager) updateServer(server ServerRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record.Server = server
	if m.save != nil {
		return m.save(cloneRecord(m.record))
	}
	return nil
}

func (m *Manager) Candidates() []Candidate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Candidate, 0, len(m.devices))
	for _, device := range m.devices {
		kind, capabilities, ok := classify(device)
		if !ok {
			continue
		}
		candidate := Candidate{DeviceID: device.ID, Name: device.Name, AppID: device.AppID, Class: device.Class,
			Available: device.Available, Kind: kind, Capabilities: capabilities}
		if endpoint := endpointByDevice(m.record.Endpoints, device.ID); endpoint != nil {
			candidate.Selected, candidate.Endpoint = endpoint.Enabled, endpoint.Endpoint
		}
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].DeviceID < result[j].DeviceID
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

type Candidate struct {
	DeviceID     string   `json:"deviceId"`
	Name         string   `json:"name"`
	AppID        string   `json:"appId"`
	Class        string   `json:"class"`
	Available    bool     `json:"available"`
	Kind         Kind     `json:"kind"`
	Capabilities []string `json:"capabilities"`
	Selected     bool     `json:"selected"`
	Endpoint     uint16   `json:"endpoint,omitempty"`
}

func (m *Manager) SetExported(deviceID string, exported bool) (EndpointRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	device, ok := m.devices[deviceID]
	if !ok {
		return EndpointRecord{}, fmt.Errorf("Stulp device %s is not available to the Matter bridge", deviceID)
	}
	kind, capabilities, ok := classify(device)
	if !ok {
		return EndpointRecord{}, errors.New("device has no Matter bridge adapter")
	}
	index := slices.IndexFunc(m.record.Endpoints, func(endpoint EndpointRecord) bool { return endpoint.DeviceID == deviceID })
	if index < 0 {
		if !exported {
			return EndpointRecord{}, errors.New("device is not exported")
		}
		if m.record.NextEndpoint == 0 || m.record.NextEndpoint == 0xFFFF {
			return EndpointRecord{}, errors.New("Matter bridge endpoint space is exhausted")
		}
		m.record.Endpoints = append(m.record.Endpoints, EndpointRecord{
			Endpoint: m.record.NextEndpoint, DeviceID: deviceID, Kind: kind, Name: device.Name,
			Enabled: true, Capabilities: capabilities, CreatedAt: time.Now().UTC(),
		})
		m.record.NextEndpoint++
		index = len(m.record.Endpoints) - 1
	} else {
		m.record.Endpoints[index].Enabled = exported
		m.record.Endpoints[index].Name = device.Name
		m.record.Endpoints[index].Kind = kind
		m.record.Endpoints[index].Capabilities = capabilities
	}
	if m.save != nil {
		if err := m.save(cloneRecord(m.record)); err != nil {
			return EndpointRecord{}, err
		}
	}
	endpoint := m.record.Endpoints[index]
	notifyChanged(m.changed, endpoint)
	return endpoint, nil
}

func (m *Manager) UpdateDevice(device appsdk.HomeDevice) {
	m.mu.Lock()
	if device.Removed {
		delete(m.devices, device.ID)
	} else {
		m.devices[device.ID] = device
	}
	endpoint := endpointByDevice(m.record.Endpoints, device.ID)
	var changed EndpointRecord
	if endpoint != nil {
		changed = *endpoint
	}
	m.mu.Unlock()
	if changed.Endpoint != 0 {
		notifyChanged(m.changed, changed)
	}
}

func (m *Manager) Changes() <-chan EndpointRecord { return m.changed }

func (m *Manager) Device(endpoint uint16) (appsdk.HomeDevice, EndpointRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, record := range m.record.Endpoints {
		if record.Endpoint == endpoint && record.Enabled {
			device, ok := m.devices[record.DeviceID]
			return device, record, ok
		}
	}
	return appsdk.HomeDevice{}, EndpointRecord{}, false
}

func (m *Manager) Invoke(endpoint uint16, command string, value any) error {
	m.mu.RLock()
	var record EndpointRecord
	for _, candidate := range m.record.Endpoints {
		if candidate.Endpoint == endpoint && candidate.Enabled {
			record = candidate
			break
		}
	}
	invoke := m.invoke
	m.mu.RUnlock()
	if record.Endpoint == 0 || invoke == nil {
		return errors.New("Matter bridge endpoint is not controllable")
	}
	capability, translated, err := translateCommand(record, command, value)
	if err != nil {
		return err
	}
	return invoke(record.DeviceID, capability, translated)
}

func translateCommand(endpoint EndpointRecord, command string, value any) (string, any, error) {
	switch endpoint.Kind {
	case KindOnOff, KindDimmableLight:
		switch command {
		case "on":
			return "onoff", true, nil
		case "off":
			return "onoff", false, nil
		case "toggle":
			return "onoff", "toggle", nil
		case "level":
			return "dim", value, nil
		}
	case KindWindowCovering:
		switch command {
		case "open":
			return "windowcoverings_state", "up", nil
		case "close":
			return "windowcoverings_state", "down", nil
		case "stop":
			return "windowcoverings_state", "idle", nil
		case "closed_fraction":
			number, ok := numeric(value)
			if !ok || number < 0 || number > 1 {
				return "", nil, errors.New("closed fraction must be 0..1")
			}
			// Stulp/Somfy uses 1=open; Matter Window Covering uses 0%=open,
			// 100%=closed. This inversion is intentional.
			return "windowcoverings_set", 1 - number, nil
		}
	}
	return "", nil, fmt.Errorf("command %q is not supported by %s", command, endpoint.Kind)
}

func classify(device appsdk.HomeDevice) (Kind, []string, bool) {
	has := func(id string) bool { return slices.Contains(device.Capabilities, id) }
	switch {
	case has("windowcoverings_set") && has("windowcoverings_state"):
		return KindWindowCovering, []string{"windowcoverings_set", "windowcoverings_state"}, true
	case has("onoff") && has("dim"):
		return KindDimmableLight, []string{"onoff", "dim"}, true
	case has("onoff"):
		return KindOnOff, []string{"onoff"}, true
	case has("alarm_contact"):
		return KindContact, []string{"alarm_contact"}, true
	case has("alarm_motion"):
		return KindOccupancy, []string{"alarm_motion"}, true
	case has("measure_temperature"):
		return KindTemperature, []string{"measure_temperature"}, true
	case has("measure_humidity"):
		return KindHumidity, []string{"measure_humidity"}, true
	}
	return "", nil, false
}

func endpointByDevice(endpoints []EndpointRecord, deviceID string) *EndpointRecord {
	for index := range endpoints {
		if endpoints[index].DeviceID == deviceID {
			return &endpoints[index]
		}
	}
	return nil
}

func cloneRecord(record Record) Record {
	record.Endpoints = slices.Clone(record.Endpoints)
	for index := range record.Endpoints {
		record.Endpoints[index].Capabilities = slices.Clone(record.Endpoints[index].Capabilities)
	}
	record.Server.Salt = slices.Clone(record.Server.Salt)
	record.Server.Fabrics = slices.Clone(record.Server.Fabrics)
	return record
}

func notifyChanged(channel chan EndpointRecord, endpoint EndpointRecord) {
	select {
	case channel <- endpoint:
	default:
	}
}

func numeric(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case uint16:
		return float64(number), true
	}
	return 0, false
}
