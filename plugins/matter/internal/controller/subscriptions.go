package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/stulp/plugins/matter/internal/discovery"
	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/message"
	"github.com/xinix00/stulp/plugins/matter/internal/pase"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
	"github.com/xinix00/stulp/plugins/matter/internal/transport"
)

const (
	switchCluster uint32 = 0x003B
	// 1s: bij 0 mag een spraakzame publisher (energiemeters!) onbeperkt vaak
	// rapporteren, en elk rapport kost hier decrypt+TLV+store+SSE. Eén seconde
	// batcht dat aan de brón zonder merkbare vertraging voor een dashboard;
	// events (knoppen) blijven de spec volgen en reizen met het eerstvolgende
	// rapport mee (CPU-ronde 19-08: matter was ~6% van de bundel).
	subscriptionMinInterval = 1
	subscriptionMaxInterval = 300
)

var errMatterNodeGone = errors.New("Matter node has no local endpoints")

// errSubscriptionLapsed is de wachthond die afging: de publisher miste zijn
// rapportagevenster. Een eigen fout en geen tekst, want de hersteld-lus
// behandelt hem anders dan echt falen — zie de speling daar.
var errSubscriptionLapsed = errors.New("Matter subscription exceeded its maximum reporting interval")

type activeSubscription struct {
	id       uint32
	activity chan struct{}
}

func (c *Controller) bootstrapSubscriptions() {
	defer c.wg.Done()
	devices, err := c.store.Devices(c.ctx)
	if err != nil {
		if c.ctx.Err() == nil {
			c.logger.Warn("cannot load Matter devices for subscriptions", "error", err)
		}
		return
	}
	if refreshed, refreshErr := c.refreshOperationalConnections(c.ctx, devices); refreshErr != nil {
		if c.ctx.Err() == nil {
			c.logger.Debug("Matter operational discovery refresh failed", "error", refreshErr)
		}
	} else {
		devices = refreshed
	}
	seen := make(map[uint64]bool)
	for _, device := range devices {
		info, err := deviceConnection(device)
		if err == nil && !seen[info.nodeID] {
			seen[info.nodeID] = true
			c.startSubscription(info.nodeID)
		}
	}
}

// refreshOperationalConnections learns the peer's current address and MRP
// timing before the first CASE after startup. Sleepy Thread devices advertise
// SII/SAI in DNS-SD and can legitimately ignore a Sigma1 sent on the generic
// 500 ms cadence.
func (c *Controller) refreshOperationalConnections(ctx context.Context, devices []Device) ([]Device, error) {
	wanted := make(map[string][]int)
	for index, device := range devices {
		info, err := deviceConnection(device)
		if err != nil || info.remote.IP.IsLoopback() {
			continue
		}
		wanted[strings.ToUpper(fmt.Sprintf("%016X", info.nodeID))] = append(wanted[strings.ToUpper(fmt.Sprintf("%016X", info.nodeID))], index)
	}
	if len(wanted) == 0 {
		return devices, nil
	}
	compressed, err := c.fabric.CompressedID()
	if err != nil {
		return devices, err
	}
	wantFabric := strings.ToUpper(fmt.Sprintf("%X", compressed[:]))
	nodes, err := discovery.Browse(ctx, 4*time.Second)
	if err != nil {
		return devices, err
	}
	for _, candidate := range nodes {
		indices := wanted[strings.ToUpper(candidate.NodeID)]
		if candidate.Kind != "operational" || !strings.EqualFold(candidate.CompressedFabricID, wantFabric) || len(indices) == 0 {
			continue
		}
		addresses := candidate.Addresses
		if len(addresses) == 0 && candidate.Host != "" {
			addresses, _ = discovery.ResolveHost(ctx, candidate.Host, 2*time.Second)
		}
		for _, index := range indices {
			device := devices[index]
			if device.Store == nil {
				device.Store = make(map[string]any)
			}
			if len(addresses) > 0 {
				port := candidate.Port
				if port == 0 {
					port = transport.Port
				}
				device.Store["matter.address"] = net.JoinHostPort(addresses[0], strconv.Itoa(int(port)))
			}
			copyMRPTXT(device.Store, candidate.Text)
			if err := c.store.UpdateDevice(ctx, device); err != nil {
				return devices, err
			}
			devices[index] = device
		}
	}
	return devices, nil
}

// EnsureSubscription start de rapportage-worker voor een node als die er niet
// al is. Voor de driver, op device.init: dat is het moment waarop een gekozen
// apparaat écht in de store staat — bij het koppelen, maar ook bij een adoptie
// na een restore. Idempotent en niet-blokkerend, dus veilig om royaal te roepen.
func (c *Controller) EnsureSubscription(nodeID uint64) {
	c.startSubscription(nodeID)
}

func (c *Controller) startSubscription(nodeID uint64) {
	if nodeID == 0 {
		return
	}
	c.subMu.Lock()
	if c.ctx.Err() != nil {
		c.subMu.Unlock()
		return
	}
	if _, exists := c.workers[nodeID]; exists {
		c.subMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.workers[nodeID] = cancel
	c.wg.Add(1)
	c.subMu.Unlock()
	go c.maintainSubscription(ctx, nodeID)
}

func (c *Controller) stopSubscription(nodeID uint64) {
	c.subMu.Lock()
	if cancel := c.workers[nodeID]; cancel != nil {
		cancel()
	}
	delete(c.subscriptions, nodeID)
	c.subMu.Unlock()
}

func (c *Controller) maintainSubscription(ctx context.Context, nodeID uint64) {
	defer c.wg.Done()
	defer func() {
		c.subMu.Lock()
		delete(c.workers, nodeID)
		delete(c.subscriptions, nodeID)
		c.subMu.Unlock()
	}()
	backoff := time.Second
	goneOnce := false
	for {
		started := time.Now()
		err := c.subscribeOnce(ctx, nodeID)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errMatterNodeGone) {
			// "Geen apparaten voor deze node" is direct ná het commissioneren
			// de NORMALE toestand: Commission start deze worker terwijl de
			// apparaten nog prototypes zijn — de gebruiker kiest ze pas daarna
			// in de koppel-UI. Meteen stoppen liet elk vers gekoppeld apparaat
			// zonder subscription achter (sessie open, waarden leeg) tot een
			// plugin-herstart; gemeten 19-08 met twee stekkers. Eén tweede
			// blik na vijftien seconden dekt dat venster; twee keer niets is
			// een node die echt weg is, en dan hoort de worker te stoppen.
			// (Kiest iemand pas ná dat venster, dan herstart OnInit van het
			// apparaat de worker — zie EnsureSubscription.)
			if goneOnce {
				return
			}
			goneOnce = true
			select {
			case <-time.After(15 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}
		goneOnce = false
		// Speling: een gemist rapportagevenster ná een lang gezonde sessie is
		// jitter of een korte stall (de rapporten komen precies óp het
		// maximuminterval, en op een gedeelde core schuift dat weleens), geen
		// bewijs van een dood apparaat. Meteen opnieuw abonneren, zónder de
		// tegels grijs te zetten en zónder backoff — lukt dát niet, dan is de
		// mislukking zelf het bewijs en gaat hij hieronder alsnog grijs. De
		// duurdrempel voorkomt een stille lus om een publisher die wel
		// abonneert maar nooit rapporteert.
		if errors.Is(err, errSubscriptionLapsed) && time.Since(started) > 30*time.Second {
			c.logger.Debug("Matter subscription lapsed; resubscribing before declaring the node unreachable",
				"node", fmt.Sprintf("%016X", nodeID))
			backoff = time.Second
			continue
		}
		c.markNodeUnavailable(nodeID, err)
		c.logger.Warn("Matter subscription stopped; retrying", "node", fmt.Sprintf("%016X", nodeID), "error", err)
		retryDelay := subscriptionRetryDelay(err, backoff)
		timer := time.NewTimer(retryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func subscriptionRetryDelay(err error, backoff time.Duration) time.Duration {
	delay := backoff
	var status pase.StatusReport
	if errors.As(err, &status) && status.ProtocolCode == pase.StatusBusy && delay < 15*time.Second {
		delay = 15 * time.Second
	}
	if strings.Contains(err.Error(), "send CASE Sigma1: peer did not acknowledge") && delay < 15*time.Second {
		delay = 15 * time.Second
	}
	return delay
}

func (c *Controller) subscribeOnce(ctx context.Context, nodeID uint64) error {
	devices, info, err := c.nodeDevices(ctx, nodeID)
	if err != nil {
		return err
	}
	// Capability support grows without requiring users to remove our fabric and
	// commission the accessory again. Older builds omitted endpoints for which
	// they knew no capability at all, so their stored cluster lists alone cannot
	// upgrade them. Re-read Descriptor once per model version before subscribing.
	if modelRefreshRequired(devices) {
		refreshed, refreshErr := c.refreshNodeModel(ctx, nodeID, devices, info)
		if refreshErr != nil {
			c.logger.Debug("Matter device-model refresh failed; subscribing to stored model",
				"node", fmt.Sprintf("%016X", nodeID), "error", refreshErr)
		} else {
			devices = refreshed
			if len(devices) > 0 {
				if refreshedInfo, parseErr := deviceConnection(devices[0]); parseErr == nil {
					info = refreshedInfo
				}
			}
		}
	}
	attributes, events := subscriptionPaths(devices)
	c.mu.Lock()
	session, err := c.session(ctx, info)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	client := im.Client{Transport: c.node, Session: session}
	subscription, err := client.Subscribe(ctx, attributes, events, subscriptionMinInterval, subscriptionMaxInterval)
	if err != nil {
		c.expireSession(nodeID, session)
		return err
	}
	activity := make(chan struct{}, 1)
	c.subMu.Lock()
	c.subscriptions[nodeID] = activeSubscription{id: subscription.ID, activity: activity}
	c.subMu.Unlock()
	c.applyReports(ctx, nodeID, subscription.Reports, subscription.Events)
	c.markNodeAvailable(nodeID)

	// A publisher must report at least once per negotiated maximum interval.
	// The grace covers MRP retries and scheduling without hiding a dead link.
	timeout := time.Duration(subscription.MaxInterval)*time.Second +
		time.Duration(subscription.MaxInterval)*time.Second/2 + 5*time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			c.subMu.Lock()
			if current, ok := c.subscriptions[nodeID]; ok && current.id == subscription.ID {
				delete(c.subscriptions, nodeID)
			}
			c.subMu.Unlock()
			c.expireSession(nodeID, session)
			return errSubscriptionLapsed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// nodeIndexTTL: hoe lang de node→device-id-index geldig blijft. Apparaten
// komen en gaan vrijwel nooit tijdens bedrijf, maar élke rapportbatch riep
// nodeDevices aan en die kloonde via store.Devices álle apparaten (vier maps
// per stuk) om er twee over te houden — 23 nodes × rapporten per seconde was
// de duurste lus van de bundel (CPU-ronde 19-08). Een minuut oude index is
// hier ruim vers genoeg; een gemiste nieuwkomer doet hooguit één minuut over
// zijn eerste rapport.
const nodeIndexTTL = time.Minute

func (c *Controller) nodeDevices(ctx context.Context, nodeID uint64) ([]Device, connectionInfo, error) {
	c.nodeIdxMu.Lock()
	fresh := time.Since(c.nodeIdxAt) < nodeIndexTTL && c.nodeIdx != nil
	ids := c.nodeIdx[nodeID]
	c.nodeIdxMu.Unlock()

	var devices []Device
	var connection connectionInfo
	if fresh {
		for _, id := range ids {
			device, err := c.store.Device(ctx, id)
			if err != nil {
				continue // net verwijderd; de TTL-herbouw ruimt de index op
			}
			info, parseErr := deviceConnection(device)
			if parseErr == nil && info.nodeID == nodeID {
				if len(devices) == 0 {
					connection = info
				}
				devices = append(devices, device)
			}
		}
		if len(devices) > 0 {
			return devices, connection, nil
		}
		// Lege of verouderde index voor deze node: door naar de volledige scan.
	}

	all, err := c.store.Devices(ctx)
	if err != nil {
		return nil, connectionInfo{}, err
	}
	index := make(map[uint64][]string)
	for _, device := range all {
		info, parseErr := deviceConnection(device)
		if parseErr != nil {
			continue
		}
		index[info.nodeID] = append(index[info.nodeID], device.ID)
		if info.nodeID == nodeID {
			if len(devices) == 0 {
				connection = info
			}
			devices = append(devices, device)
		}
	}
	c.nodeIdxMu.Lock()
	c.nodeIdx, c.nodeIdxAt = index, time.Now()
	c.nodeIdxMu.Unlock()
	if len(devices) == 0 {
		return nil, connectionInfo{}, errMatterNodeGone
	}
	return devices, connection, nil
}

func subscriptionPaths(devices []Device) ([]im.AttributePath, []im.EventPath) {
	attributes := make([]im.AttributePath, 0, len(devices)*2)
	events := make([]im.EventPath, 0, len(devices))
	seenAttributes := make(map[string]bool)
	seenEndpoints := make(map[uint16]bool)
	for _, device := range devices {
		info, err := deviceConnection(device)
		if err != nil {
			continue
		}
		for _, deviceEndpoint := range deviceEndpoints(device) {
			if !seenEndpoints[deviceEndpoint] {
				endpoint := deviceEndpoint
				events = append(events, im.EventPath{Endpoint: &endpoint})
				seenEndpoints[endpoint] = true
			}
		}
		for _, capability := range device.Capabilities {
			endpoint := capabilityEndpoint(device, capability, info.endpoint)
			cluster, attribute, ok := capabilityAttribute(device, capability, endpoint)
			if !ok {
				continue
			}
			key := fmt.Sprintf("%d/%d/%d", endpoint, cluster, attribute)
			if seenAttributes[key] {
				continue
			}
			seenAttributes[key] = true
			attributes = append(attributes, im.ConcreteAttributePath(endpoint, cluster, attribute))
		}
		for _, setting := range storedMatterSettings(device.Store["matter.settings"]) {
			cluster, clusterOK := parseClusterHex(setting.Cluster)
			attribute, attributeOK := parseClusterHex(setting.Attribute)
			if !clusterOK || !attributeOK {
				continue
			}
			key := fmt.Sprintf("%d/%d/%d", setting.Endpoint, cluster, attribute)
			if seenAttributes[key] {
				continue
			}
			seenAttributes[key] = true
			attributes = append(attributes, im.ConcreteAttributePath(setting.Endpoint, cluster, attribute))
		}
	}
	return attributes, events
}

func capabilityAttribute(device Device, capability string, endpoint uint16) (uint32, uint32, bool) {
	deviceTypes, servers := storedEndpointDeviceTypes(device, endpoint), storedMatterIDs(device.Store["matter.serverClusters"])
	// Records from before cluster inventory existed still retain enough class
	// information for the thermostat's LocalTemperature fallback.
	if len(servers) == 0 && device.Class == "thermostat" {
		servers = append(servers, thermostatCluster)
	}
	mapping, ok := mappingForCapability(deviceTypes, servers, capability)
	if !ok {
		return 0, 0, false
	}
	return mapping.Cluster, mapping.Attribute, true
}

func storedEndpointDeviceTypes(device Device, endpoint uint16) []uint32 {
	for _, inventory := range storedEndpointInventories(device.Store["~matter.endpointInventory"]) {
		if inventory.Endpoint == endpoint {
			return storedMatterIDs(inventory.DeviceTypes)
		}
	}
	return storedMatterIDs(device.Store["matter.deviceTypes"])
}

func storedMatterIDs(raw any) []uint32 {
	values := storedStrings(raw)
	result := make([]uint32, 0, len(values))
	for _, value := range values {
		if parsed, ok := parseClusterHex(value); ok {
			result = append(result, parsed)
		}
	}
	return result
}

func storedDeviceHasCluster(device Device, cluster uint32) bool {
	want := fmt.Sprintf("0x%X", cluster)
	switch values := device.Store["matter.serverClusters"].(type) {
	case []string:
		for _, value := range values {
			if value == want {
				return true
			}
		}
	case []any:
		for _, raw := range values {
			if value, ok := raw.(string); ok && value == want {
				return true
			}
		}
	}
	return false
}

func (c *Controller) acceptReports() {
	defer c.wg.Done()
	for {
		exchange, err := c.node.Accept(c.ctx)
		if err != nil {
			return
		}
		c.wg.Add(1)
		go func(exchange *transport.Exchange) {
			defer c.wg.Done()
			defer exchange.Close()
			c.handleReportExchange(exchange)
		}(exchange)
	}
}

func (c *Controller) handleReportExchange(exchange *transport.Exchange) {
	if exchange.ProtocolID() != message.ProtocolInteractionModel || exchange.PeerNodeID() == 0 {
		_ = exchange.Acknowledge()
		return
	}
	nodeID := exchange.PeerNodeID()
	c.subMu.RLock()
	active, ok := c.subscriptions[nodeID]
	c.subMu.RUnlock()
	if !ok {
		c.rejectSubscriptionReport(exchange)
		return
	}
	report, err := im.ReceiveSubscriptionReport(c.ctx, exchange, &active.id)
	if err != nil {
		c.logger.Debug("rejected Matter subscription report", "node", fmt.Sprintf("%016X", nodeID), "error", err)
		return
	}
	c.subMu.RLock()
	current, stillActive := c.subscriptions[nodeID]
	c.subMu.RUnlock()
	if !stillActive || current.id != active.id {
		return
	}
	c.applyReports(c.ctx, nodeID, report.Reports, report.Events)
	select {
	case current.activity <- struct{}{}:
	default:
	}
}

func (c *Controller) rejectSubscriptionReport(exchange *transport.Exchange) {
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	opcode, _, err := exchange.Receive(ctx)
	if err != nil {
		return
	}
	if opcode != im.OpcodeReportData {
		_ = exchange.Acknowledge()
		return
	}
	status, err := im.EncodeStatusResponse(im.StatusInvalidSubscription)
	if err == nil {
		_ = exchange.SendOnce(im.OpcodeStatusResponse, status)
	}
}

func (c *Controller) expireSession(nodeID uint64, session *transport.SecureSession) {
	if c.dropSessionIf(nodeID, session) {
		c.node.RemoveSession(session.LocalID)
	}
}

func (c *Controller) applyReports(ctx context.Context, nodeID uint64, attributes []im.AttributeReport,
	events []im.EventReport) {
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	devices, _, err := c.nodeDevices(ctx, nodeID)
	if err != nil {
		return
	}
	byEndpoint := make(map[uint16]*Device, len(devices))
	for index := range devices {
		info, parseErr := deviceConnection(devices[index])
		if parseErr == nil {
			for _, endpoint := range deviceEndpoints(devices[index]) {
				byEndpoint[endpoint] = &devices[index]
			}
			if len(deviceEndpoints(devices[index])) == 0 {
				byEndpoint[info.endpoint] = &devices[index]
			}
		}
	}
	changed := make(map[string]*Device)
	type pendingEvent struct {
		deviceID string
		tokens   map[string]any
		state    map[string]any
	}
	pendingEvents := make([]pendingEvent, 0, len(events))
	for _, report := range attributes {
		if report.Status != nil || report.Path.Endpoint == nil || report.Path.Cluster == nil || report.Path.Attribute == nil {
			continue
		}
		device := byEndpoint[*report.Path.Endpoint]
		if device == nil {
			continue
		}
		if settingID, settingValue, settingOK := reportedMatterSetting(*device, *report.Path.Endpoint, *report.Path.Cluster, *report.Path.Attribute, report.Value); settingOK {
			if device.Settings == nil {
				device.Settings = make(map[string]any)
			}
			if !reflect.DeepEqual(device.Settings[settingID], settingValue) || !device.Available || device.Message != "" {
				device.Settings[settingID] = settingValue
				device.Available, device.Message = true, ""
				changed[device.ID] = device
			}
			continue
		}
		capability, value, ok := reportedCapability(*report.Path.Endpoint, *report.Path.Cluster, *report.Path.Attribute, report.Value, *device)
		if !ok {
			continue
		}
		if device.State == nil {
			device.State = make(map[string]any)
		}
		if !reflect.DeepEqual(device.State[capability], value) || !device.Available || device.Message != "" {
			device.State[capability] = value
			device.Available, device.Message = true, ""
			changed[device.ID] = device
		}
	}
	for _, event := range events {
		if event.Status != nil || event.Path.Endpoint == nil || event.Path.Cluster == nil || event.Path.Event == nil {
			continue
		}
		device := byEndpoint[*event.Path.Endpoint]
		if device == nil || duplicateEvent(*device, event.Number) {
			continue
		}
		if device.Store == nil {
			device.Store = make(map[string]any)
		}
		device.Store["matter.lastEventNumber"] = strconv.FormatUint(event.Number, 10)
		if *event.Path.Cluster == switchCluster {
			if pressed, ok := switchPressed(*event.Path.Event); ok {
				if device.State == nil {
					device.State = make(map[string]any)
				}
				if capability := capabilityForEndpoint(*device, "button", *event.Path.Endpoint); capability != "" {
					device.State[capability] = pressed
				}
			}
		}
		device.Available, device.Message = true, ""
		changed[device.ID] = device
		name := matterEventName(*event.Path.Cluster, *event.Path.Event)
		tokens := map[string]any{
			"device": device.Name, "deviceId": device.ID, "event": name, "eventNumber": event.Number,
			"cluster": fmt.Sprintf("0x%04X", *event.Path.Cluster), "eventId": fmt.Sprintf("0x%04X", *event.Path.Event),
			"priority": event.Priority, "data": eventValue(event.Value),
		}
		state := map[string]any{
			"deviceId": device.ID, "event": name, "cluster": tokens["cluster"], "eventId": tokens["eventId"],
		}
		pendingEvents = append(pendingEvents, pendingEvent{deviceID: device.ID, tokens: tokens, state: state})
	}
	updated := make(map[string]bool, len(changed))
	for _, device := range changed {
		if err := c.store.UpdateDevice(ctx, *device); err != nil {
			c.logger.Warn("cannot persist Matter subscription update", "device", device.ID, "error", err)
		} else {
			updated[device.ID] = true
		}
	}
	for _, event := range pendingEvents {
		// Persist the deduplication marker before publishing the Flow trigger.
		// A crash can then lose an event, but can never run the same automation
		// twice after a retransmission.
		if !updated[event.deviceID] {
			continue
		}
		if err := c.store.RecordSystemFlowEvent(ctx, "trigger", "matter_event", event.tokens, event.state); err != nil {
			c.logger.Warn("cannot persist Matter event", "device", event.deviceID, "error", err)
		}
	}
}

func reportedMatterSetting(device Device, endpoint uint16, cluster, attribute uint32, value im.Value) (string, any, bool) {
	if value.Type != tlv.TypeUint {
		return "", nil, false
	}
	for _, setting := range storedMatterSettings(device.Store["matter.settings"]) {
		settingCluster, clusterOK := parseClusterHex(setting.Cluster)
		settingAttribute, attributeOK := parseClusterHex(setting.Attribute)
		if clusterOK && attributeOK && setting.Endpoint == endpoint && settingCluster == cluster && settingAttribute == attribute &&
			value.Uint < uint64(setting.Levels) {
			return setting.ID, value.Uint, true
		}
	}
	return "", nil, false
}

func reportedCapability(endpoint uint16, cluster, attribute uint32, value im.Value, device Device) (string, any, bool) {
	deviceTypes, servers := storedEndpointDeviceTypes(device, endpoint), storedMatterIDs(device.Store["matter.serverClusters"])
	mapping, ok := mappingForReport(deviceTypes, servers, cluster, attribute)
	if !ok {
		return "", nil, false
	}
	capability := capabilityForEndpoint(device, mapping.Capability, endpoint)
	if capability == "" {
		return "", nil, false
	}
	decoded, ok := mapping.Decode(value)
	return capability, decoded, ok
}

func duplicateEvent(device Device, number uint64) bool {
	previous, _ := device.Store["matter.lastEventNumber"].(string)
	parsed, err := strconv.ParseUint(previous, 10, 64)
	return err == nil && number <= parsed
}

func switchPressed(event uint32) (bool, bool) {
	switch event {
	case 1, 2, 5: // InitialPress, LongPress, MultiPressOngoing
		return true, true
	case 3, 4, 6: // ShortRelease, LongRelease, MultiPressComplete
		return false, true
	default:
		return false, false
	}
}

func matterEventName(cluster, event uint32) string {
	if cluster == switchCluster {
		names := []string{"switch_latched", "initial_press", "long_press", "short_release", "long_release", "multi_press_ongoing", "multi_press_complete"}
		if event < uint32(len(names)) {
			return names[event]
		}
	}
	if cluster == doorLockCluster {
		names := []string{"door_lock_alarm", "door_state_change", "lock_operation", "lock_operation_error", "lock_user_change"}
		if event < uint32(len(names)) {
			return names[event]
		}
	}
	return fmt.Sprintf("matter_0x%04X_0x%04X", cluster, event)
}

func eventValue(value im.Value) any {
	switch value.Type {
	case tlv.TypeBool:
		return value.Bool
	case tlv.TypeInt:
		return value.Int
	case tlv.TypeUint:
		return value.Uint
	case tlv.TypeFloat:
		return value.Float
	case tlv.TypeString:
		return string(value.Data)
	case tlv.TypeBytes:
		return hex.EncodeToString(value.Data)
	case tlv.TypeArray, tlv.TypeList:
		result := make([]any, 0, len(value.Children))
		for _, child := range value.Children {
			result = append(result, eventValue(child))
		}
		return result
	case tlv.TypeStructure:
		result := make(map[string]any, len(value.Children))
		for index, child := range value.Children {
			key := strconv.Itoa(index)
			if tag, ok := child.Tag.ContextNumber(); ok {
				key = strconv.Itoa(int(tag))
			}
			result[key] = eventValue(child)
		}
		return result
	default:
		return nil
	}
}

func (c *Controller) markNodeUnavailable(nodeID uint64, cause error) {
	// Zelf kijken of we nog mogen schrijven, en er niet op vertrouwen dat de
	// Backing dat doet. Die is een app-SDK en niet een database: hij duwt de
	// wijziging gewoon door. Zonder deze controle meldt een controller die al
	// afgesloten is alsnog apparaten als onbereikbaar, en dat overschrijft wat
	// zijn opvolger net heeft vastgesteld.
	if c.ctx != nil && c.ctx.Err() != nil {
		return
	}
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	devices, _, err := c.nodeDevices(c.ctx, nodeID)
	if err != nil {
		return
	}
	for _, device := range devices {
		message := "Matter live updates niet bereikbaar"
		if cause != nil {
			message += ": " + cause.Error()
		}
		if !device.Available && device.Message == message {
			continue
		}
		device.Available, device.Message = false, message
		_ = c.store.UpdateDevice(c.ctx, device)
	}
}

func (c *Controller) markNodeAvailable(nodeID uint64) {
	c.reportMu.Lock()
	defer c.reportMu.Unlock()
	devices, _, err := c.nodeDevices(c.ctx, nodeID)
	if err != nil {
		return
	}
	for _, device := range devices {
		if device.Available && device.Message == "" {
			continue
		}
		device.Available, device.Message = true, ""
		_ = c.store.UpdateDevice(c.ctx, device)
	}
}
