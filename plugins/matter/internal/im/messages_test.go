package im

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

func TestReadRequestWireLayout(t *testing.T) {
	payload, err := EncodeReadRequest([]AttributePath{ConcreteAttributePath(1, 6, 0)}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x15,       // anonymous structure
		0x36, 0x00, // attributeRequests array, tag 0
		0x17,             // anonymous path list
		0x24, 0x02, 0x01, // endpoint 1
		0x24, 0x03, 0x06, // cluster 6
		0x24, 0x04, 0x00, // attribute 0
		0x18, 0x18, // path and array end
		0x29, 0x03, // fabricFiltered true
		0x24, 0xFF, 0x01, 0x18, // revision 1 and root end
	}
	if !bytes.Equal(payload, want) {
		t.Fatalf("ReadRequest = % X\nwant        = % X", payload, want)
	}
}

func TestWriteMessagesRoundTrip(t *testing.T) {
	version := uint32(19)
	path := ConcreteAttributePath(7, 0x0080, 0)
	payload, err := EncodeWriteRequest([]AttributeWrite{{
		Path: path, DataVersion: &version,
		Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutUintWidth(tag, 2, 1) },
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeWriteRequest(payload)
	if err != nil || request.SuppressResponse || request.Timed || len(request.Writes) != 1 ||
		request.Writes[0].Path.Endpoint == nil || *request.Writes[0].Path.Endpoint != 7 ||
		request.Writes[0].Path.Cluster == nil || *request.Writes[0].Path.Cluster != 0x0080 ||
		request.Writes[0].Path.Attribute == nil || *request.Writes[0].Path.Attribute != 0 ||
		request.Writes[0].DataVersion == nil || *request.Writes[0].DataVersion != version ||
		request.Writes[0].Value.Type != tlv.TypeUint || request.Writes[0].Value.Uint != 2 {
		t.Fatalf("WriteRequest = %#v, %v", request, err)
	}

	clusterStatus := uint8(3)
	responseBytes, err := EncodeWriteResponse([]AttributeWriteResult{{
		Path: path, Status: Status{Global: StatusUnsupportedAccess, Cluster: &clusterStatus},
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeWriteResponse(responseBytes)
	if err != nil || len(response) != 1 || response[0].Status.Global != StatusUnsupportedAccess ||
		response[0].Status.Cluster == nil || *response[0].Status.Cluster != clusterStatus {
		t.Fatalf("WriteResponse = %#v, %v", response, err)
	}
}

func TestSubscribeMessagesAndEventReportRoundTrip(t *testing.T) {
	endpoint := uint16(1)
	requestBytes, err := EncodeSubscribeRequest(
		[]AttributePath{ConcreteAttributePath(endpoint, 6, 0)},
		[]EventPath{{Endpoint: &endpoint}}, 0, 300, false, true)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeSubscribeRequest(requestBytes)
	if err != nil || request.MinInterval != 0 || request.MaxInterval != 300 || request.KeepSubscriptions ||
		!request.FabricFiltered || len(request.Attributes) != 1 || len(request.Events) != 1 ||
		request.Events[0].Endpoint == nil || *request.Events[0].Endpoint != endpoint {
		t.Fatalf("SubscribeRequest = %#v, %v", request, err)
	}

	subscriptionID := uint32(0x12345678)
	cluster, eventID := uint32(0x003B), uint32(1)
	timestamp := uint64(9876)
	reportBytes, err := EncodeReportDataMessage(&subscriptionID, []AttributeData{{
		Path:  ConcreteAttributePath(endpoint, 6, 0),
		Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutBool(tag, true) },
	}}, []EventData{{
		Path: EventPath{Endpoint: &endpoint, Cluster: &cluster, Event: &eventID}, Number: 42, Priority: 1,
		SystemTimestamp: &timestamp,
		Value: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 1)
			writer.EndContainer()
		},
	}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	report, err := DecodeReportDataMessage(reportBytes)
	if err != nil || report.SubscriptionID == nil || *report.SubscriptionID != subscriptionID ||
		len(report.Reports) != 1 || len(report.Events) != 1 || report.Events[0].Number != 42 ||
		report.Events[0].Path.Cluster == nil || *report.Events[0].Path.Cluster != cluster ||
		report.Events[0].SystemTimestamp == nil || *report.Events[0].SystemTimestamp != timestamp {
		t.Fatalf("ReportData = %#v, %v", report, err)
	}
	responseBytes, err := EncodeSubscribeResponse(subscriptionID, 120)
	if err != nil {
		t.Fatal(err)
	}
	response, err := DecodeSubscribeResponse(responseBytes)
	if err != nil || response.SubscriptionID != subscriptionID || response.MaxInterval != 120 {
		t.Fatalf("SubscribeResponse = %#v, %v", response, err)
	}
}

func TestInvokeRequestContainsGenericCommandFields(t *testing.T) {
	payload, err := EncodeInvokeRequest([]Command{{
		Path: CommandPath{Endpoint: 0, Cluster: 0x30, Command: 0},
		Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 60)
			writer.PutUint(tlv.Context(1), 1)
			writer.EndContainer()
		},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeTree(payload)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := requiredField(root, 2, tlv.TypeArray)
	if err != nil || len(requests.Children) != 1 {
		t.Fatalf("invoke requests = %#v, %v", requests, err)
	}
	command := requests.Children[0]
	path, err := requiredField(command, 0, tlv.TypeList)
	if err != nil {
		t.Fatal(err)
	}
	decodedPath, err := decodeCommandPath(path)
	if err != nil || decodedPath != (CommandPath{Endpoint: 0, Cluster: 0x30, Command: 0}) {
		t.Fatalf("path = %+v, %v", decodedPath, err)
	}
	fields, err := requiredField(command, 1, tlv.TypeStructure)
	if err != nil {
		t.Fatal(err)
	}
	if expiry, _, _ := uintField(fields, 0, true); expiry != 60 {
		t.Fatalf("expiry = %d", expiry)
	}
	decoded, err := DecodeInvokeRequest(payload)
	if err != nil || len(decoded) != 1 || decoded[0].Path != (CommandPath{Endpoint: 0, Cluster: 0x30, Command: 0}) {
		t.Fatalf("decoded request = %#v, %v", decoded, err)
	}
	if expiry, ok := decoded[0].Fields.Field(0); !ok || expiry.Uint != 60 {
		t.Fatalf("decoded expiry = %#v", expiry)
	}
}

func TestEncodeInvokeResponseCoversCommandAndStatusForms(t *testing.T) {
	success := Status{Global: StatusSuccess}
	payload, err := EncodeInvokeResponseMessage([]CommandResponse{
		{Path: CommandPath{Endpoint: 0, Cluster: 0x30, Command: 1}, Fields: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.PutUint(tlv.Context(0), 0)
			writer.EndContainer()
		}},
		{Path: CommandPath{Endpoint: 0, Cluster: 0x3E, Command: 0x0B}, Status: &success},
	}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeInvokeResponseMessage(payload)
	responses := decoded.Results
	if err != nil || len(responses) != 2 {
		t.Fatalf("responses = %#v, %v", responses, err)
	}
	if responses[0].Fields == nil || !responses[0].Status.OK() || responses[1].Fields != nil || !responses[1].Status.OK() {
		t.Fatalf("unexpected response forms: %#v", responses)
	}
}

func TestDecodeReportAndInvokeResponse(t *testing.T) {
	report := reportData(t, true)
	decodedReport, err := DecodeReportDataMessage(report)
	reports := decodedReport.Reports
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %#v, %v", reports, err)
	}
	if reports[0].Path.Endpoint == nil || *reports[0].Path.Endpoint != 1 ||
		reports[0].Path.Cluster == nil || *reports[0].Path.Cluster != 6 ||
		reports[0].Value.Type != tlv.TypeBool || !reports[0].Value.Bool {
		t.Fatalf("decoded report = %+v", reports[0])
	}

	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.PutBool(tlv.Context(0), false)
	writer.StartArray(tlv.Context(1))
	writer.StartStructure(tlv.Anonymous())
	writer.StartStructure(tlv.Context(1)) // CommandStatusIB
	writeCommandPath(&writer, tlv.Context(0), CommandPath{Endpoint: 1, Cluster: 6, Command: 1})
	writer.StartStructure(tlv.Context(1)) // StatusIB
	writer.PutUint(tlv.Context(0), uint64(StatusSuccess))
	writer.EndContainer()
	writer.EndContainer()
	writer.EndContainer()
	writer.EndContainer()
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	invoke, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	decodedInvoke, err := DecodeInvokeResponseMessage(invoke)
	results := decodedInvoke.Results
	if err != nil || len(results) != 1 || !results[0].Status.OK() {
		t.Fatalf("invoke results = %#v, %v", results, err)
	}
}

func TestClientReadUsesEncryptedTransport(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	device, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	i2r := bytes.Repeat([]byte{0x41}, 16)
	r2i := bytes.Repeat([]byte{0x52}, 16)
	controllerSession, err := controller.RegisterSession(transport.SessionConfig{
		LocalID: 1, PeerID: 2, OutboundKey: i2r, InboundKey: r2i, Remote: device.LocalAddr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.RegisterSession(transport.SessionConfig{
		LocalID: 2, PeerID: 1, OutboundKey: r2i, InboundKey: i2r, Remote: controller.LocalAddr(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		exchange, err := device.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()
		opcode, _, err := exchange.Receive(ctx)
		if err == nil && opcode != OpcodeReadRequest {
			err = &unexpectedOpcode{got: opcode, want: OpcodeReadRequest}
		}
		if err == nil {
			err = exchange.Send(ctx, OpcodeReportData, reportData(t, true))
		}
		if err == nil {
			var status []byte
			opcode, status, err = exchange.Receive(ctx)
			if err == nil && opcode != OpcodeStatusResponse {
				err = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
			}
			if err == nil {
				decoded, decodeErr := DecodeStatusResponse(status)
				if decodeErr != nil || !decoded.OK() {
					err = decodeErr
					if err == nil {
						err = decoded
					}
				}
			}
		}
		if err == nil {
			err = exchange.Acknowledge()
		}
		serverErr <- err
	}()

	client := Client{Transport: controller, Session: controllerSession}
	reports, err := client.Read(ctx, ConcreteAttributePath(1, 6, 0))
	if err != nil || len(reports) != 1 || !reports[0].Value.Bool {
		t.Fatalf("encrypted Read = %#v, %v", reports, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestClientSubscriptionReceivesPrimingAndUnsolicitedReports(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, device, controllerSession, deviceSession := encryptedPair(t, logger)
	defer controller.Close()
	defer device.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := uint16(1)
	subscriptionID := uint32(77)
	serverErr := make(chan error, 1)
	go func() {
		exchange, err := device.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()
		opcode, requestBytes, err := exchange.Receive(ctx)
		if err == nil && opcode != OpcodeSubscribeRequest {
			err = &unexpectedOpcode{got: opcode, want: OpcodeSubscribeRequest}
		}
		if err == nil {
			request, decodeErr := DecodeSubscribeRequest(requestBytes)
			if decodeErr != nil || len(request.Attributes) != 1 || len(request.Events) != 1 {
				err = errors.Join(decodeErr, fmt.Errorf("unexpected subscription paths"))
			}
		}
		if err == nil {
			priming, encodeErr := EncodeReportDataMessage(&subscriptionID, []AttributeData{{
				Path:  ConcreteAttributePath(endpoint, 6, 0),
				Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutBool(tag, false) },
			}}, nil, false, false)
			err = encodeErr
			if err == nil {
				err = exchange.Send(ctx, OpcodeReportData, priming)
			}
		}
		if err == nil {
			opcode, statusBytes, receiveErr := exchange.Receive(ctx)
			err = receiveErr
			if err == nil && opcode != OpcodeStatusResponse {
				err = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
			}
			if err == nil {
				status, decodeErr := DecodeStatusResponse(statusBytes)
				if decodeErr != nil || !status.OK() {
					err = errors.Join(decodeErr, status)
				}
			}
		}
		if err == nil {
			response, encodeErr := EncodeSubscribeResponse(subscriptionID, 30)
			err = encodeErr
			if err == nil {
				err = exchange.Send(ctx, OpcodeSubscribeResponse, response)
			}
		}
		serverErr <- err
	}()

	client := Client{Transport: controller, Session: controllerSession}
	subscription, err := client.Subscribe(ctx, []AttributePath{ConcreteAttributePath(endpoint, 6, 0)},
		[]EventPath{{Endpoint: &endpoint}}, 0, 300)
	if err != nil || subscription.ID != subscriptionID || subscription.MaxInterval != 30 ||
		len(subscription.Reports) != 1 || subscription.Reports[0].Value.Bool {
		t.Fatalf("Subscribe = %#v, %v", subscription, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}

	cluster, eventID := uint32(0x003B), uint32(1)
	unsolicitedTimestamp := uint64(12345)
	unsolicited, err := EncodeReportDataMessage(&subscriptionID, []AttributeData{{
		Path:  ConcreteAttributePath(endpoint, 6, 0),
		Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutBool(tag, true) },
	}}, []EventData{{
		Path: EventPath{Endpoint: &endpoint, Cluster: &cluster, Event: &eventID}, Number: 99, Priority: 1,
		SystemTimestamp: &unsolicitedTimestamp,
		Value: func(writer *tlv.Writer, tag tlv.Tag) {
			writer.StartStructure(tag)
			writer.EndContainer()
		},
	}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	reportErr := make(chan error, 1)
	go func() {
		exchange, startErr := device.InitiateSecure(deviceSession, message.ProtocolInteractionModel)
		if startErr != nil {
			reportErr <- startErr
			return
		}
		defer exchange.Close()
		if sendErr := exchange.Send(ctx, OpcodeReportData, unsolicited); sendErr != nil {
			reportErr <- sendErr
			return
		}
		opcode, statusBytes, receiveErr := exchange.Receive(ctx)
		if receiveErr == nil && opcode != OpcodeStatusResponse {
			receiveErr = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
		}
		if receiveErr == nil {
			status, decodeErr := DecodeStatusResponse(statusBytes)
			if decodeErr != nil || !status.OK() {
				receiveErr = errors.Join(decodeErr, status)
			}
		}
		reportErr <- receiveErr
	}()
	incoming, err := controller.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ReceiveSubscriptionReport(ctx, incoming, &subscriptionID)
	incoming.Close()
	if err != nil || len(report.Reports) != 1 || !report.Reports[0].Value.Bool ||
		len(report.Events) != 1 || report.Events[0].Number != 99 {
		t.Fatalf("unsolicited report = %#v, %v", report, err)
	}
	if err := <-reportErr; err != nil {
		t.Fatal(err)
	}
}

func TestClientReadCollectsReportChunks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, device, controllerSession, _ := encryptedPair(t, logger)
	defer controller.Close()
	defer device.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		exchange, err := device.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()
		if opcode, _, receiveErr := exchange.Receive(ctx); receiveErr != nil || opcode != OpcodeReadRequest {
			if receiveErr != nil {
				serverErr <- receiveErr
			} else {
				serverErr <- &unexpectedOpcode{got: opcode, want: OpcodeReadRequest}
			}
			return
		}
		first, err := boolReport(1, 6, 0, false, false, true)
		if err == nil {
			err = exchange.Send(ctx, OpcodeReportData, first)
		}
		if err == nil {
			opcode, status, receiveErr := exchange.Receive(ctx)
			err = receiveErr
			if err == nil && opcode != OpcodeStatusResponse {
				err = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
			}
			if err == nil {
				decoded, decodeErr := DecodeStatusResponse(status)
				if decodeErr != nil || !decoded.OK() {
					err = errors.Join(decodeErr, decoded)
				}
			}
		}
		second, encodeErr := boolReport(1, 6, 0, true, false, false)
		if err == nil {
			err = encodeErr
		}
		if err == nil {
			err = exchange.Send(ctx, OpcodeReportData, second)
		}
		if err == nil {
			opcode, status, receiveErr := exchange.Receive(ctx)
			err = receiveErr
			if err == nil && opcode != OpcodeStatusResponse {
				err = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
			}
			if err == nil {
				decoded, decodeErr := DecodeStatusResponse(status)
				if decodeErr != nil || !decoded.OK() {
					err = errors.Join(decodeErr, decoded)
				}
			}
		}
		if err == nil {
			err = exchange.Acknowledge()
		}
		serverErr <- err
	}()

	reports, err := (Client{Transport: controller, Session: controllerSession}).Read(ctx,
		ConcreteAttributePath(1, 6, 0))
	if err != nil || len(reports) != 2 || reports[0].Value.Bool || !reports[1].Value.Bool {
		t.Fatalf("chunked Read = %#v, %v", reports, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestClientTimedInvokeUsesOneExchange(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, device, controllerSession, _ := encryptedPair(t, logger)
	defer controller.Close()
	defer device.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		exchange, err := device.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()
		opcode, timedBytes, err := exchange.Receive(ctx)
		if err == nil && opcode != OpcodeTimedRequest {
			err = &unexpectedOpcode{got: opcode, want: OpcodeTimedRequest}
		}
		if err == nil {
			root, decodeErr := decodeTree(timedBytes)
			timeout, _, fieldErr := uintField(root, 0, true)
			if decodeErr != nil || fieldErr != nil || timeout != 5000 {
				err = errors.Join(decodeErr, fieldErr, fmt.Errorf("TimedRequest timeout = %d", timeout))
			}
		}
		if err == nil {
			var status []byte
			status, err = EncodeStatusResponse(StatusSuccess)
			if err == nil {
				err = exchange.Send(ctx, OpcodeStatusResponse, status)
			}
		}
		var commands []InvokeRequestCommand
		if err == nil {
			opcode, invokeBytes, receiveErr := exchange.Receive(ctx)
			err = receiveErr
			if err == nil && opcode != OpcodeInvokeRequest {
				err = &unexpectedOpcode{got: opcode, want: OpcodeInvokeRequest}
			}
			if err == nil {
				root, decodeErr := decodeTree(invokeBytes)
				timed, ok := root.Field(1)
				if decodeErr != nil || !ok || timed.Type != tlv.TypeBool || !timed.Bool {
					err = errors.Join(decodeErr, errors.New("InvokeRequest is not marked timed"))
				}
				if err == nil {
					commands, err = DecodeInvokeRequest(invokeBytes)
				}
			}
		}
		if err == nil && (len(commands) != 1 || commands[0].Path != (CommandPath{Endpoint: 1, Cluster: 0x101, Command: 0})) {
			err = fmt.Errorf("unexpected timed commands: %#v", commands)
		}
		if err == nil {
			status := Status{Global: StatusSuccess}
			response, encodeErr := EncodeInvokeResponseMessage([]CommandResponse{{Path: commands[0].Path, Status: &status}}, false, false)
			err = encodeErr
			if err == nil {
				err = exchange.Send(ctx, OpcodeInvokeResponse, response)
			}
		}
		serverErr <- err
	}()

	results, err := (Client{Transport: controller, Session: controllerSession}).InvokeTimed(ctx, 5000,
		Command{Path: CommandPath{Endpoint: 1, Cluster: 0x101, Command: 0}})
	if err != nil || len(results) != 1 || !results[0].Status.OK() {
		t.Fatalf("timed Invoke = %#v, %v", results, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestClientInvokeCollectsResponseChunks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller, device, controllerSession, _ := encryptedPair(t, logger)
	defer controller.Close()
	defer device.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paths := []CommandPath{{Endpoint: 1, Cluster: 6, Command: 0}, {Endpoint: 1, Cluster: 6, Command: 1}}
	serverErr := make(chan error, 1)
	go func() {
		exchange, err := device.Accept(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		defer exchange.Close()
		opcode, request, err := exchange.Receive(ctx)
		if err == nil && opcode != OpcodeInvokeRequest {
			err = &unexpectedOpcode{got: opcode, want: OpcodeInvokeRequest}
		}
		if err == nil {
			commands, decodeErr := DecodeInvokeRequest(request)
			if decodeErr != nil || len(commands) != 2 {
				err = errors.Join(decodeErr, fmt.Errorf("invoke commands = %d", len(commands)))
			}
		}
		status := Status{Global: StatusSuccess}
		if err == nil {
			first, encodeErr := EncodeInvokeResponseMessage(
				[]CommandResponse{{Path: paths[0], Status: &status}}, false, true)
			err = encodeErr
			if err == nil {
				err = exchange.Send(ctx, OpcodeInvokeResponse, first)
			}
		}
		if err == nil {
			opcode, statusBytes, receiveErr := exchange.Receive(ctx)
			err = receiveErr
			if err == nil && opcode != OpcodeStatusResponse {
				err = &unexpectedOpcode{got: opcode, want: OpcodeStatusResponse}
			}
			if err == nil {
				decoded, decodeErr := DecodeStatusResponse(statusBytes)
				if decodeErr != nil || !decoded.OK() {
					err = errors.Join(decodeErr, decoded)
				}
			}
		}
		if err == nil {
			second, encodeErr := EncodeInvokeResponseMessage(
				[]CommandResponse{{Path: paths[1], Status: &status}}, false, false)
			err = encodeErr
			if err == nil {
				err = exchange.Send(ctx, OpcodeInvokeResponse, second)
			}
		}
		serverErr <- err
	}()

	results, err := (Client{Transport: controller, Session: controllerSession}).Invoke(ctx,
		Command{Path: paths[0]}, Command{Path: paths[1]})
	if err != nil || len(results) != 2 || results[0].Path != paths[0] || results[1].Path != paths[1] {
		t.Fatalf("chunked Invoke = %#v, %v", results, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func encryptedPair(t *testing.T, logger *slog.Logger) (*transport.Node, *transport.Node, *transport.SecureSession, *transport.SecureSession) {
	t.Helper()
	controller, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	device, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		controller.Close()
		t.Fatal(err)
	}
	i2r := bytes.Repeat([]byte{0x61}, 16)
	r2i := bytes.Repeat([]byte{0x72}, 16)
	controllerSession, err := controller.RegisterSession(transport.SessionConfig{
		LocalID: 11, PeerID: 22, OutboundKey: i2r, InboundKey: r2i, Remote: device.LocalAddr(),
	})
	if err != nil {
		controller.Close()
		device.Close()
		t.Fatal(err)
	}
	deviceSession, err := device.RegisterSession(transport.SessionConfig{
		LocalID: 22, PeerID: 11, OutboundKey: r2i, InboundKey: i2r, Remote: controller.LocalAddr(),
	})
	if err != nil {
		controller.Close()
		device.Close()
		t.Fatal(err)
	}
	return controller, device, controllerSession, deviceSession
}

func boolReport(endpoint uint16, cluster, attribute uint32, value, suppress, more bool) ([]byte, error) {
	return EncodeReportDataMessage(nil, []AttributeData{{
		Path:  ConcreteAttributePath(endpoint, cluster, attribute),
		Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutBool(tag, value) },
	}}, nil, suppress, more)
}

type unexpectedOpcode struct{ got, want uint8 }

func (e *unexpectedOpcode) Error() string { return "unexpected Interaction Model opcode" }

func reportData(t *testing.T, on bool) []byte {
	t.Helper()
	var writer tlv.Writer
	writer.StartStructure(tlv.Anonymous())
	writer.StartArray(tlv.Context(1))
	writer.StartStructure(tlv.Anonymous()) // AttributeReportIB
	writer.StartStructure(tlv.Context(1))  // AttributeDataIB
	writer.PutUint(tlv.Context(0), 7)
	writer.StartList(tlv.Context(1))
	writer.PutUint(tlv.Context(2), 1)
	writer.PutUint(tlv.Context(3), 6)
	writer.PutUint(tlv.Context(4), 0)
	writer.EndContainer()
	writer.PutBool(tlv.Context(2), on)
	writer.EndContainer()
	writer.EndContainer()
	writer.EndContainer()
	writer.PutBool(tlv.Context(4), false)
	writer.PutUint(tlv.Context(0xFF), Revision)
	writer.EndContainer()
	payload, err := writer.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

var _ = message.ProtocolInteractionModel
