package main

// De Sigenergy Gateway als apparaat.
//
// Lezen en schakelen gaan hier via de optionele mySigen-koppeling. De gewone
// energiemetingen blijven lokaal over Modbus lopen; zo maakt een cloudstoring de
// rest van het systeem niet grijs. `off_grid` is een normale boolean en krijgt
// daardoor vanzelf Flow-triggers, een conditie, een actie en Scene-ondersteuning.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/mysigen"
)

const (
	gatewayPollInterval = 30 * time.Second
	gatewayReadTimeout  = 15 * time.Second
	gatewaySwitchTime   = 70 * time.Second
)

type gatewayDriver struct{}

type gatewayHandle interface {
	ID() string
	SetCapabilityValues(map[string]any) error
	SetAvailable() error
	SetUnavailable(string) error
	Error(string)
}

type gatewayDevice struct {
	device    gatewayHandle
	stationID int64
	// Een test kan één concrete client vastzetten. In productie blijft dit nil
	// en wordt de actuele app-koppeling gebruikt, zodat opnieuw koppelen geen
	// device-herstart vraagt.
	cloud cloudClient

	mu        sync.Mutex
	cancel    context.CancelFunc
	command   context.CancelFunc
	switching bool
}

func (gatewayDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	stationID, err := stationIDOf(device.Data()["stationId"])
	if err != nil {
		return nil, fmt.Errorf("deze Gateway heeft geen geldig mySigen-station-id; koppel hem opnieuw: %w", err)
	}
	return &gatewayDevice{device: device, stationID: stationID}, nil
}

func stationIDOf(value any) (int64, error) {
	switch id := value.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		if err == nil && parsed > 0 {
			return parsed, nil
		}
	case float64:
		parsed := int64(id)
		if id == float64(parsed) && parsed > 0 {
			return parsed, nil
		}
	case int64:
		if id > 0 {
			return id, nil
		}
	case int:
		if id > 0 {
			return int64(id), nil
		}
	}
	return 0, fmt.Errorf("ongeldige waarde %v", value)
}

// ListDevices toont stations waarvoor mySigen een echte Gateway-netstand
// terugstuurt. showButton en ControlMode bepalen alleen of een eigenaar op dat
// moment handmatig mag schakelen; ze mogen een aanwezige noodstroom-Gateway niet
// onzichtbaar maken. OnCapability doet vóór ieder commando nog steeds de strenge
// bedienings-preflight.
func (gatewayDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	client, err := instance.apiCloud()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudCallTimeout)
	defer cancel()
	stations, err := client.Stations(ctx)
	if err != nil {
		return nil, err
	}
	found := make([]appsdk.PairedDevice, 0, len(stations.Stations))
	var firstFailure error
	for _, station := range stations.Stations {
		status, err := client.GatewayStatus(ctx, station.ID)
		if err != nil {
			if firstFailure == nil {
				firstFailure = err
			}
			continue
		}
		if !status.KnownGridStatus() {
			continue
		}
		name := strings.TrimSpace(station.Name)
		if name == "" {
			name = "Sigenergy"
		}
		found = append(found, appsdk.PairedDevice{
			Name: name + " Gateway",
			// Als tekst bewaren: JSON-getallen zijn float64 en een cloud-id hoeft
			// niet afhankelijk te zijn van diens exacte gehele bereik.
			Data: map[string]any{"stationId": strconv.FormatInt(station.ID, 10)},
			Store: map[string]any{
				"stationName":  station.Name,
				"manufacturer": "Sigenergy",
			},
		})
	}
	if len(found) == 0 {
		if firstFailure != nil {
			return nil, fmt.Errorf("geen Sigenergy Gateway-status gevonden: %w", firstFailure)
		}
		return nil, fmt.Errorf("mySigen gaf voor geen station een herkenbare Gateway-netstand terug")
	}
	return found, nil
}

func (g *gatewayDevice) OnInit() error {
	ctx, cancel := context.WithCancel(context.Background())
	g.mu.Lock()
	g.cancel = cancel
	g.mu.Unlock()
	instance.watchGateway(g.device.ID(), g)
	go g.run(ctx)
	return nil
}

func (g *gatewayDevice) OnDeleted() {
	instance.forgetGateway(g.device.ID())
	g.halt()
}

func (g *gatewayDevice) halt() {
	g.mu.Lock()
	cancel, command := g.cancel, g.command
	g.cancel = nil
	g.command = nil
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if command != nil {
		command()
	}
}

func (g *gatewayDevice) cancelCommand() {
	g.mu.Lock()
	cancel := g.command
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (g *gatewayDevice) run(ctx context.Context) {
	g.refresh(ctx)
	ticker := time.NewTicker(gatewayPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.refresh(ctx)
		}
	}
}

func (g *gatewayDevice) activeCloud() (cloudClient, error) {
	if g.cloud != nil {
		return g.cloud, nil
	}
	return instance.apiCloud()
}

func (g *gatewayDevice) refresh(parent context.Context) {
	client, err := g.activeCloud()
	if err != nil {
		_ = g.device.SetUnavailable(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(parent, gatewayReadTimeout)
	defer cancel()
	status, err := client.GatewayStatus(ctx, g.stationID)
	if err != nil {
		_ = g.device.SetUnavailable(err.Error())
		return
	}
	if err := g.apply(status); err != nil {
		g.device.Error(err.Error())
	}
}

func gatewayGridStatus(status mysigen.GatewayStatus) string {
	switch status.OnOffGridStatus {
	case mysigen.StatusOnGrid:
		return "on_grid"
	case mysigen.StatusAutomaticIsland:
		return "off_grid_automatic"
	case mysigen.StatusManualIsland:
		return "off_grid_manual"
	case mysigen.StatusGeneratorGrid:
		return "generator_grid"
	default:
		return fmt.Sprintf("unknown_%d", status.OnOffGridStatus)
	}
}

func (g *gatewayDevice) apply(status mysigen.GatewayStatus) error {
	if err := g.device.SetCapabilityValues(map[string]any{
		"off_grid":    status.OffGrid(),
		"grid_status": gatewayGridStatus(status),
	}); err != nil {
		return err
	}
	return g.device.SetAvailable()
}

// OnCapability doet de volledige read-only preflight voordat het commando wordt
// aangenomen. De POST en begrensde readback lopen daarna buiten de lifecycle-
// worker: een Gateway kan dertig seconden omschakelen en mag in die tijd niet
// alle andere apparaten van deze plugin blokkeren.
func (g *gatewayDevice) OnCapability(name string, value any) error {
	if name != "off_grid" {
		return fmt.Errorf("deze Gateway kent capability %q niet", name)
	}
	offGrid, ok := value.(bool)
	if !ok {
		return errors.New("de noodstroomstand moet true of false zijn")
	}
	g.mu.Lock()
	if g.switching {
		g.mu.Unlock()
		return mysigen.ErrTransitionBusy
	}
	g.mu.Unlock()

	client, err := g.activeCloud()
	if err != nil {
		return err
	}
	target := mysigen.TargetOnGrid
	if offGrid {
		target = mysigen.TargetOffGrid
	}
	ctx, cancel := context.WithTimeout(context.Background(), gatewayReadTimeout)
	defer cancel()
	prepared, err := client.PrepareSwitch(ctx, g.stationID, target)
	if err != nil {
		return err
	}
	if prepared.AlreadyReached {
		return g.apply(prepared.Current)
	}
	if prepared.Confirmation == "" {
		return fmt.Errorf("mySigen gaf geen bevestiging voor de Gateway-schakeling")
	}

	g.mu.Lock()
	if g.switching {
		g.mu.Unlock()
		return mysigen.ErrTransitionBusy
	}
	g.switching = true
	commandContext, commandCancel := context.WithTimeout(context.Background(), gatewaySwitchTime)
	g.command = commandCancel
	g.mu.Unlock()
	go g.execute(commandContext, client, prepared.Confirmation)
	return nil
}

func (g *gatewayDevice) execute(ctx context.Context, client cloudClient, confirmation string) {
	defer func() {
		g.mu.Lock()
		if g.command != nil {
			g.command()
		}
		g.command = nil
		g.switching = false
		g.mu.Unlock()
	}()
	result, err := client.ExecuteSwitch(ctx, confirmation, mysigen.PollOptions{})
	// OnStop/OnDeleted heeft het appsdk-handle vanaf nu niet meer nodig en kan
	// de processessie al gesloten hebben. Een geannuleerde opdracht schrijft
	// daarom niets meer terug; de volgende generatie leest zelf opnieuw.
	if ctx.Err() != nil {
		return
	}
	if (err == nil || result.CommandSent) && result.Status.OnOffGridStatus >= 0 {
		if applyErr := g.apply(result.Status); applyErr != nil {
			g.device.Error(applyErr.Error())
		}
	}
	if err != nil {
		g.device.Error(err.Error())
		// Een onzekere schrijfuitkomst wordt nooit opnieuw verstuurd. Eén extra
		// gewone statuslezing mag wel; die projecteert wat de Gateway werkelijk
		// deed zonder een tweede schakeling te riskeren.
		g.refresh(context.Background())
	}
}

func (a *app) watchGateway(deviceID string, gateway *gatewayDevice) {
	a.mu.Lock()
	a.gateways[deviceID] = gateway
	a.mu.Unlock()
}

func (a *app) forgetGateway(deviceID string) {
	a.mu.Lock()
	delete(a.gateways, deviceID)
	a.mu.Unlock()
}

func (a *app) refreshGateways() {
	a.mu.RLock()
	gateways := make([]*gatewayDevice, 0, len(a.gateways))
	for _, gateway := range a.gateways {
		gateways = append(gateways, gateway)
	}
	a.mu.RUnlock()
	for _, gateway := range gateways {
		go gateway.refresh(context.Background())
	}
}

var (
	_ appsdk.Pairer            = gatewayDriver{}
	_ appsdk.DeviceHandler     = (*gatewayDevice)(nil)
	_ appsdk.CapabilityHandler = (*gatewayDevice)(nil)
	_ appsdk.Deleter           = (*gatewayDevice)(nil)
)
