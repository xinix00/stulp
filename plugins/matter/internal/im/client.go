package im

import (
	"context"
	"fmt"

	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

type Client struct {
	Transport *transport.Node
	Session   *transport.SecureSession
}

type Subscription struct {
	ID          uint32
	MaxInterval uint16
	Reports     []AttributeReport
	Events      []EventReport
}

// Subscribe establishes one Matter subscription and returns its priming
// report. Later reports arrive on peer-initiated exchanges and are consumed
// with ReceiveSubscriptionReport.
func (c Client) Subscribe(ctx context.Context, attributes []AttributePath, events []EventPath,
	minInterval, maxInterval uint16) (Subscription, error) {
	// Replacing our previous subscriptions prevents duplicate reports after a
	// reconnect; it does not affect subscriptions owned by another fabric.
	payload, err := EncodeSubscribeRequest(attributes, events, minInterval, maxInterval, false, true)
	if err != nil {
		return Subscription{}, err
	}
	exchange, err := c.start(ctx, OpcodeSubscribeRequest, payload)
	if err != nil {
		return Subscription{}, err
	}
	defer exchange.Close()

	var subscription Subscription
	hasSubscriptionID := false
	for {
		opcode, response, err := exchange.Receive(ctx)
		if err != nil {
			return Subscription{}, err
		}
		if opcode == OpcodeStatusResponse {
			_ = exchange.Acknowledge()
			return Subscription{}, responseStatusError(response, OpcodeReportData)
		}
		if opcode != OpcodeReportData {
			_ = exchange.Acknowledge()
			return Subscription{}, fmt.Errorf("expected subscription ReportData, got opcode 0x%02x", opcode)
		}
		report, err := DecodeReportDataMessage(response)
		if err != nil {
			_ = exchange.Acknowledge()
			return Subscription{}, err
		}
		if report.SubscriptionID == nil {
			_ = exchange.Acknowledge()
			return Subscription{}, fmt.Errorf("subscription priming report has no subscription ID")
		}
		if hasSubscriptionID && subscription.ID != *report.SubscriptionID {
			_ = exchange.Acknowledge()
			return Subscription{}, fmt.Errorf("subscription ID changed during priming report")
		}
		subscription.ID = *report.SubscriptionID
		hasSubscriptionID = true
		subscription.Reports = append(subscription.Reports, report.Reports...)
		subscription.Events = append(subscription.Events, report.Events...)
		if !report.SuppressResponse {
			status, encodeErr := EncodeStatusResponse(StatusSuccess)
			if encodeErr != nil {
				return Subscription{}, encodeErr
			}
			if err := exchange.Send(ctx, OpcodeStatusResponse, status); err != nil {
				return Subscription{}, err
			}
		} else if err := exchange.Acknowledge(); err != nil {
			return Subscription{}, err
		}
		if !report.MoreChunkedMessages {
			break
		}
	}

	opcode, response, err := exchange.Receive(ctx)
	if err != nil {
		return Subscription{}, err
	}
	if opcode == OpcodeStatusResponse {
		_ = exchange.Acknowledge()
		return Subscription{}, responseStatusError(response, OpcodeSubscribeResponse)
	}
	if opcode != OpcodeSubscribeResponse {
		_ = exchange.Acknowledge()
		return Subscription{}, fmt.Errorf("expected SubscribeResponse, got opcode 0x%02x", opcode)
	}
	accepted, err := DecodeSubscribeResponse(response)
	if err != nil {
		_ = exchange.Acknowledge()
		return Subscription{}, err
	}
	if accepted.SubscriptionID != subscription.ID {
		_ = exchange.Acknowledge()
		return Subscription{}, fmt.Errorf("SubscribeResponse ID does not match priming report")
	}
	if err := exchange.Acknowledge(); err != nil {
		return Subscription{}, err
	}
	subscription.MaxInterval = accepted.MaxInterval
	return subscription, nil
}

// ReceiveSubscriptionReport consumes every chunk on an unsolicited report
// exchange and sends the Interaction Model status required by the publisher.
func ReceiveSubscriptionReport(ctx context.Context, exchange *transport.Exchange, expectedID *uint32) (ReportDataMessage, error) {
	var combined ReportDataMessage
	for {
		opcode, payload, err := exchange.Receive(ctx)
		if err != nil {
			return ReportDataMessage{}, err
		}
		if opcode != OpcodeReportData {
			_ = exchange.Acknowledge()
			return ReportDataMessage{}, fmt.Errorf("expected unsolicited ReportData, got opcode 0x%02x", opcode)
		}
		report, err := DecodeReportDataMessage(payload)
		if err != nil {
			_ = exchange.Acknowledge()
			return ReportDataMessage{}, err
		}
		if report.SubscriptionID == nil {
			_ = exchange.Acknowledge()
			return ReportDataMessage{}, fmt.Errorf("unsolicited ReportData has no subscription ID")
		}
		if expectedID != nil && *report.SubscriptionID != *expectedID {
			status, encodeErr := EncodeStatusResponse(StatusInvalidSubscription)
			if encodeErr == nil {
				_ = exchange.SendOnce(OpcodeStatusResponse, status)
			}
			return ReportDataMessage{}, fmt.Errorf("unsolicited ReportData has unknown subscription ID %d", *report.SubscriptionID)
		}
		if combined.SubscriptionID != nil && *combined.SubscriptionID != *report.SubscriptionID {
			_ = exchange.Acknowledge()
			return ReportDataMessage{}, fmt.Errorf("subscription ID changed between report chunks")
		}
		id := *report.SubscriptionID
		combined.SubscriptionID = &id
		combined.Reports = append(combined.Reports, report.Reports...)
		combined.Events = append(combined.Events, report.Events...)
		if !report.SuppressResponse {
			status, encodeErr := EncodeStatusResponse(StatusSuccess)
			if encodeErr != nil {
				return ReportDataMessage{}, encodeErr
			}
			if report.MoreChunkedMessages {
				if err := exchange.Send(ctx, OpcodeStatusResponse, status); err != nil {
					return ReportDataMessage{}, err
				}
			} else if err := exchange.SendOnce(OpcodeStatusResponse, status); err != nil {
				return ReportDataMessage{}, err
			}
		} else if err := exchange.Acknowledge(); err != nil {
			return ReportDataMessage{}, err
		}
		if !report.MoreChunkedMessages {
			return combined, nil
		}
	}
}

func (c Client) Read(ctx context.Context, paths ...AttributePath) ([]AttributeReport, error) {
	return c.read(ctx, true, paths)
}

// ReadAcrossFabrics leest zonder fabric-filter. Nodig voor precies één geval:
// de fabric-tabel bekijken over een PASE-sessie. PASE heeft geen eigen fabric,
// dus een gefilterde read van een fabric-scoped lijst komt dan LEEG terug —
// en een heler die daarop vertrouwt vindt de wees nooit (gemeten 20-08: de
// FabricConflict-heling gaf stil op en 0x09 viel door).
func (c Client) ReadAcrossFabrics(ctx context.Context, paths ...AttributePath) ([]AttributeReport, error) {
	return c.read(ctx, false, paths)
}

func (c Client) read(ctx context.Context, fabricFiltered bool, paths []AttributePath) ([]AttributeReport, error) {
	payload, err := EncodeReadRequest(paths, fabricFiltered)
	if err != nil {
		return nil, err
	}
	exchange, err := c.start(ctx, OpcodeReadRequest, payload)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()

	var reports []AttributeReport
	for {
		opcode, response, err := exchange.Receive(ctx)
		if err != nil {
			return nil, err
		}
		if opcode == OpcodeStatusResponse {
			_ = exchange.Acknowledge()
			return nil, responseStatusError(response, OpcodeReportData)
		}
		if opcode != OpcodeReportData {
			_ = exchange.Acknowledge()
			return nil, fmt.Errorf("expected Interaction Model opcode 0x%02x, got 0x%02x", OpcodeReportData, opcode)
		}
		message, err := DecodeReportDataMessage(response)
		if err != nil {
			_ = exchange.Acknowledge()
			return nil, err
		}
		reports = append(reports, message.Reports...)

		// ReportData explicitly asks for an Interaction Model status unless
		// suppressResponse is set. That status also carries the MRP ack. A
		// transport-only ack is insufficient for real Matter servers.
		if !message.SuppressResponse {
			status, err := EncodeStatusResponse(StatusSuccess)
			if err != nil {
				return nil, err
			}
			if err := exchange.Send(ctx, OpcodeStatusResponse, status); err != nil {
				return nil, err
			}
		} else if err := exchange.Acknowledge(); err != nil {
			return nil, err
		}
		if !message.MoreChunkedMessages {
			return reports, nil
		}
	}
}

// Write sends cluster-typed values through the generic Interaction Model.
// Callers must inspect every returned status: Matter can accept one path and
// reject another in the same transaction.
func (c Client) Write(ctx context.Context, writes ...AttributeWrite) ([]AttributeWriteResult, error) {
	payload, err := EncodeWriteRequest(writes, false)
	if err != nil {
		return nil, err
	}
	exchange, err := c.start(ctx, OpcodeWriteRequest, payload)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()
	opcode, response, err := exchange.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if opcode == OpcodeStatusResponse {
		_ = exchange.Acknowledge()
		return nil, responseStatusError(response, OpcodeWriteResponse)
	}
	if opcode != OpcodeWriteResponse {
		_ = exchange.Acknowledge()
		return nil, fmt.Errorf("expected Interaction Model opcode 0x%02x, got 0x%02x", OpcodeWriteResponse, opcode)
	}
	results, err := DecodeWriteResponse(response)
	if err != nil {
		_ = exchange.Acknowledge()
		return nil, err
	}
	if err := exchange.Acknowledge(); err != nil {
		return nil, err
	}
	return results, nil
}

func (c Client) Invoke(ctx context.Context, commands ...Command) ([]InvokeResult, error) {
	payload, err := EncodeInvokeRequest(commands, false)
	if err != nil {
		return nil, err
	}
	exchange, err := c.start(ctx, OpcodeInvokeRequest, payload)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()
	return receiveInvoke(ctx, exchange)
}

// InvokeTimed performs TimedRequest and InvokeRequest on one exchange. Matter
// requires this for commands whose effect must not be replayed after an
// unbounded network delay, notably lock and unlock.
func (c Client) InvokeTimed(ctx context.Context, timeout uint16, commands ...Command) ([]InvokeResult, error) {
	timedRequest, err := EncodeTimedRequest(timeout)
	if err != nil {
		return nil, err
	}
	exchange, err := c.start(ctx, OpcodeTimedRequest, timedRequest)
	if err != nil {
		return nil, err
	}
	defer exchange.Close()
	opcode, response, err := exchange.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if opcode != OpcodeStatusResponse {
		_ = exchange.Acknowledge()
		return nil, fmt.Errorf("expected TimedRequest status, got Interaction Model opcode 0x%02x", opcode)
	}
	status, err := DecodeStatusResponse(response)
	if err != nil {
		_ = exchange.Acknowledge()
		return nil, err
	}
	if !status.OK() {
		_ = exchange.Acknowledge()
		return nil, status
	}
	payload, err := EncodeInvokeRequest(commands, true)
	if err != nil {
		_ = exchange.Acknowledge()
		return nil, err
	}
	// Sending on the same exchange acknowledges the successful status and
	// proves the invoke arrived inside the timed window.
	if err := exchange.Send(ctx, OpcodeInvokeRequest, payload); err != nil {
		return nil, err
	}
	return receiveInvoke(ctx, exchange)
}

func receiveInvoke(ctx context.Context, exchange *transport.Exchange) ([]InvokeResult, error) {
	var results []InvokeResult
	for {
		opcode, response, err := exchange.Receive(ctx)
		if err != nil {
			return nil, err
		}
		if opcode == OpcodeStatusResponse {
			_ = exchange.Acknowledge()
			return nil, responseStatusError(response, OpcodeInvokeResponse)
		}
		if opcode != OpcodeInvokeResponse {
			_ = exchange.Acknowledge()
			return nil, fmt.Errorf("expected Interaction Model opcode 0x%02x, got 0x%02x", OpcodeInvokeResponse, opcode)
		}
		message, err := DecodeInvokeResponseMessage(response)
		if err != nil {
			_ = exchange.Acknowledge()
			return nil, err
		}
		results = append(results, message.Results...)
		if !message.MoreChunkedMessages {
			if err := exchange.Acknowledge(); err != nil {
				return nil, err
			}
			return results, nil
		}
		status, err := EncodeStatusResponse(StatusSuccess)
		if err != nil {
			return nil, err
		}
		if err := exchange.Send(ctx, OpcodeStatusResponse, status); err != nil {
			return nil, err
		}
	}
}

func (c Client) start(ctx context.Context, requestOpcode uint8, payload []byte) (*transport.Exchange, error) {
	if c.Transport == nil || c.Session == nil {
		return nil, fmt.Errorf("Interaction Model client has no secure session")
	}
	exchange, err := c.Transport.InitiateSecure(c.Session, message.ProtocolInteractionModel)
	if err != nil {
		return nil, err
	}
	if err := exchange.Send(ctx, requestOpcode, payload); err != nil {
		exchange.Close()
		return nil, err
	}
	return exchange, nil
}

func responseStatusError(response []byte, expectedOpcode uint8) error {
	status, err := DecodeStatusResponse(response)
	if err != nil {
		return err
	}
	if !status.OK() {
		return status
	}
	return fmt.Errorf("peer returned success StatusResponse instead of opcode 0x%02x", expectedOpcode)
}
