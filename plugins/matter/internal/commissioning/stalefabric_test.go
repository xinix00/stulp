package commissioning

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

// Een half afgemaakt verwijderen laat ónze fabric op het apparaat achter, en
// AddNOC weigert dan met FabricConflict. De heler moet uit de fabric-tabel de
// juiste index vissen -- van ÓNZE fabric, niet die van een ander systeem dat
// er ook op staat.
func TestStaleFabricIndexFindsOurOrphan(t *testing.T) {
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
	controller.RetryInterval = 20 * time.Millisecond
	device.RetryInterval = 20 * time.Millisecond

	i2r := bytes.Repeat([]byte{0x31}, 16)
	r2i := bytes.Repeat([]byte{0x42}, 16)
	controllerSession, err := controller.RegisterSession(transport.SessionConfig{
		LocalID: 0x1001, PeerID: 0x2001, OutboundKey: i2r, InboundKey: r2i, Remote: device.LocalAddr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := device.RegisterSession(transport.SessionConfig{
		LocalID: 0x2001, PeerID: 0x1001, OutboundKey: r2i, InboundKey: i2r, Remote: controller.LocalAddr(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Het nep-apparaat: één ReadRequest, beantwoord met een fabric-tabel met
	// twee bewoners -- een vreemde fabric op index 1, de onze op index 3.
	const ourFabricID = uint64(0xD0D0CACA0001)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- func() error {
			exchange, err := device.Accept(ctx)
			if err != nil {
				return err
			}
			defer exchange.Close()
			opcode, _, err := exchange.Receive(ctx)
			if err != nil {
				return err
			}
			if opcode != im.OpcodeReadRequest {
				t.Errorf("nep-apparaat kreeg opcode %#x, wilde ReadRequest", opcode)
			}
			endpoint := uint16(0)
			cluster := ClusterOperationalCredentials
			attribute := uint32(0x0001)
			fabricEntry := func(writer *tlv.Writer, tag tlv.Tag, fabricID uint64, index uint8) {
				writer.StartStructure(tag)
				writer.PutBytes(tlv.Context(1), bytes.Repeat([]byte{0x11}, 65)) // RootPublicKey
				writer.PutUint(tlv.Context(2), 0xFFF1)                          // VendorID
				writer.PutUint(tlv.Context(3), fabricID)
				writer.PutUint(tlv.Context(4), 0x10019) // NodeID
				writer.PutUintWidth(tlv.Context(254), uint64(index), 1)
				writer.EndContainer()
			}
			report, err := im.EncodeReportDataMessage(nil, []im.AttributeData{{
				Path: im.AttributePath{Endpoint: &endpoint, Cluster: &cluster, Attribute: &attribute},
				Value: func(writer *tlv.Writer, tag tlv.Tag) {
					writer.StartArray(tag)
					fabricEntry(writer, tlv.Anonymous(), 0xBEEF0000BEEF, 1)
					fabricEntry(writer, tlv.Anonymous(), ourFabricID, 3)
					writer.EndContainer()
				},
			}}, nil, true, false)
			if err != nil {
				return err
			}
			return exchange.SendOnce(im.OpcodeReportData, report)
		}()
	}()

	client := Client{IM: im.Client{Transport: controller, Session: controllerSession}}
	index, err := client.StaleFabricIndex(ctx, ourFabricID)
	if err != nil {
		t.Fatal(err)
	}
	if index != 3 {
		t.Fatalf("StaleFabricIndex = %d, wilde 3 (de vreemde fabric op 1 moet blijven staan)", index)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
