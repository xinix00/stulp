// Package controller connects Stulp's device model to its native
// Matter stack. It is an on-network controller; Wi-Fi/Thread network
// provisioning and native radios are intentionally outside its scope.
package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/casesession"
	"github.com/xinix00/stulp/plugins/matter/internal/commissioning"
	"github.com/xinix00/stulp/plugins/matter/internal/credentials"
	"github.com/xinix00/stulp/plugins/matter/internal/discovery"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/onboarding"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const (
	matterModelVersion                       = 3
	descriptorCluster                 uint32 = 0x001D
	basicCluster                      uint32 = 0x0028
	bridgedBasicCluster               uint32 = 0x0039
	onOffCluster                      uint32 = 0x0006
	levelCluster                      uint32 = 0x0008
	doorLockCluster                   uint32 = 0x0101
	thermostatCluster                 uint32 = 0x0201
	temperatureCluster                uint32 = 0x0402
	illuminanceCluster                uint32 = 0x0400
	pressureCluster                   uint32 = 0x0403
	flowCluster                       uint32 = 0x0404
	humidityCluster                   uint32 = 0x0405
	occupancyCluster                  uint32 = 0x0406
	booleanStateCluster               uint32 = 0x0045
	powerSourceCluster                uint32 = 0x002F
	electricalPowerCluster            uint32 = 0x0090
	electricalEnergyCluster           uint32 = 0x0091
	activePowerAttribute              uint32 = 0x0008
	cumulativeEnergyImportedAttribute uint32 = 0x0001
)

type Controller struct {
	store  Backing
	node   *transport.Node
	fabric *credentials.Fabric
	logger *slog.Logger

	// mu zorgt dat er één Matter-uitwisseling tegelijk loopt: commissioneren,
	// een sessie opzetten, een fabric verwijderen. sessionMu bewaakt alleen de
	// map. Dat is bewust apart. Een CASE-handshake duurt seconden en bij het
	// starten gaan er drieëntwintig achter elkaar; wie alleen wil weten óf er
	// een sessie is hoort daar niet achter te wachten. Volgorde als beide nodig
	// zijn: mu eerst, sessionMu daarbinnen, en sessionMu nooit vasthouden over
	// iets dat het netwerk op gaat.
	mu        sync.Mutex
	sessionMu sync.RWMutex
	sessions  map[uint64]*transport.SecureSession
	reportMu  sync.Mutex

	// De node→device-id-index van nodeDevices (subscriptions.go), TTL-vers.
	nodeIdxMu sync.Mutex
	nodeIdx   map[uint64][]string
	nodeIdxAt time.Time

	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	subMu         sync.RWMutex
	workers       map[uint64]context.CancelFunc
	subscriptions map[uint64]activeSubscription
}

type Candidate struct {
	Instance      string `json:"instance"`
	Name          string `json:"name"`
	Address       string `json:"address"`
	Discriminator uint16 `json:"discriminator"`
	VendorID      uint16 `json:"vendorId"`
	ProductID     uint16 `json:"productId"`
}

type CommissionRequest struct {
	Code    string `json:"code"`
	Address string `json:"address"`
}

func New(ctx context.Context, database Backing, logger *slog.Logger) (*Controller, error) {
	if database == nil {
		return nil, errors.New("Matter controller needs a store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := reconcileNativeDevices(ctx, database); err != nil {
		return nil, fmt.Errorf("reconcile native Matter endpoints: %w", err)
	}
	fabric, err := loadOrCreateFabric(ctx, database)
	if err != nil {
		return nil, err
	}
	node, err := transport.Listen(":0", logger)
	if err != nil {
		return nil, err
	}
	controllerContext, cancel := context.WithCancel(context.Background())
	controller := &Controller{
		store: database, node: node, fabric: fabric, logger: logger, sessions: make(map[uint64]*transport.SecureSession),
		ctx: controllerContext, cancel: cancel, workers: make(map[uint64]context.CancelFunc),
		subscriptions: make(map[uint64]activeSubscription),
	}
	controller.wg.Add(2)
	go controller.acceptReports()
	go controller.bootstrapSubscriptions()
	return controller, nil
}

func (c *Controller) Close() error {
	if c == nil || c.node == nil {
		return nil
	}
	c.cancel()
	// Synchronize with startSubscription so no worker can increment the
	// WaitGroup after shutdown has started waiting.
	c.subMu.Lock()
	c.subMu.Unlock()
	err := c.node.Close()
	c.wg.Wait()
	return err
}

func (c *Controller) Discover(ctx context.Context, code string, window time.Duration) ([]Candidate, error) {
	candidates, _, err := c.discover(ctx, code, window)
	return candidates, err
}

// discover returns the candidates that fit the code, and next to them every
// commissionable node the browse saw. Commission needs that second list: "niets
// gevonden" and "wel wat gevonden, maar niet dit" hebben verschillende
// oorzaken en verschillende oplossingen, en een gebruiker die de code van het
// doosje intypt verdient te horen dat het apparaat er niet is in plaats van dat
// de code niet klopt.
func (c *Controller) discover(ctx context.Context, code string, window time.Duration) ([]Candidate, []discovery.Node, error) {
	payload, err := onboarding.Parse(code)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := discovery.Browse(ctx, window)
	if err != nil {
		return nil, nil, err
	}
	open := make([]discovery.Node, 0)
	result := make([]Candidate, 0)
	for _, node := range nodes {
		if node.Kind != "commissionable" || node.CommissioningMode == 0 {
			continue
		}
		open = append(open, node)
		if !matches(payload, node) {
			continue
		}
		addresses := node.Addresses
		if len(addresses) == 0 && node.Host != "" {
			addresses, _ = discovery.ResolveHost(ctx, node.Host, 2*time.Second)
		}
		if len(addresses) == 0 {
			continue
		}
		port := node.Port
		if port == 0 {
			port = transport.Port
		}
		name := strings.TrimSpace(node.DeviceName)
		if name == "" {
			name = node.Instance
		}
		result = append(result, Candidate{
			Instance: node.Instance, Name: name,
			Address:       net.JoinHostPort(addresses[0], strconv.Itoa(int(port))),
			Discriminator: node.Discriminator, VendorID: node.VendorID, ProductID: node.ProductID,
		})
	}
	return result, open, nil
}

func matches(payload onboarding.Payload, node discovery.Node) bool {
	if payload.ShortDiscriminator {
		if payload.Discriminator>>8 != node.Discriminator>>8 {
			return false
		}
	} else if payload.Discriminator != node.Discriminator {
		return false
	}
	if payload.VendorID != 0 && node.VendorID != 0 && payload.VendorID != node.VendorID {
		return false
	}
	return payload.ProductID == 0 || node.ProductID == 0 || payload.ProductID == node.ProductID
}

// Hoe lang Commission op de advertentie wacht, en hoe lang één browse duurt.
//
// Een Thread-apparaat meldt een geopend koppelvenster eerst bij de SRP-server
// van zijn border router, en die republiceert het daarna pas op het LAN. Tussen
// "deel dit apparaat" in het andere systeem en het eerste mDNS-antwoord zit
// daardoor tijd -- gemeten tientallen seconden. Eén browse van vier tellen viel
// daar geregeld voor, en dan zei Stulp dat er niets paste terwijl het apparaat
// een halve minuut later gewoon te zien was. Vandaar: kort blijven kijken tot
// hij verschijnt, in plaats van één keer kijken en het opgeven.
const (
	commissioningBrowseWindow  = 4 * time.Second
	commissioningBrowseTimeout = 60 * time.Second
	commissioningBrowsePause   = time.Second
)

// discoverUntil browst herhaald tot er een kandidaat is of de tijd op is. De
// laatst geziene lijst met open apparaten reist mee, zodat de melding ook na
// een mislukte wachttijd kan zeggen wat er dan wél stond.
func (c *Controller) discoverUntil(ctx context.Context, code string, limit time.Duration) ([]Candidate, []discovery.Node, error) {
	deadline, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var seen []discovery.Node
	for attempt := 0; ; attempt++ {
		candidates, open, err := c.discover(deadline, code, commissioningBrowseWindow)
		if err != nil {
			// Een eerste ronde die niet eens van start komt -- een onleesbare
			// code, geen socket -- is een echte fout. Daarna telt alleen nog of
			// het apparaat verschijnt.
			if attempt == 0 {
				return nil, nil, err
			}
			return nil, seen, nil
		}
		if len(candidates) > 0 {
			return candidates, open, nil
		}
		if len(open) > 0 {
			seen = open
		}
		select {
		case <-deadline.Done():
			return nil, seen, nil
		case <-time.After(commissioningBrowsePause):
		}
	}
}

// noMatchError zegt wat de browse wél zag. Een fabrieksnieuw Wi-Fi-apparaat
// staat nog op geen enkel netwerk en adverteert dus niets: dat is geen fout in
// de code maar de bekende grens van Stulp (docs/matter.md) -- eerst in Apple
// Home erbij, daar een koppelmodus aanzetten, en die code hier plakken.
func noMatchError(payload onboarding.Payload, open []discovery.Node) error {
	if len(open) == 0 {
		return errors.New("er staat geen enkel Matter-apparaat open om te koppelen, ook niet na een minuut wachten. " +
			"Zet de koppelmodus aan in het systeem waar het apparaat nu in zit -- Homey, Apple Home, Google, Alexa -- " +
			"en probeer het daarna opnieuw. Een fabrieksnieuw apparaat staat nog op geen enkel netwerk en is voor " +
			"Stulp onzichtbaar; dat moet eerst ergens anders gekoppeld worden")
	}
	seen := make([]string, 0, len(open))
	for _, node := range open {
		name := strings.TrimSpace(node.DeviceName)
		if name == "" {
			name = node.Instance
		}
		seen = append(seen, fmt.Sprintf("%s (discriminator %d)", name, node.Discriminator))
	}
	wanted := fmt.Sprintf("discriminator %d", payload.Discriminator)
	if payload.ShortDiscriminator {
		// Een handmatige code draagt alleen de bovenste vier bits, dus meer dan
		// "hij hoort hiermee te beginnen" valt er niet over te zeggen.
		wanted = fmt.Sprintf("een discriminator die begint met %d", payload.Discriminator>>8)
	}
	return fmt.Errorf("er staan %d Matter-apparaten open om te koppelen, maar geen met %s: %s. "+
		"Gebruik de koppelcode die het andere systeem nu toont, niet de code op het apparaat zelf",
		len(open), wanted, strings.Join(seen, ", "))
}

// Commission turns one on-network pairing code into one or more Stulp device
// rows (bridges expose multiple endpoints). The method is serialized because
// Matter fail-safe commissioning is stateful and fabric node IDs are issued in
// order.
func (c *Controller) Commission(ctx context.Context, request CommissionRequest) ([]Device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	payload, err := onboarding.Parse(request.Code)
	if err != nil {
		return nil, err
	}
	address := strings.TrimSpace(request.Address)
	if address == "" {
		candidates, open, err := c.discoverUntil(ctx, request.Code, commissioningBrowseTimeout)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, noMatchError(payload, open)
		}
		if len(candidates) > 1 {
			return nil, errors.New("meerdere Matter-apparaten passen bij deze code; kies eerst een apparaat")
		}
		address = candidates[0].Address
	}
	remote, err := resolveRemote(address)
	if err != nil {
		return nil, err
	}

	commissioningContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	exchange, err := c.node.Initiate(remote, message.ProtocolSecureChannel)
	if err != nil {
		return nil, err
	}
	paseResult, err := pase.Commission(commissioningContext, exchange, payload.Passcode)
	exchange.Close()
	if err != nil {
		return nil, fmt.Errorf("Matter PASE: %w", err)
	}
	paseSession, err := c.node.RegisterSession(transport.SessionConfig{
		LocalID: paseResult.LocalSessionID, PeerID: paseResult.PeerSessionID,
		OutboundKey: paseResult.Keys.I2R, InboundKey: paseResult.Keys.R2I, Remote: remote,
	})
	if err != nil {
		return nil, err
	}
	paseActive := true
	defer func() {
		if paseActive {
			c.node.RemoveSession(paseResult.LocalSessionID)
		}
	}()
	commissioner := commissioning.Client{IM: im.Client{Transport: c.node, Session: paseSession}}
	if err := commissioner.ArmFailSafe(commissioningContext, 120, 1); err != nil {
		return nil, fmt.Errorf("arm Matter fail-safe: %w", err)
	}
	if err := commissioner.ConfigureRegulatory(commissioningContext, "XX", 2); err != nil {
		return nil, fmt.Errorf("set Matter regulatory configuration: %w", err)
	}
	attestation, err := commissioner.Attest(commissioningContext, paseResult.Keys.AttestationChallenge,
		payload.VendorID, payload.ProductID)
	if err != nil {
		return nil, fmt.Errorf("verify Matter device attestation: %w", err)
	}
	if payload.VendorID == 0 {
		payload.VendorID = attestation.VendorID
	}
	if payload.ProductID == 0 {
		payload.ProductID = attestation.ProductID
	}
	csr, err := commissioner.CSR(commissioningContext, paseResult.Keys.AttestationChallenge, attestation.DAC)
	if err != nil {
		return nil, err
	}
	publicKey, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("Matter device CSR does not contain a P-256 key")
	}
	rootMatter, err := c.fabric.RootMatterCertificate()
	if err != nil {
		return nil, err
	}
	if err := commissioner.AddTrustedRoot(commissioningContext, rootMatter); err != nil {
		return nil, fmt.Errorf("add Matter trust root: %w", err)
	}
	nodeID, err := c.store.AllocateNodeID(commissioningContext)
	if err != nil {
		return nil, err
	}
	nocCertificate, err := c.fabric.SignNode(publicKey, nodeID, time.Now())
	if err != nil {
		return nil, err
	}
	nocMatter, err := credentials.MatterCertificate(nocCertificate)
	if err != nil {
		return nil, err
	}
	fabricIndex, err := commissioner.AddNOC(commissioningContext, nocMatter, nil, c.fabric.IPK,
		c.fabric.ControllerNodeID, credentials.TestVendorID)
	if err != nil {
		// FabricConflict: het apparaat draagt ÓNZE fabric nog. Dat is de
		// vingerafdruk van een half afgemaakt verwijderen — Stulp is de rij
		// kwijt (dat beleid is bewust: een mislukte opruiming mag verwijderen
		// niet blokkeren, zie plugin.DeleteDevice) terwijl RemoveFabric het
		// apparaat nooit bereikte. De gebruiker die nu opnieuw koppelt ís de
		// opruimopdracht: haal de wees weg en probeer één keer opnieuw, in
		// plaats van hem naar een fabrieksreset te sturen.
		var conflict commissioning.CommissioningError
		if errors.As(err, &conflict) && conflict.Status == commissioning.StatusFabricConflict {
			if staleIndex, findErr := commissioner.StaleFabricIndex(commissioningContext, c.fabric.ID); findErr == nil {
				if removeErr := commissioner.RemoveFabric(commissioningContext, staleIndex); removeErr == nil {
					fabricIndex, err = commissioner.AddNOC(commissioningContext, nocMatter, nil, c.fabric.IPK,
						c.fabric.ControllerNodeID, credentials.TestVendorID)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("add Matter NOC: %w", err)
		}
	}
	c.node.RemoveSession(paseResult.LocalSessionID)
	paseActive = false

	caseSession, operationalRemote, err := c.establishAfterCommissioning(commissioningContext, remote, nodeID, nocMatter)
	if err != nil {
		return nil, fmt.Errorf("Matter CASE after AddNOC: %w", err)
	}
	c.storeSession(nodeID, caseSession)
	commissioned := false
	defer func() {
		if !commissioned {
			c.node.RemoveSession(caseSession.LocalID)
			c.dropSession(nodeID)
		}
	}()
	client := im.Client{Transport: c.node, Session: caseSession}
	prototypes, err := inspectNode(commissioningContext, client, payload, operationalRemote, nodeID, fabricIndex, nocMatter)
	if err != nil {
		return nil, fmt.Errorf("inspect commissioned Matter node: %w", err)
	}
	caseCommissioner := commissioning.Client{IM: client}
	if err := caseCommissioner.Complete(commissioningContext); err != nil {
		return nil, fmt.Errorf("complete Matter commissioning: %w", err)
	}
	commissioned = true
	c.startSubscription(nodeID)
	// De apparaten worden hier niet bewaard. Wat er gevonden is gaat terug naar
	// de aanroeper, en die bepaalt of het blijft: bij het koppelen kiest de
	// gebruiker, en dat is niet aan een app. Wat een node moet onthouden staat
	// in zijn data, dus een bewaard exemplaar is later terug te vinden.
	return prototypes, nil
}

func (c *Controller) establishAfterCommissioning(ctx context.Context, initial *net.UDPAddr, nodeID uint64,
	noc []byte) (*transport.SecureSession, *net.UDPAddr, error) {
	quick, cancel := context.WithTimeout(ctx, 12*time.Second)
	session, err := casesession.Establish(quick, c.node, initial, c.fabric, nodeID, noc)
	cancel()
	if err == nil {
		return session, initial, nil
	}
	compressed, compressedErr := c.fabric.CompressedID()
	if compressedErr != nil {
		return nil, nil, err
	}
	nodes, browseErr := discovery.Browse(ctx, 5*time.Second)
	if browseErr != nil {
		return nil, nil, fmt.Errorf("initial address: %v; operational discovery: %w", err, browseErr)
	}
	wantFabric := strings.ToUpper(fmt.Sprintf("%X", compressed[:]))
	wantNode := strings.ToUpper(fmt.Sprintf("%016X", nodeID))
	for _, candidate := range nodes {
		if candidate.Kind != "operational" || !strings.EqualFold(candidate.CompressedFabricID, wantFabric) ||
			!strings.EqualFold(candidate.NodeID, wantNode) {
			continue
		}
		addresses := candidate.Addresses
		if len(addresses) == 0 && candidate.Host != "" {
			addresses, _ = discovery.ResolveHost(ctx, candidate.Host, 2*time.Second)
		}
		if len(addresses) == 0 {
			continue
		}
		port := candidate.Port
		if port == 0 {
			port = transport.Port
		}
		remote, resolveErr := resolveRemote(net.JoinHostPort(addresses[0], strconv.Itoa(int(port))))
		if resolveErr != nil {
			continue
		}
		session, caseErr := casesession.EstablishWithRetry(
			ctx, c.node, remote, c.fabric, nodeID, noc, advertisedMRPTiming(candidate.Text),
		)
		if caseErr == nil {
			return session, remote, nil
		}
	}
	return nil, nil, fmt.Errorf("initial CASE failed: %w; operational advertisement not reachable", err)
}

func (c *Controller) SetCapability(ctx context.Context, deviceID, capability string, value any) error {
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	info, err := deviceConnection(device)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		session, sessionErr := c.session(ctx, info)
		if sessionErr != nil {
			return sessionErr
		}
		client := im.Client{Transport: c.node, Session: session}
		endpoint := capabilityEndpoint(device, capability, info.endpoint)
		err = invokeCapability(ctx, client, endpoint, device, capability, value)
		if err == nil {
			c.reportMu.Lock()
			latest, readErr := c.store.Device(ctx, device.ID)
			if readErr == nil {
				if latest.State == nil {
					latest.State = make(map[string]any)
				}
				latest.State[capability] = normalizedCapability(capability, value)
				latest.Available, latest.Message = true, ""
				readErr = c.store.UpdateDevice(ctx, latest)
			}
			c.reportMu.Unlock()
			return readErr
		}
		c.node.RemoveSession(session.LocalID)
		c.dropSession(info.nodeID)
	}
	c.reportMu.Lock()
	if latest, readErr := c.store.Device(c.ctx, device.ID); readErr == nil {
		latest.Available, latest.Message = false, err.Error()
		_ = c.store.UpdateDevice(c.ctx, latest)
	}
	c.reportMu.Unlock()
	return err
}

func (c *Controller) DeleteDevice(ctx context.Context, deviceID string) error {
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	info, infoErr := deviceConnection(device)
	if infoErr != nil {
		return c.store.DeleteDevice(ctx, deviceID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	devices, err := c.store.Devices(ctx)
	if err != nil {
		return err
	}
	for _, remaining := range devices {
		if remaining.ID == deviceID {
			continue
		}
		remainingInfo, parseErr := deviceConnection(remaining)
		if parseErr == nil && remainingInfo.nodeID == info.nodeID {
			return c.store.DeleteDevice(ctx, deviceID)
		}
	}
	if info.fabricIndex == 0 {
		return errors.New("Matter device has no fabric index; refusing to leave an orphaned fabric on the accessory")
	}
	session, err := c.session(ctx, info)
	if err != nil {
		return fmt.Errorf("connect before removing Matter fabric: %w", err)
	}
	commissioner := commissioning.Client{IM: im.Client{Transport: c.node, Session: session}}
	if err := commissioner.RemoveFabric(ctx, info.fabricIndex); err != nil {
		c.node.RemoveSession(session.LocalID)
		c.dropSession(info.nodeID)
		return fmt.Errorf("remove Matter fabric from accessory: %w", err)
	}
	c.node.RemoveSession(session.LocalID)
	c.dropSession(info.nodeID)
	c.stopSubscription(info.nodeID)
	return c.store.DeleteDevice(ctx, deviceID)
}

func invokeCapability(ctx context.Context, client im.Client, endpoint uint16, device Device, capability string, value any) error {
	deviceTypes := storedEndpointDeviceTypes(device, endpoint)
	servers := storedMatterIDs(device.Store["matter.serverClusters"])
	command, timed, err := commandForCapability(deviceTypes, servers, endpoint, capability, value)
	if err != nil {
		return err
	}
	var results []im.InvokeResult
	if timed {
		results, err = client.InvokeTimed(ctx, 5000, command)
	} else {
		results, err = client.Invoke(ctx, command)
	}
	if err != nil {
		return err
	}
	if len(results) != 1 || !results[0].Status.OK() {
		return errors.New("Matter command was not accepted")
	}
	return nil
}

func normalizedCapability(capability string, value any) any {
	switch baseCapability(capability) {
	case "dim", "light_hue", "light_saturation":
		if number, ok := number(value); ok {
			return number
		}
	}
	return value
}

type connectionInfo struct {
	nodeID      uint64
	endpoint    uint16
	fabricIndex uint8
	remote      *net.UDPAddr
	noc         []byte
	timing      transport.MRPTiming
}

func deviceConnection(device Device) (connectionInfo, error) {
	nodeText, _ := device.Store["matter.nodeId"].(string)
	nodeID, err := strconv.ParseUint(nodeText, 16, 64)
	if err != nil || nodeID == 0 {
		return connectionInfo{}, errors.New("Matter device has no valid node ID")
	}
	endpoint, ok := number(device.Store["matter.endpoint"])
	if !ok || endpoint < 0 || endpoint > math.MaxUint16 {
		return connectionInfo{}, errors.New("Matter device has no valid endpoint")
	}
	address, _ := device.Store["matter.address"].(string)
	remote, err := resolveRemote(address)
	if err != nil {
		return connectionInfo{}, err
	}
	encodedNOC, _ := device.Store["matter.noc"].(string)
	noc, err := base64.StdEncoding.DecodeString(encodedNOC)
	if err != nil || len(noc) == 0 {
		return connectionInfo{}, errors.New("Matter device has no operational certificate")
	}
	var fabricIndex uint8
	if value, ok := number(device.Store["matter.fabricIndex"]); ok && value > 0 && value <= math.MaxUint8 {
		fabricIndex = uint8(value)
	}
	return connectionInfo{
		nodeID: nodeID, endpoint: uint16(endpoint), fabricIndex: fabricIndex,
		remote: remote, noc: noc, timing: storedMRPTiming(device.Store),
	}, nil
}

func (c *Controller) session(ctx context.Context, info connectionInfo) (*transport.SecureSession, error) {
	if existing := c.lookupSession(info.nodeID); existing != nil {
		return existing, nil
	}
	session, err := casesession.EstablishWithRetry(ctx, c.node, info.remote, c.fabric, info.nodeID, info.noc, info.timing)
	if err != nil {
		return nil, err
	}
	c.storeSession(info.nodeID, session)
	return session, nil
}

func (c *Controller) lookupSession(nodeID uint64) *transport.SecureSession {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.sessions[nodeID]
}

func (c *Controller) storeSession(nodeID uint64, session *transport.SecureSession) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.sessions[nodeID] = session
}

func (c *Controller) dropSession(nodeID uint64) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	delete(c.sessions, nodeID)
}

// dropSessionIf haalt de sessie alleen weg als het nog dezelfde is. Tussen het
// mislukken van een uitwisseling en het opruimen kan er al een nieuwe sessie
// staan, en die hoort niet het slachtoffer te worden van de oude.
func (c *Controller) dropSessionIf(nodeID uint64, session *transport.SecureSession) bool {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.sessions[nodeID] != session {
		return false
	}
	delete(c.sessions, nodeID)
	return true
}

// storedMRPTiming reads what commissioning learned about this peer. It is the
// only source on a HopOS node: there is no multicast there, so operational
// DNS-SD cannot be re-browsed and these values are what the fabric knows.
func storedMRPTiming(values map[string]any) transport.MRPTiming {
	return transport.MRPTiming{
		Idle:            storedInterval(values, "matter.mrpIdleInterval"),
		Active:          storedInterval(values, "matter.mrpActiveInterval"),
		ActiveThreshold: storedInterval(values, "matter.mrpActiveThreshold"),
	}
}

// storedInterval refuses anything above a minute. An interval is a promise about
// how long we wait before deciding a device is gone, and a bad record must not be
// able to turn that into a quarter of an hour.
func storedInterval(values map[string]any, key string) time.Duration {
	milliseconds, ok := number(values[key])
	if !ok || milliseconds <= 0 || milliseconds > 60_000 {
		return 0
	}
	return time.Duration(milliseconds * float64(time.Millisecond))
}

// advertisedMRPTiming reads the same three values straight from a DNS-SD record,
// for the one moment we have one: commissioning, on a host with multicast.
func advertisedMRPTiming(text map[string]string) transport.MRPTiming {
	interval := func(key string) time.Duration {
		if milliseconds, ok := mrpTXTMilliseconds(text, key); ok && milliseconds <= 60_000 {
			return time.Duration(milliseconds) * time.Millisecond
		}
		return 0
	}
	return transport.MRPTiming{Idle: interval("SII"), Active: interval("SAI"), ActiveThreshold: interval("SAT")}
}

func copyMRPTXT(destination map[string]any, text map[string]string) {
	for source, target := range map[string]string{
		"SII": "matter.mrpIdleInterval", "SAI": "matter.mrpActiveInterval", "SAT": "matter.mrpActiveThreshold",
	} {
		if milliseconds, ok := mrpTXTMilliseconds(text, source); ok {
			destination[target] = milliseconds
		}
	}
}

func mrpTXTMilliseconds(text map[string]string, wanted string) (int64, bool) {
	for key, raw := range text {
		if !strings.EqualFold(key, wanted) {
			continue
		}
		milliseconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		return milliseconds, err == nil && milliseconds > 0
	}
	return 0, false
}

func resolveRemote(address string) (*net.UDPAddr, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, errors.New("Matter device address is required")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, strconv.Itoa(transport.Port))
	}
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve Matter address %q: %w", address, err)
	}
	return remote, nil
}

func loadOrCreateFabric(ctx context.Context, database Backing) (*credentials.Fabric, error) {
	record, exists, err := database.Fabric(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		rootKey, err := credentials.ParsePrivateKey(record.RootKeyDER)
		if err != nil {
			return nil, fmt.Errorf("load Matter root key: %w", err)
		}
		controllerKey, err := credentials.ParsePrivateKey(record.ControllerKeyDER)
		if err != nil {
			return nil, fmt.Errorf("load Matter controller key: %w", err)
		}
		rootCertificate, err := x509.ParseCertificate(record.RootCertDER)
		if err != nil {
			return nil, fmt.Errorf("load Matter root certificate: %w", err)
		}
		controllerCertificate, err := x509.ParseCertificate(record.ControllerCertDER)
		if err != nil {
			return nil, fmt.Errorf("load Matter controller certificate: %w", err)
		}
		fabric := &credentials.Fabric{
			ID: record.FabricID, RootID: record.RootID, ControllerNodeID: record.ControllerNodeID,
			IPK: record.IPK, RootKey: rootKey, RootCertificate: rootCertificate,
			ControllerKey: controllerKey, ControllerNOC: controllerCertificate,
		}
		return fabric, fabric.Validate()
	}
	fabricID, err := randomPositiveID()
	if err != nil {
		return nil, err
	}
	rootID, err := randomPositiveID()
	if err != nil {
		return nil, err
	}
	controllerID, err := randomPositiveID()
	if err != nil {
		return nil, err
	}
	fabric, err := credentials.NewFabric(fabricID, rootID, controllerID, time.Now())
	if err != nil {
		return nil, err
	}
	rootKey, _ := credentials.MarshalPrivateKey(fabric.RootKey)
	controllerKey, _ := credentials.MarshalPrivateKey(fabric.ControllerKey)
	if err := database.SaveFabric(ctx, FabricRecord{
		FabricID: fabric.ID, RootID: fabric.RootID, ControllerNodeID: fabric.ControllerNodeID, IPK: fabric.IPK,
		RootKeyDER: rootKey, RootCertDER: fabric.RootCertificate.Raw,
		ControllerKeyDER: controllerKey, ControllerCertDER: fabric.ControllerNOC.Raw,
		NextNodeID: 0x10000,
	}); err != nil {
		return nil, err
	}
	return fabric, nil
}

func randomPositiveID() (uint64, error) {
	for {
		var encoded [8]byte
		if _, err := rand.Read(encoded[:]); err != nil {
			return 0, err
		}
		value := binary.LittleEndian.Uint64(encoded[:]) & math.MaxInt64
		if value != 0 {
			return value, nil
		}
	}
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
	}
}

func uniqueUint16(values []uint16) []uint16 {
	slices.Sort(values)
	return slices.Compact(values)
}
