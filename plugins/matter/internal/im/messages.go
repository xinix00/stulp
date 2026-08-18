// Package im implements the small generic core of Matter's Interaction
// Model. Cluster-specific code supplies command fields and consumes attribute
// values; Read and Invoke framing stays shared by commissioning and devices.
package im

import (
	"errors"
	"fmt"

	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

const Revision = 1

// Interaction Model protocol opcodes.
const (
	OpcodeStatusResponse    uint8 = 0x01
	OpcodeReadRequest       uint8 = 0x02
	OpcodeSubscribeRequest  uint8 = 0x03
	OpcodeSubscribeResponse uint8 = 0x04
	OpcodeReportData        uint8 = 0x05
	OpcodeWriteRequest      uint8 = 0x06
	OpcodeWriteResponse     uint8 = 0x07
	OpcodeInvokeRequest     uint8 = 0x08
	OpcodeInvokeResponse    uint8 = 0x09
	OpcodeTimedRequest      uint8 = 0x0A
)

// Global Interaction Model statuses used by the generic client.
const (
	StatusSuccess              uint8 = 0x00
	StatusFailure              uint8 = 0x01
	StatusInvalidSubscription  uint8 = 0x7D
	StatusInvalidAction        uint8 = 0x80
	StatusUnsupportedCommand   uint8 = 0x81
	StatusUnsupportedAttribute uint8 = 0x86
	StatusUnsupportedAccess    uint8 = 0x7E
	StatusBusy                 uint8 = 0x9C
)

type AttributePath struct {
	Endpoint  *uint16
	Cluster   *uint32
	Attribute *uint32
}

// EventPath selects Matter events. Nil fields are wildcards; subscriptions
// commonly use an endpoint-only path so newly introduced cluster events are
// received without a Stulp update.
type EventPath struct {
	Node     *uint64
	Endpoint *uint16
	Cluster  *uint32
	Event    *uint32
	Urgent   *bool
}

type ReadRequest struct {
	Paths          []AttributePath
	FabricFiltered bool
}

func ConcreteAttributePath(endpoint uint16, cluster, attribute uint32) AttributePath {
	return AttributePath{Endpoint: &endpoint, Cluster: &cluster, Attribute: &attribute}
}

type CommandPath struct {
	Endpoint uint16
	Cluster  uint32
	Command  uint32
}

// Command writes one InvokeRequest entry. Fields must write the command's
// context-tagged field structure using the supplied tag.
type Command struct {
	Path   CommandPath
	Fields func(*tlv.Writer, tlv.Tag)
	Ref    *uint16
}

// InvokeRequestCommand is the decoded peer-side view of Command. Stulp is a
// controller today, but keeping request decoding beside response decoding
// lets protocol tests and future native servers share the exact same parser.
type InvokeRequestCommand struct {
	Path   CommandPath
	Fields Value
	Ref    *uint16
}

// CommandResponse describes either a command-data response (Status nil) or a
// status-only response. Status-only responses retain the request path, which
// is important for commands such as AddTrustedRoot that have no response
// command of their own.
type CommandResponse struct {
	Path   CommandPath
	Fields func(*tlv.Writer, tlv.Tag)
	Status *Status
	Ref    *uint16
}

func EncodeReadRequest(paths []AttributePath, fabricFiltered bool) ([]byte, error) {
	if len(paths) == 0 {
		return nil, errors.New("ReadRequest needs at least one attribute path")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.StartArray(tlv.Context(0))
	for _, path := range paths {
		writer.StartList(tlv.Anonymous())
		if path.Endpoint != nil {
			writer.PutUint(tlv.Context(2), uint64(*path.Endpoint))
		}
		if path.Cluster != nil {
			writer.PutUint(tlv.Context(3), uint64(*path.Cluster))
		}
		if path.Attribute != nil {
			writer.PutUint(tlv.Context(4), uint64(*path.Attribute))
		}
		writer.EndContainer()
	}
	writer.EndContainer()
	writer.PutBool(tlv.Context(3), fabricFiltered)
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func DecodeReadRequest(data []byte) (ReadRequest, error) {
	root, err := decodeTree(data)
	if err != nil {
		return ReadRequest{}, fmt.Errorf("decode ReadRequest: %w", err)
	}
	requests, err := requiredField(root, 0, tlv.TypeArray)
	if err != nil {
		return ReadRequest{}, err
	}
	result := ReadRequest{Paths: make([]AttributePath, 0, len(requests.Children))}
	for _, encoded := range requests.Children {
		if encoded.Type != tlv.TypeList {
			return ReadRequest{}, errors.New("ReadRequest attribute path is not a list")
		}
		path, err := decodeAttributePath(encoded)
		if err != nil {
			return ReadRequest{}, err
		}
		result.Paths = append(result.Paths, path)
	}
	fabricFiltered, ok := root.Field(3)
	if !ok || fabricFiltered.Type != tlv.TypeBool {
		return ReadRequest{}, errors.New("ReadRequest has no fabricFiltered boolean")
	}
	result.FabricFiltered = fabricFiltered.Bool
	return result, nil
}

type SubscribeRequest struct {
	KeepSubscriptions bool
	MinInterval       uint16
	MaxInterval       uint16
	Attributes        []AttributePath
	Events            []EventPath
	FabricFiltered    bool
}

func EncodeSubscribeRequest(attributes []AttributePath, events []EventPath, minInterval, maxInterval uint16,
	keepSubscriptions, fabricFiltered bool) ([]byte, error) {
	if len(attributes) == 0 && len(events) == 0 {
		return nil, errors.New("SubscribeRequest needs at least one attribute or event path")
	}
	if maxInterval == 0 || minInterval > maxInterval {
		return nil, errors.New("SubscribeRequest has an invalid interval range")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBool(tlv.Context(0), keepSubscriptions)
	writer.PutUintWidth(tlv.Context(1), uint64(minInterval), 2)
	writer.PutUintWidth(tlv.Context(2), uint64(maxInterval), 2)
	if len(attributes) > 0 {
		writer.StartArray(tlv.Context(3))
		for _, path := range attributes {
			writeAttributePath(&writer, tlv.Anonymous(), path)
		}
		writer.EndContainer()
	}
	if len(events) > 0 {
		writer.StartArray(tlv.Context(4))
		for _, path := range events {
			writeEventPath(&writer, tlv.Anonymous(), path)
		}
		writer.EndContainer()
	}
	writer.PutBool(tlv.Context(7), fabricFiltered)
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func DecodeSubscribeRequest(data []byte) (SubscribeRequest, error) {
	root, err := decodeTree(data)
	if err != nil {
		return SubscribeRequest{}, fmt.Errorf("decode SubscribeRequest: %w", err)
	}
	keep, ok := root.Field(0)
	if !ok || keep.Type != tlv.TypeBool {
		return SubscribeRequest{}, errors.New("SubscribeRequest has no keepSubscriptions boolean")
	}
	minInterval, _, err := uintField(root, 1, true)
	if err != nil || minInterval > 0xFFFF {
		return SubscribeRequest{}, errors.New("SubscribeRequest has no valid minimum interval")
	}
	maxInterval, _, err := uintField(root, 2, true)
	if err != nil || maxInterval == 0 || maxInterval > 0xFFFF || minInterval > maxInterval {
		return SubscribeRequest{}, errors.New("SubscribeRequest has no valid maximum interval")
	}
	fabricFiltered, ok := root.Field(7)
	if !ok || fabricFiltered.Type != tlv.TypeBool {
		return SubscribeRequest{}, errors.New("SubscribeRequest has no fabricFiltered boolean")
	}
	result := SubscribeRequest{
		KeepSubscriptions: keep.Bool, MinInterval: uint16(minInterval), MaxInterval: uint16(maxInterval),
		FabricFiltered: fabricFiltered.Bool,
	}
	if raw, ok := root.Field(3); ok {
		if raw.Type != tlv.TypeArray {
			return SubscribeRequest{}, errors.New("SubscribeRequest attributeRequests is not an array")
		}
		for _, encoded := range raw.Children {
			path, err := decodeAttributePath(encoded)
			if err != nil {
				return SubscribeRequest{}, err
			}
			result.Attributes = append(result.Attributes, path)
		}
	}
	if raw, ok := root.Field(4); ok {
		if raw.Type != tlv.TypeArray {
			return SubscribeRequest{}, errors.New("SubscribeRequest eventRequests is not an array")
		}
		for _, encoded := range raw.Children {
			path, err := decodeEventPath(encoded)
			if err != nil {
				return SubscribeRequest{}, err
			}
			result.Events = append(result.Events, path)
		}
	}
	if len(result.Attributes) == 0 && len(result.Events) == 0 {
		return SubscribeRequest{}, errors.New("SubscribeRequest has no paths")
	}
	return result, nil
}

func writeAttributePath(writer *tlv.Writer, tag tlv.Tag, path AttributePath) {
	writer.StartList(tag)
	if path.Endpoint != nil {
		writer.PutUint(tlv.Context(2), uint64(*path.Endpoint))
	}
	if path.Cluster != nil {
		writer.PutUint(tlv.Context(3), uint64(*path.Cluster))
	}
	if path.Attribute != nil {
		writer.PutUint(tlv.Context(4), uint64(*path.Attribute))
	}
	writer.EndContainer()
}

func writeEventPath(writer *tlv.Writer, tag tlv.Tag, path EventPath) {
	writer.StartList(tag)
	if path.Node != nil {
		writer.PutUint(tlv.Context(0), *path.Node)
	}
	if path.Endpoint != nil {
		writer.PutUint(tlv.Context(1), uint64(*path.Endpoint))
	}
	if path.Cluster != nil {
		writer.PutUint(tlv.Context(2), uint64(*path.Cluster))
	}
	if path.Event != nil {
		writer.PutUint(tlv.Context(3), uint64(*path.Event))
	}
	if path.Urgent != nil {
		writer.PutBool(tlv.Context(4), *path.Urgent)
	}
	writer.EndContainer()
}

func EncodeInvokeRequest(commands []Command, timed bool) ([]byte, error) {
	if len(commands) == 0 {
		return nil, errors.New("InvokeRequest needs at least one command")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBool(tlv.Context(0), false) // suppressResponse
	writer.PutBool(tlv.Context(1), timed)
	writer.StartArray(tlv.Context(2))
	for _, command := range commands {
		writer.StartStructure(tlv.Anonymous()) // CommandDataIB
		writeCommandPath(&writer, tlv.Context(0), command.Path)
		if command.Fields == nil {
			writer.StartStructure(tlv.Context(1))
			writer.EndContainer()
		} else {
			command.Fields(&writer, tlv.Context(1))
		}
		if command.Ref != nil {
			writer.PutUint(tlv.Context(2), uint64(*command.Ref))
		}
		writer.EndContainer()
	}
	writer.EndContainer()
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

// EncodeTimedRequest opens a bounded window for the following write or
// invoke on the same exchange. Safety-sensitive clusters such as Door Lock
// require this two-message transaction.
func EncodeTimedRequest(timeout uint16) ([]byte, error) {
	if timeout == 0 {
		return nil, errors.New("TimedRequest needs a non-zero timeout")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUintWidth(tlv.Context(0), uint64(timeout), 2)
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func DecodeInvokeRequest(data []byte) ([]InvokeRequestCommand, error) {
	root, err := decodeTree(data)
	if err != nil {
		return nil, fmt.Errorf("decode InvokeRequest: %w", err)
	}
	requests, err := requiredField(root, 2, tlv.TypeArray)
	if err != nil {
		return nil, err
	}
	result := make([]InvokeRequestCommand, 0, len(requests.Children))
	for _, request := range requests.Children {
		pathValue, err := requiredField(request, 0, tlv.TypeList)
		if err != nil {
			return nil, err
		}
		path, err := decodeCommandPath(pathValue)
		if err != nil {
			return nil, err
		}
		fields, err := requiredField(request, 1, tlv.TypeStructure)
		if err != nil {
			return nil, err
		}
		decoded := InvokeRequestCommand{Path: path, Fields: fields}
		if ref, ok, err := uintField(request, 2, false); err != nil {
			return nil, err
		} else if ok {
			if ref > 0xFFFF {
				return nil, errors.New("invoke request reference exceeds 16 bits")
			}
			converted := uint16(ref)
			decoded.Ref = &converted
		}
		result = append(result, decoded)
	}
	return result, nil
}

// EncodeInvokeResponseMessage encodes one response chunk. The explicit flags
// are what Matter servers and stateful protocol tests need to split a large
// response; pass false for both to send a single unchunked message.
func EncodeInvokeResponseMessage(responses []CommandResponse, suppressResponse, moreChunkedMessages bool) ([]byte, error) {
	if len(responses) == 0 {
		return nil, errors.New("InvokeResponse needs at least one response")
	}
	if suppressResponse && moreChunkedMessages {
		return nil, errors.New("InvokeResponse cannot suppress a required chunk response")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBool(tlv.Context(0), suppressResponse)
	writer.StartArray(tlv.Context(1))
	for _, response := range responses {
		writer.StartStructure(tlv.Anonymous()) // InvokeResponseIB
		if response.Status == nil {
			writer.StartStructure(tlv.Context(0)) // CommandDataIB
			writeCommandPath(&writer, tlv.Context(0), response.Path)
			if response.Fields != nil {
				response.Fields(&writer, tlv.Context(1))
			} else {
				writer.StartStructure(tlv.Context(1))
				writer.EndContainer()
			}
			if response.Ref != nil {
				writer.PutUint(tlv.Context(2), uint64(*response.Ref))
			}
			writer.EndContainer()
		} else {
			writer.StartStructure(tlv.Context(1)) // CommandStatusIB
			writeCommandPath(&writer, tlv.Context(0), response.Path)
			writer.StartStructure(tlv.Context(1)) // StatusIB
			writer.PutUint(tlv.Context(0), uint64(response.Status.Global))
			if response.Status.Cluster != nil {
				writer.PutUint(tlv.Context(1), uint64(*response.Status.Cluster))
			}
			writer.EndContainer()
			if response.Ref != nil {
				writer.PutUint(tlv.Context(2), uint64(*response.Ref))
			}
			writer.EndContainer()
		}
		writer.EndContainer()
	}
	writer.EndContainer()
	if moreChunkedMessages {
		writer.PutBool(tlv.Context(2), true)
	}
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func writeCommandPath(writer *tlv.Writer, tag tlv.Tag, path CommandPath) {
	writer.StartList(tag)
	writer.PutUint(tlv.Context(0), uint64(path.Endpoint))
	writer.PutUint(tlv.Context(1), uint64(path.Cluster))
	writer.PutUint(tlv.Context(2), uint64(path.Command))
	writer.EndContainer()
}

type AttributeReport struct {
	Path        AttributePath
	DataVersion *uint32
	Value       Value
	Status      *Status
}

type ReportDataMessage struct {
	SubscriptionID      *uint32
	Reports             []AttributeReport
	Events              []EventReport
	SuppressResponse    bool
	MoreChunkedMessages bool
}

// AttributeData is the server-side counterpart of AttributeReport. Value
// writes the attribute's cluster-specific TLV value at the supplied tag.
type AttributeData struct {
	Path        AttributePath
	DataVersion *uint32
	Value       func(*tlv.Writer, tlv.Tag)
}

// AttributeWrite is one concrete AttributeDataIB in a WriteRequest. Writes
// deliberately share the same value callback as server-side reports: the
// cluster owns the TLV type while the generic Interaction Model owns framing.
type AttributeWrite = AttributeData

// AttributeWriteResult is one AttributeStatusIB returned by a Matter server.
type AttributeWriteResult struct {
	Path   AttributePath
	Status Status
}

type DecodedAttributeWrite struct {
	Path        AttributePath
	DataVersion *uint32
	Value       Value
}

type WriteRequestMessage struct {
	SuppressResponse bool
	Timed            bool
	Writes           []DecodedAttributeWrite
}

func EncodeWriteRequest(writes []AttributeWrite, timed bool) ([]byte, error) {
	if len(writes) == 0 {
		return nil, errors.New("WriteRequest needs at least one attribute")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBool(tlv.Context(0), false) // suppressResponse
	writer.PutBool(tlv.Context(1), timed)
	writer.StartArray(tlv.Context(2))
	for _, write := range writes {
		if write.Path.Endpoint == nil || write.Path.Cluster == nil || write.Path.Attribute == nil || write.Value == nil {
			return nil, errors.New("WriteRequest needs concrete attribute paths and values")
		}
		writer.StartStructure(tlv.Anonymous()) // AttributeDataIB
		if write.DataVersion != nil {
			writer.PutUint(tlv.Context(0), uint64(*write.DataVersion))
		}
		writeAttributePath(&writer, tlv.Context(1), write.Path)
		write.Value(&writer, tlv.Context(2))
		writer.EndContainer()
	}
	writer.EndContainer()
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func DecodeWriteRequest(data []byte) (WriteRequestMessage, error) {
	root, err := decodeTree(data)
	if err != nil {
		return WriteRequestMessage{}, fmt.Errorf("decode WriteRequest: %w", err)
	}
	suppress, suppressOK := root.Field(0)
	timed, timedOK := root.Field(1)
	encoded, err := requiredField(root, 2, tlv.TypeArray)
	if err != nil || !suppressOK || suppress.Type != tlv.TypeBool || !timedOK || timed.Type != tlv.TypeBool {
		return WriteRequestMessage{}, errors.Join(err, errors.New("WriteRequest has invalid control fields"))
	}
	result := WriteRequestMessage{SuppressResponse: suppress.Bool, Timed: timed.Bool, Writes: make([]DecodedAttributeWrite, 0, len(encoded.Children))}
	for _, raw := range encoded.Children {
		if raw.Type != tlv.TypeStructure {
			return WriteRequestMessage{}, errors.New("WriteRequest AttributeDataIB is not a structure")
		}
		pathValue, err := requiredField(raw, 1, tlv.TypeList)
		if err != nil {
			return WriteRequestMessage{}, err
		}
		path, err := decodeAttributePath(pathValue)
		if err != nil {
			return WriteRequestMessage{}, err
		}
		value, ok := raw.Field(2)
		if !ok {
			return WriteRequestMessage{}, errors.New("WriteRequest AttributeDataIB is missing data")
		}
		write := DecodedAttributeWrite{Path: path, Value: value}
		if version, ok, err := uintField(raw, 0, false); err != nil {
			return WriteRequestMessage{}, err
		} else if ok {
			if version > 0xFFFFFFFF {
				return WriteRequestMessage{}, errors.New("WriteRequest data version exceeds 32 bits")
			}
			converted := uint32(version)
			write.DataVersion = &converted
		}
		result.Writes = append(result.Writes, write)
	}
	return result, nil
}

func DecodeWriteResponse(data []byte) ([]AttributeWriteResult, error) {
	root, err := decodeTree(data)
	if err != nil {
		return nil, fmt.Errorf("decode WriteResponse: %w", err)
	}
	statuses, err := requiredField(root, 0, tlv.TypeArray)
	if err != nil {
		return nil, err
	}
	results := make([]AttributeWriteResult, 0, len(statuses.Children))
	for _, raw := range statuses.Children {
		pathValue, err := requiredField(raw, 0, tlv.TypeList)
		if err != nil {
			return nil, err
		}
		path, err := decodeAttributePath(pathValue)
		if err != nil {
			return nil, err
		}
		statusValue, err := requiredField(raw, 1, tlv.TypeStructure)
		if err != nil {
			return nil, err
		}
		status, err := decodeStatus(statusValue)
		if err != nil {
			return nil, err
		}
		results = append(results, AttributeWriteResult{Path: path, Status: status})
	}
	return results, nil
}

// EncodeWriteResponse is used by the encrypted fake device and wire tests.
// A production controller only needs DecodeWriteResponse.
func EncodeWriteResponse(results []AttributeWriteResult) ([]byte, error) {
	if len(results) == 0 {
		return nil, errors.New("WriteResponse needs at least one status")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.StartArray(tlv.Context(0))
	for _, result := range results {
		if result.Path.Endpoint == nil || result.Path.Cluster == nil || result.Path.Attribute == nil {
			return nil, errors.New("WriteResponse needs concrete attribute paths")
		}
		writer.StartStructure(tlv.Anonymous()) // AttributeStatusIB
		writeAttributePath(&writer, tlv.Context(0), result.Path)
		writer.StartStructure(tlv.Context(1)) // StatusIB
		writer.PutUint(tlv.Context(0), uint64(result.Status.Global))
		if result.Status.Cluster != nil {
			writer.PutUint(tlv.Context(1), uint64(*result.Status.Cluster))
		}
		writer.EndContainer()
		writer.EndContainer()
	}
	writer.EndContainer()
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

type EventReport struct {
	Path                 EventPath
	Number               uint64
	Priority             uint8
	EpochTimestamp       *uint64
	SystemTimestamp      *uint64
	DeltaEpochTimestamp  *uint64
	DeltaSystemTimestamp *uint64
	Value                Value
	Status               *Status
}

// EventData is the server-side counterpart of EventReport. It is used by the
// protocol simulator and can later be reused if Stulp exposes native Matter
// server endpoints.
type EventData struct {
	Path                 EventPath
	Number               uint64
	Priority             uint8
	EpochTimestamp       *uint64
	SystemTimestamp      *uint64
	DeltaEpochTimestamp  *uint64
	DeltaSystemTimestamp *uint64
	Value                func(*tlv.Writer, tlv.Tag)
}

type Status struct {
	Global  uint8
	Cluster *uint8
}

func (s Status) OK() bool { return s.Global == StatusSuccess }

func (s Status) Error() string {
	if s.Cluster != nil {
		return fmt.Sprintf("Interaction Model status 0x%02x, cluster status 0x%02x", s.Global, *s.Cluster)
	}
	return fmt.Sprintf("Interaction Model status 0x%02x", s.Global)
}

func DecodeReportDataMessage(data []byte) (ReportDataMessage, error) {
	root, err := decodeTree(data)
	if err != nil {
		return ReportDataMessage{}, fmt.Errorf("decode ReportData: %w", err)
	}
	if root.Type != tlv.TypeStructure {
		return ReportDataMessage{}, errors.New("ReportData root is not a structure")
	}
	var message ReportDataMessage
	if value, ok, err := uintField(root, 0, false); err != nil {
		return ReportDataMessage{}, err
	} else if ok {
		if value > 0xFFFFFFFF {
			return ReportDataMessage{}, errors.New("ReportData subscription ID exceeds 32 bits")
		}
		converted := uint32(value)
		message.SubscriptionID = &converted
	}
	if value, ok := root.Field(3); ok {
		if value.Type != tlv.TypeBool {
			return ReportDataMessage{}, errors.New("ReportData moreChunkedMessages is not boolean")
		}
		message.MoreChunkedMessages = value.Bool
	}
	if value, ok := root.Field(4); ok {
		if value.Type != tlv.TypeBool {
			return ReportDataMessage{}, errors.New("ReportData suppressResponse is not boolean")
		}
		message.SuppressResponse = value.Bool
	}
	if message.MoreChunkedMessages && message.SuppressResponse {
		return ReportDataMessage{}, errors.New("ReportData cannot suppress a required chunk response")
	}
	if reports, ok := root.Field(1); ok {
		if reports.Type != tlv.TypeArray {
			return ReportDataMessage{}, errors.New("ReportData attributeReports is not an array")
		}
		result := make([]AttributeReport, 0, len(reports.Children))
		for _, raw := range reports.Children {
			report, err := decodeAttributeReport(raw)
			if err != nil {
				return ReportDataMessage{}, err
			}
			result = append(result, report)
		}
		message.Reports = result
	}
	if events, ok := root.Field(2); ok {
		if events.Type != tlv.TypeArray {
			return ReportDataMessage{}, errors.New("ReportData eventReports is not an array")
		}
		for _, raw := range events.Children {
			report, err := decodeEventReport(raw)
			if err != nil {
				return ReportDataMessage{}, err
			}
			message.Events = append(message.Events, report)
		}
	}
	return message, nil
}

func EncodeReportDataMessage(subscriptionID *uint32, reports []AttributeData, events []EventData,
	suppressResponse, moreChunkedMessages bool) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	if subscriptionID != nil {
		writer.PutUintWidth(tlv.Context(0), uint64(*subscriptionID), 4)
	}
	if len(reports) > 0 {
		writer.StartArray(tlv.Context(1))
		for _, report := range reports {
			if report.Path.Endpoint == nil || report.Path.Cluster == nil || report.Path.Attribute == nil || report.Value == nil {
				return nil, errors.New("ReportData needs concrete attribute paths and values")
			}
			writer.StartStructure(tlv.Anonymous()) // AttributeReportIB
			writer.StartStructure(tlv.Context(1))  // AttributeDataIB
			if report.DataVersion != nil {
				writer.PutUint(tlv.Context(0), uint64(*report.DataVersion))
			}
			writer.StartList(tlv.Context(1))
			writer.PutUint(tlv.Context(2), uint64(*report.Path.Endpoint))
			writer.PutUint(tlv.Context(3), uint64(*report.Path.Cluster))
			writer.PutUint(tlv.Context(4), uint64(*report.Path.Attribute))
			writer.EndContainer()
			report.Value(&writer, tlv.Context(2))
			writer.EndContainer()
			writer.EndContainer()
		}
		writer.EndContainer()
	}
	if len(events) > 0 {
		writer.StartArray(tlv.Context(2))
		for _, event := range events {
			if event.Path.Endpoint == nil || event.Path.Cluster == nil || event.Path.Event == nil || event.Value == nil {
				return nil, errors.New("ReportData needs concrete event paths and values")
			}
			timestamps := 0
			for _, timestamp := range []*uint64{event.EpochTimestamp, event.SystemTimestamp, event.DeltaEpochTimestamp, event.DeltaSystemTimestamp} {
				if timestamp != nil {
					timestamps++
				}
			}
			if timestamps != 1 {
				return nil, errors.New("EventDataIB needs exactly one timestamp")
			}
			writer.StartStructure(tlv.Anonymous()) // EventReportIB
			writer.StartStructure(tlv.Context(1))  // EventDataIB
			writeEventPath(&writer, tlv.Context(0), event.Path)
			writer.PutUint(tlv.Context(1), event.Number)
			writer.PutUintWidth(tlv.Context(2), uint64(event.Priority), 1)
			if event.EpochTimestamp != nil {
				writer.PutUint(tlv.Context(3), *event.EpochTimestamp)
			}
			if event.SystemTimestamp != nil {
				writer.PutUint(tlv.Context(4), *event.SystemTimestamp)
			}
			if event.DeltaEpochTimestamp != nil {
				writer.PutUint(tlv.Context(5), *event.DeltaEpochTimestamp)
			}
			if event.DeltaSystemTimestamp != nil {
				writer.PutUint(tlv.Context(6), *event.DeltaSystemTimestamp)
			}
			event.Value(&writer, tlv.Context(7))
			writer.EndContainer()
			writer.EndContainer()
		}
		writer.EndContainer()
	}
	if moreChunkedMessages {
		writer.PutBool(tlv.Context(3), true)
	}
	if suppressResponse {
		writer.PutBool(tlv.Context(4), true)
	}
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func decodeEventReport(raw Value) (EventReport, error) {
	if raw.Type != tlv.TypeStructure {
		return EventReport{}, errors.New("EventReportIB is not a structure")
	}
	if statusValue, ok := raw.Field(0); ok {
		pathValue, err := requiredField(statusValue, 0, tlv.TypeList)
		if err != nil {
			return EventReport{}, err
		}
		path, err := decodeEventPath(pathValue)
		if err != nil {
			return EventReport{}, err
		}
		statusIB, err := requiredField(statusValue, 1, tlv.TypeStructure)
		if err != nil {
			return EventReport{}, err
		}
		status, err := decodeStatus(statusIB)
		return EventReport{Path: path, Status: &status}, err
	}
	data, err := requiredField(raw, 1, tlv.TypeStructure)
	if err != nil {
		return EventReport{}, errors.New("EventReportIB has neither status nor data")
	}
	pathValue, err := requiredField(data, 0, tlv.TypeList)
	if err != nil {
		return EventReport{}, err
	}
	path, err := decodeEventPath(pathValue)
	if err != nil {
		return EventReport{}, err
	}
	number, _, err := uintField(data, 1, true)
	if err != nil {
		return EventReport{}, err
	}
	priority, _, err := uintField(data, 2, true)
	if err != nil || priority > 0xFF {
		return EventReport{}, errors.New("EventDataIB has no valid priority")
	}
	value, ok := data.Field(7)
	if !ok {
		return EventReport{}, errors.New("EventDataIB is missing data")
	}
	report := EventReport{Path: path, Number: number, Priority: uint8(priority), Value: value}
	for tag, target := range map[uint8]**uint64{
		3: &report.EpochTimestamp, 4: &report.SystemTimestamp,
		5: &report.DeltaEpochTimestamp, 6: &report.DeltaSystemTimestamp,
	} {
		if timestamp, ok, err := uintField(data, tag, false); err != nil {
			return EventReport{}, err
		} else if ok {
			converted := timestamp
			*target = &converted
		}
	}
	timestamps := 0
	for _, timestamp := range []*uint64{report.EpochTimestamp, report.SystemTimestamp, report.DeltaEpochTimestamp, report.DeltaSystemTimestamp} {
		if timestamp != nil {
			timestamps++
		}
	}
	if timestamps != 1 {
		return EventReport{}, errors.New("EventDataIB needs exactly one timestamp")
	}
	return report, nil
}

func decodeEventPath(value Value) (EventPath, error) {
	if value.Type != tlv.TypeList {
		return EventPath{}, errors.New("event path is not a list")
	}
	var path EventPath
	if node, ok, err := uintField(value, 0, false); err != nil {
		return path, err
	} else if ok {
		path.Node = &node
	}
	if endpoint, ok, err := uintField(value, 1, false); err != nil {
		return path, err
	} else if ok {
		if endpoint > 0xFFFF {
			return path, errors.New("event endpoint exceeds 16 bits")
		}
		converted := uint16(endpoint)
		path.Endpoint = &converted
	}
	if cluster, ok, err := uintField(value, 2, false); err != nil {
		return path, err
	} else if ok {
		if cluster > 0xFFFFFFFF {
			return path, errors.New("event cluster exceeds 32 bits")
		}
		converted := uint32(cluster)
		path.Cluster = &converted
	}
	if event, ok, err := uintField(value, 3, false); err != nil {
		return path, err
	} else if ok {
		if event > 0xFFFFFFFF {
			return path, errors.New("event ID exceeds 32 bits")
		}
		converted := uint32(event)
		path.Event = &converted
	}
	if urgent, ok := value.Field(4); ok {
		if urgent.Type != tlv.TypeBool {
			return path, errors.New("event urgent flag is not boolean")
		}
		converted := urgent.Bool
		path.Urgent = &converted
	}
	return path, nil
}

func decodeAttributeReport(raw Value) (AttributeReport, error) {
	if raw.Type != tlv.TypeStructure {
		return AttributeReport{}, errors.New("AttributeReportIB is not a structure")
	}
	if statusValue, ok := raw.Field(0); ok {
		pathValue, err := requiredField(statusValue, 0, tlv.TypeList)
		if err != nil {
			return AttributeReport{}, err
		}
		path, err := decodeAttributePath(pathValue)
		if err != nil {
			return AttributeReport{}, err
		}
		statusIB, err := requiredField(statusValue, 1, tlv.TypeStructure)
		if err != nil {
			return AttributeReport{}, err
		}
		status, err := decodeStatus(statusIB)
		return AttributeReport{Path: path, Status: &status}, err
	}
	dataValue, err := requiredField(raw, 1, tlv.TypeStructure)
	if err != nil {
		return AttributeReport{}, errors.New("AttributeReportIB has neither status nor data")
	}
	pathValue, err := requiredField(dataValue, 1, tlv.TypeList)
	if err != nil {
		return AttributeReport{}, err
	}
	path, err := decodeAttributePath(pathValue)
	if err != nil {
		return AttributeReport{}, err
	}
	value, ok := dataValue.Field(2)
	if !ok {
		return AttributeReport{}, errors.New("AttributeDataIB is missing data")
	}
	report := AttributeReport{Path: path, Value: value}
	if version, ok, err := uintField(dataValue, 0, false); err != nil {
		return AttributeReport{}, err
	} else if ok && version <= 0xFFFFFFFF {
		converted := uint32(version)
		report.DataVersion = &converted
	}
	return report, nil
}

func decodeAttributePath(value Value) (AttributePath, error) {
	var path AttributePath
	if endpoint, ok, err := uintField(value, 2, false); err != nil {
		return path, err
	} else if ok {
		if endpoint > 0xFFFF {
			return path, errors.New("attribute endpoint exceeds 16 bits")
		}
		converted := uint16(endpoint)
		path.Endpoint = &converted
	}
	if cluster, ok, err := uintField(value, 3, false); err != nil {
		return path, err
	} else if ok {
		if cluster > 0xFFFFFFFF {
			return path, errors.New("attribute cluster exceeds 32 bits")
		}
		converted := uint32(cluster)
		path.Cluster = &converted
	}
	if attribute, ok, err := uintField(value, 4, false); err != nil {
		return path, err
	} else if ok {
		if attribute > 0xFFFFFFFF {
			return path, errors.New("attribute ID exceeds 32 bits")
		}
		converted := uint32(attribute)
		path.Attribute = &converted
	}
	return path, nil
}

func decodeStatus(value Value) (Status, error) {
	global, _, err := uintField(value, 0, true)
	if err != nil {
		return Status{}, fmt.Errorf("status: %w", err)
	}
	if global > 0xFF {
		return Status{}, errors.New("global status exceeds 8 bits")
	}
	status := Status{Global: uint8(global)}
	if cluster, ok, err := uintField(value, 1, false); err != nil {
		return Status{}, err
	} else if ok {
		if cluster > 0xFF {
			return Status{}, errors.New("cluster status exceeds 8 bits")
		}
		converted := uint8(cluster)
		status.Cluster = &converted
	}
	return status, nil
}

type InvokeResult struct {
	Path   CommandPath
	Fields *Value
	Status Status
	Ref    *uint16
}

type InvokeResponseMessage struct {
	Results             []InvokeResult
	SuppressResponse    bool
	MoreChunkedMessages bool
}

func DecodeInvokeResponseMessage(data []byte) (InvokeResponseMessage, error) {
	root, err := decodeTree(data)
	if err != nil {
		return InvokeResponseMessage{}, fmt.Errorf("decode InvokeResponse: %w", err)
	}
	var message InvokeResponseMessage
	if suppress, ok := root.Field(0); ok {
		if suppress.Type != tlv.TypeBool {
			return InvokeResponseMessage{}, errors.New("InvokeResponse suppressResponse is not boolean")
		}
		message.SuppressResponse = suppress.Bool
	}
	if more, ok := root.Field(2); ok {
		if more.Type != tlv.TypeBool {
			return InvokeResponseMessage{}, errors.New("InvokeResponse moreChunkedMessages is not boolean")
		}
		message.MoreChunkedMessages = more.Bool
	}
	if message.SuppressResponse && message.MoreChunkedMessages {
		return InvokeResponseMessage{}, errors.New("InvokeResponse cannot suppress a required chunk response")
	}
	responses, err := requiredField(root, 1, tlv.TypeArray)
	if err != nil {
		return InvokeResponseMessage{}, err
	}
	result := make([]InvokeResult, 0, len(responses.Children))
	for _, response := range responses.Children {
		decoded, err := decodeInvokeResult(response)
		if err != nil {
			return InvokeResponseMessage{}, err
		}
		result = append(result, decoded)
	}
	message.Results = result
	return message, nil
}

func decodeInvokeResult(response Value) (InvokeResult, error) {
	if command, ok := response.Field(0); ok {
		pathValue, err := requiredField(command, 0, tlv.TypeList)
		if err != nil {
			return InvokeResult{}, err
		}
		path, err := decodeCommandPath(pathValue)
		if err != nil {
			return InvokeResult{}, err
		}
		result := InvokeResult{Path: path, Status: Status{Global: StatusSuccess}}
		if fields, ok := command.Field(1); ok {
			result.Fields = &fields
		}
		if ref, ok, err := uintField(command, 2, false); err != nil {
			return InvokeResult{}, err
		} else if ok && ref <= 0xFFFF {
			converted := uint16(ref)
			result.Ref = &converted
		}
		return result, nil
	}
	statusValue, err := requiredField(response, 1, tlv.TypeStructure)
	if err != nil {
		return InvokeResult{}, errors.New("InvokeResponseIB has neither command nor status")
	}
	pathValue, err := requiredField(statusValue, 0, tlv.TypeList)
	if err != nil {
		return InvokeResult{}, err
	}
	path, err := decodeCommandPath(pathValue)
	if err != nil {
		return InvokeResult{}, err
	}
	statusIB, err := requiredField(statusValue, 1, tlv.TypeStructure)
	if err != nil {
		return InvokeResult{}, err
	}
	status, err := decodeStatus(statusIB)
	return InvokeResult{Path: path, Status: status}, err
}

func decodeCommandPath(value Value) (CommandPath, error) {
	endpoint, _, err := uintField(value, 0, true)
	if err != nil {
		return CommandPath{}, fmt.Errorf("command endpoint: %w", err)
	}
	if endpoint > 0xFFFF {
		return CommandPath{}, errors.New("command endpoint exceeds 16 bits")
	}
	cluster, _, err := uintField(value, 1, true)
	if err != nil {
		return CommandPath{}, fmt.Errorf("command cluster: %w", err)
	}
	if cluster > 0xFFFFFFFF {
		return CommandPath{}, errors.New("command cluster exceeds 32 bits")
	}
	command, _, err := uintField(value, 2, true)
	if err != nil {
		return CommandPath{}, fmt.Errorf("command ID: %w", err)
	}
	if command > 0xFFFFFFFF {
		return CommandPath{}, errors.New("command ID exceeds 32 bits")
	}
	return CommandPath{Endpoint: uint16(endpoint), Cluster: uint32(cluster), Command: uint32(command)}, nil
}

func DecodeStatusResponse(data []byte) (Status, error) {
	root, err := decodeTree(data)
	if err != nil {
		return Status{}, err
	}
	value, _, err := uintField(root, 0, true)
	if err != nil || value > 0xFF {
		return Status{}, errors.New("invalid StatusResponse status")
	}
	return Status{Global: uint8(value)}, nil
}

func EncodeStatusResponse(status uint8) ([]byte, error) {
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUint(tlv.Context(0), uint64(status))
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

type SubscribeResponse struct {
	SubscriptionID uint32
	MaxInterval    uint16
}

func EncodeSubscribeResponse(subscriptionID uint32, maxInterval uint16) ([]byte, error) {
	if maxInterval == 0 {
		return nil, errors.New("SubscribeResponse needs a non-zero maximum interval")
	}
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutUintWidth(tlv.Context(0), uint64(subscriptionID), 4)
	writer.PutUintWidth(tlv.Context(2), uint64(maxInterval), 2)
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	return writer.Bytes()
}

func DecodeSubscribeResponse(data []byte) (SubscribeResponse, error) {
	root, err := decodeTree(data)
	if err != nil {
		return SubscribeResponse{}, fmt.Errorf("decode SubscribeResponse: %w", err)
	}
	id, _, err := uintField(root, 0, true)
	if err != nil || id > 0xFFFFFFFF {
		return SubscribeResponse{}, errors.New("SubscribeResponse has no valid subscription ID")
	}
	maxInterval, _, err := uintField(root, 2, true)
	if err != nil || maxInterval == 0 || maxInterval > 0xFFFF {
		return SubscribeResponse{}, errors.New("SubscribeResponse has no valid maximum interval")
	}
	return SubscribeResponse{SubscriptionID: uint32(id), MaxInterval: uint16(maxInterval)}, nil
}
