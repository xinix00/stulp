package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

func TestExpireSessionOnlyRemovesTheCurrentSession(t *testing.T) {
	node, err := transport.Listen("127.0.0.1:0", discardMatterLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	config := transport.SessionConfig{
		LocalID: 41, PeerID: 42, LocalNodeID: 1, PeerNodeID: 2,
		OutboundKey: bytes.Repeat([]byte{1}, 16), InboundKey: bytes.Repeat([]byte{2}, 16),
		Remote: node.LocalAddr(),
	}
	current, err := node.RegisterSession(config)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = uint64(0x2222)
	controller := &Controller{node: node, sessions: map[uint64]*transport.SecureSession{nodeID: current}}

	controller.expireSession(nodeID, &transport.SecureSession{LocalID: current.LocalID})
	if controller.sessions[nodeID] != current {
		t.Fatal("a stale session handle expired the current CASE session")
	}
	if _, err := node.RegisterSession(config); err == nil {
		t.Fatal("stale expiry also removed the live transport session")
	}

	controller.expireSession(nodeID, current)
	if controller.sessions[nodeID] != nil {
		t.Fatal("current CASE session remained cached after expiry")
	}
	if _, err := node.RegisterSession(config); err != nil {
		t.Fatalf("expired transport session ID could not be reused: %v", err)
	}
}

func TestMarkNodeUnavailableUsesControllerCancellation(t *testing.T) {
	database := newBacking()
	ctx, cancel := context.WithCancel(context.Background())
	controller := &Controller{store: database, ctx: ctx}
	device := addRecoveryMatterDevice(t, database, 0x3333, 1, 7, "127.0.0.1:5540")

	controller.markNodeUnavailable(0x3333, errors.New("router offline"))
	updated, err := database.Device(context.Background(), device.ID)
	if err != nil || updated.Available || !strings.Contains(updated.Message, "router offline") {
		t.Fatalf("node failure was not persisted: device=%#v err=%v", updated, err)
	}

	updated.Available, updated.Message = true, ""
	if err := database.UpdateDevice(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	cancel()
	controller.markNodeUnavailable(0x3333, errors.New("late shutdown error"))
	updated, err = database.Device(context.Background(), device.ID)
	if err != nil || !updated.Available || updated.Message != "" {
		t.Fatalf("canceled controller performed a background write: device=%#v err=%v", updated, err)
	}
}

func TestRejectUnknownSubscriptionReturnsInvalidSubscription(t *testing.T) {
	logger := discardMatterLogger()
	controllerNode, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controllerNode.Close()
	deviceNode, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceNode.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	subscriptionID := uint32(99)
	payload, err := im.EncodeReportDataMessage(&subscriptionID, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err := deviceNode.Initiate(controllerNode.LocalAddr(), message.ProtocolInteractionModel)
	if err != nil {
		t.Fatal(err)
	}
	defer outgoing.Close()
	if err := outgoing.SendOnce(im.OpcodeReportData, payload); err != nil {
		t.Fatal(err)
	}
	incoming, err := controllerNode.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer incoming.Close()
	controller := &Controller{ctx: ctx}
	controller.rejectSubscriptionReport(incoming)

	opcode, response, err := outgoing.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opcode != im.OpcodeStatusResponse {
		t.Fatalf("unknown subscription response opcode = 0x%02X", opcode)
	}
	status, err := im.DecodeStatusResponse(response)
	if err != nil || status.Global != im.StatusInvalidSubscription {
		t.Fatalf("unknown subscription status = %#v, %v", status, err)
	}
}

func TestDeleteDeviceKeepsLocalStateWhenRemoveFabricFails(t *testing.T) {
	logger := discardMatterLogger()
	controllerNode, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer controllerNode.Close()
	deviceNode, err := transport.Listen("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceNode.Close()
	controllerNode.RetryInterval = 20 * time.Millisecond
	deviceNode.RetryInterval = 20 * time.Millisecond

	controllerKey := bytes.Repeat([]byte{0x31}, 16)
	deviceKey := bytes.Repeat([]byte{0x42}, 16)
	const nodeID = uint64(0x4444)
	controllerSession, err := controllerNode.RegisterSession(transport.SessionConfig{
		LocalID: 0x1101, PeerID: 0x2201, LocalNodeID: 1, PeerNodeID: nodeID,
		OutboundKey: controllerKey, InboundKey: deviceKey, Remote: deviceNode.LocalAddr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deviceNode.RegisterSession(transport.SessionConfig{
		LocalID: 0x2201, PeerID: 0x1101, LocalNodeID: nodeID, PeerNodeID: 1,
		OutboundKey: deviceKey, InboundKey: controllerKey, Remote: controllerNode.LocalAddr(),
	}); err != nil {
		t.Fatal(err)
	}
	database := newBacking()
	device := addRecoveryMatterDevice(t, database, nodeID, 1, 7, deviceNode.LocalAddr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	controller := &Controller{
		store: database, node: controllerNode, logger: logger, ctx: ctx,
		sessions: map[uint64]*transport.SecureSession{nodeID: controllerSession},
		workers:  make(map[uint64]context.CancelFunc), subscriptions: make(map[uint64]activeSubscription),
	}

	serverErr := make(chan error, 1)
	go func() {
		exchange, acceptErr := deviceNode.Accept(ctx)
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer exchange.Close()
		opcode, request, receiveErr := exchange.Receive(ctx)
		if receiveErr != nil {
			serverErr <- receiveErr
			return
		}
		if opcode != im.OpcodeInvokeRequest {
			serverErr <- errors.New("RemoveFabric did not use InvokeRequest")
			return
		}
		commands, decodeErr := im.DecodeInvokeRequest(request)
		if decodeErr != nil || len(commands) != 1 {
			serverErr <- errors.Join(decodeErr, errors.New("RemoveFabric request did not contain one command"))
			return
		}
		command := commands[0]
		index, ok := command.Fields.Field(0)
		if command.Path.Endpoint != 0 || command.Path.Cluster != commissioning.ClusterOperationalCredentials ||
			command.Path.Command != commissioning.CommandRemoveFabric || !ok || index.Type != tlv.TypeUint || index.Uint != 7 {
			serverErr <- errors.New("RemoveFabric request targeted the wrong fabric")
			return
		}
		response, encodeErr := im.EncodeInvokeResponseMessage([]im.CommandResponse{{
			Path: im.CommandPath{Endpoint: 0, Cluster: commissioning.ClusterOperationalCredentials, Command: commissioning.CommandNOCResponse},
			Fields: func(writer *tlv.Writer, tag tlv.Tag) {
				writer.StartStructure(tag)
				writer.PutUint(tlv.Context(0), 1)
				writer.PutString(tlv.Context(2), "fabric busy")
				writer.EndContainer()
			},
		}}, false, false)
		if encodeErr == nil {
			encodeErr = exchange.Send(ctx, im.OpcodeInvokeResponse, response)
		}
		serverErr <- encodeErr
	}()

	err = controller.DeleteDevice(ctx, device.ID)
	if err == nil || !strings.Contains(err.Error(), "fabric busy") {
		t.Fatalf("RemoveFabric rejection was not returned: %v", err)
	}
	if serverFailure := <-serverErr; serverFailure != nil {
		t.Fatal(serverFailure)
	}
	if _, err := database.Device(context.Background(), device.ID); err != nil {
		t.Fatalf("local device was deleted after rejected RemoveFabric: %v", err)
	}
	if controller.sessions[nodeID] != nil {
		t.Fatal("failed RemoveFabric retained a suspect CASE session")
	}
}

func TestDeleteDeviceRefusesMissingFabricIndex(t *testing.T) {
	database := newBacking()
	device := addRecoveryMatterDevice(t, database, 0x5555, 1, 0, "127.0.0.1:5540")
	controller := &Controller{store: database, sessions: make(map[uint64]*transport.SecureSession)}
	if err := controller.DeleteDevice(context.Background(), device.ID); err == nil || !strings.Contains(err.Error(), "no fabric index") {
		t.Fatalf("destructive deletion without fabric index was not refused: %v", err)
	}
	if _, err := database.Device(context.Background(), device.ID); err != nil {
		t.Fatalf("refused deletion removed local state: %v", err)
	}
}

func addRecoveryMatterDevice(t *testing.T, database *fakeBacking, nodeID uint64, endpoint uint16, fabricIndex uint8, address string) Device {
	t.Helper()
	device, err := database.AddDevice(context.Background(), Device{
		DriverID: "matter", Name: "Recovery test", Class: "sensor",
		Data: map[string]any{"id": "recovery-test"}, Capabilities: []string{"alarm_contact"},
		State: map[string]any{"alarm_contact": false}, Available: true,
		Store: map[string]any{
			"matter.nodeId":       fmt.Sprintf("%016X", nodeID),
			"matter.endpoint":     endpoint,
			"matter.modelVersion": matterModelVersion,
			"matter.fabricIndex":  fabricIndex,
			"matter.address":      address,
			"matter.noc":          base64.StdEncoding.EncodeToString([]byte{1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return device
}

func discardMatterLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
