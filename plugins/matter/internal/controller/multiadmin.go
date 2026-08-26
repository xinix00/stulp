package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
)

const (
	minimumSharingWindow = 3 * time.Minute
	maximumSharingWindow = 15 * time.Minute
)

// SharingWindow is the user-facing result of native Matter Multi-Admin. A
// Matter node is shared as a whole, not one Stulp endpoint at a time. Devices
// therefore lists the exact scope that the receiving ecosystem will see.
type SharingWindow struct {
	DeviceID      string       `json:"deviceId"`
	NodeID        string       `json:"nodeId"`
	ManualCode    string       `json:"manualCode"`
	QRCode        string       `json:"qrCode"`
	OpenedAt      time.Time    `json:"openedAt"`
	ExpiresAt     time.Time    `json:"expiresAt"`
	Devices       []NodeDevice `json:"devices"`
	WholeBridge   bool         `json:"wholeBridge"`
	Discriminator uint16       `json:"-"`
	Passcode      uint32       `json:"-"`
}

// OpenSharingWindow asks an already commissioned accessory to admit a second
// Matter administrator. This is native Multi-Admin: no proxy device is made
// and the new ecosystem talks directly to the original Matter node.
func (c *Controller) OpenSharingWindow(ctx context.Context, deviceID string, duration time.Duration) (SharingWindow, error) {
	if duration < minimumSharingWindow || duration > maximumSharingWindow {
		return SharingWindow{}, fmt.Errorf("Matter sharing window must be between %s and %s", minimumSharingWindow, maximumSharingWindow)
	}
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return SharingWindow{}, err
	}
	info, err := deviceConnection(device)
	if err != nil {
		return SharingWindow{}, err
	}
	devices, _, err := c.nodeDevices(ctx, info.nodeID)
	if err != nil {
		return SharingWindow{}, err
	}
	parameters, err := commissioning.NewWindowParameters(duration)
	if err != nil {
		return SharingWindow{}, err
	}
	vendorID, productID := nodeVIDPID(devices)
	payload := onboarding.Payload{
		Version: 0, VendorID: vendorID, ProductID: productID, CustomFlow: 0,
		Discovery: 1 << 2, Discriminator: parameters.Discriminator, Passcode: parameters.Passcode,
	}
	manualCode, err := payload.ManualCode()
	if err != nil {
		return SharingWindow{}, err
	}
	qrCode, err := payload.QR()
	if err != nil {
		return SharingWindow{}, err
	}

	session, err := c.session(ctx, info)
	if err != nil {
		return SharingWindow{}, fmt.Errorf("connect before sharing Matter node: %w", err)
	}
	client := commissioning.Client{IM: im.Client{Transport: c.node, Session: session}}
	if err := client.OpenCommissioningWindow(ctx, parameters); err != nil {
		// The CASE session itself may be stale. Do not retry this command: if its
		// response alone got lost, a retry can turn a successfully opened window
		// into an apparently failed Busy response.
		c.expireSession(info.nodeID, session)
		return SharingWindow{}, fmt.Errorf("open Matter sharing window: %w", err)
	}

	opened := time.Now().UTC()
	return SharingWindow{
		DeviceID: deviceID, NodeID: fmt.Sprintf("%016X", info.nodeID),
		ManualCode: manualCode, QRCode: qrCode, OpenedAt: opened,
		ExpiresAt: opened.Add(time.Duration(parameters.Timeout) * time.Second),
		Devices:   nodeDeviceSummaries(devices), WholeBridge: nodeContainsBridgedDevices(devices),
		Discriminator: parameters.Discriminator, Passcode: parameters.Passcode,
	}, nil
}

func (c *Controller) RevokeSharingWindow(ctx context.Context, deviceID string) error {
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	info, err := deviceConnection(device)
	if err != nil {
		return err
	}
	session, err := c.session(ctx, info)
	if err != nil {
		return fmt.Errorf("connect before closing Matter sharing window: %w", err)
	}
	client := commissioning.Client{IM: im.Client{Transport: c.node, Session: session}}
	if err := client.RevokeCommissioning(ctx); err != nil {
		c.expireSession(info.nodeID, session)
		return fmt.Errorf("close Matter sharing window: %w", err)
	}
	return nil
}

func nodeVIDPID(devices []Device) (uint16, uint16) {
	for _, device := range devices {
		vendor, vendorOK := number(device.Data["vendorId"])
		product, productOK := number(device.Data["productId"])
		if vendorOK && productOK && vendor >= 0 && vendor <= math.MaxUint16 && product >= 0 && product <= math.MaxUint16 {
			return uint16(vendor), uint16(product)
		}
	}
	return 0, 0
}

func nodeContainsBridgedDevices(devices []Device) bool {
	for _, device := range devices {
		if bridged, _ := device.Store["matter.bridged"].(bool); bridged {
			return true
		}
	}
	return false
}

func nodeDeviceSummaries(devices []Device) []NodeDevice {
	result := make([]NodeDevice, 0, len(devices))
	for _, device := range devices {
		endpoint, _ := number(device.Store["matter.endpoint"])
		result = append(result, NodeDevice{
			ID: device.ID, Name: device.Name, Endpoint: fmt.Sprintf("%d", uint16(endpoint)),
			Available: device.Available, UnavailableMessage: device.Message,
		})
	}
	return result
}
