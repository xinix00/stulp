package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
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
	// Automations are latency-sensitive: this subscription also carries motion,
	// contact and button events, so asking the publisher to batch for a second
	// makes a healthy Flow feel intermittent. Zero lets a node report an urgent
	// change immediately. The controller still coalesces every report into one
	// device update, and a device remains free to apply its own reporting policy.
	subscriptionMinInterval = 0
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
	go c.maintainSubscription(ctx, nodeID, c.subscribeOnce)
}

func (c *Controller) stopSubscription(nodeID uint64) {
	c.subMu.Lock()
	if cancel := c.workers[nodeID]; cancel != nil {
		cancel()
	}
	delete(c.subscriptions, nodeID)
	c.subMu.Unlock()
}

// maintainSubscription krijgt subscribe als parameter en niet als veld op de
// Controller: de lus is het gedrag dat getest moet worden (stil binnen het
// route-venster, grijs en retry erbuiten) en een echte subscribeOnce is in een
// test niet tot een route-fout te dwingen. Een parameter draagt dat zonder
// productie-state toe te voegen.
func (c *Controller) maintainSubscription(ctx context.Context, nodeID uint64, subscribe func(context.Context, uint64) error) {
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
		attemptCtx, routeToken, gateErr := c.beginSubscriptionAttempt(ctx)
		started := time.Now()
		err := gateErr
		var routeWaitStarted time.Time
		if err == nil {
			err = subscribe(attemptCtx, nodeID)
			routeWaitStarted = c.finishSubscriptionAttempt(attemptCtx, routeToken, nodeID, err)
		}
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
		// "no IPv6 route" is een toestand van de gedeelde IPv6-stack, geen
		// oordeel over dit apparaat: bij een (her)start moet de router
		// advertisement nog landen voordat leannet een v6-route heeft, en tot die
		// tijd faalt élke CASE
		// onmiddellijk. Zevenentwintig workers die daar elk per poging een warn
		// van maken en hun tegel grijs zetten, maken van elke boot een
		// rampenfilm die zichzelf een minuut later oplost (gemeten 20-08).
		// Dus: binnen routeWaitLoud stil vasthouden, niets overschrijven, en met
		// één gedeelde probe opnieuw kijken — de eerste poging ná de RA slaagt
		// gewoon. Eén debug-regel per episode houdt het log eerlijk zonder de
		// ringbuffer te verzuipen. De tegel liegt hier niet bij: bereikbaarheid
		// overleeft geen start van Stulp — hij is state, geen configuratie (zie
		// deviceRecord in de store) — dus een boot en een restore beginnen
		// eerlijk onbereikbaar, en mid-run staat er een toestand die deze run
		// zelf heeft waargemaakt.
		if isNoIPv6Route(err) {
			if routeWaitStarted.IsZero() {
				routeWaitStarted = c.routeRecoveryStarted()
			}
			// A different in-flight exchange may already have proved the route
			// after this error was produced. In that case this stale result must
			// neither reopen the gate nor make a tile flicker unavailable.
			if routeWaitStarted.IsZero() {
				continue
			}
			// Stil is goed voor een opstartmoment, niet voor een toestand. Na
			// routeWaitLoud is dit geen boot meer maar een node zonder route, en
			// dan hoort dit géén eigen wachtlus te zijn: DOORVALLEN naar het
			// gedeelde pad hieronder, dat de node onbereikbaar markeert (grijs is
			// hier de waarheid), één keer waarschuwt met de échte fout, en blijft
			// proberen met per-node backoff; de routeprobe zelf blijft gedeeld en
			// maximaal vijf seconden uit elkaar. Vóór 21-08 bleef deze tak eeuwig
			// doorlussen: na een restore hielden achtentwintig workers stil een
			// "subscription" vast die nooit bestond, terwijl elke tegel op
			// UNDEFINED bleef staan omdat markNodeUnavailable werd overgeslagen --
			// stilte die eruitzag als een werkende node.
			if time.Since(routeWaitStarted) <= routeWaitLoud {
				// beginSubscriptionAttempt owns the shared, exponentially backed
				// off timer. Looping here does not launch another CASE attempt: this
				// worker joins the same controller-wide wait as every other node.
				continue
			}
			if current := c.routeRecoveryStarted(); current.IsZero() || current != routeWaitStarted {
				continue
			}
		}
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

// subscriptionAttemptKey carries the gate token and route-proof epoch into
// subscribeOnce. A successful Subscribe does not return (it becomes the
// long-lived worker), so subscribeOnce must release the startup gate as soon
// as that response is registered rather than only when the subscription
// eventually lapses.
type subscriptionAttemptKey struct{}

type subscriptionAttempt struct {
	token uint64
	proof uint64
}

// beginSubscriptionAttempt makes the first startup exchange a canary. Once a
// route has worked, workers are free to proceed normally (CASE setup is
// coalesced only with another setup to the same node). If that canary sees
// LEAN's no-route error, only
// one worker is admitted at each shared retry deadline; all others sleep on the
// same state change. After routeWaitLoud they may report the shared error to
// their own device without performing another network exchange.
func (c *Controller) beginSubscriptionAttempt(ctx context.Context) (context.Context, uint64, error) {
	for {
		now := time.Now()
		c.routeMu.Lock()
		changed := c.routeChangeLocked()

		if !c.routeRecovering {
			if c.routeKnown {
				attempt := subscriptionAttempt{proof: c.routeProof}
				c.routeMu.Unlock()
				return context.WithValue(ctx, subscriptionAttemptKey{}, attempt), 0, nil
			}
			if c.routeProbe == 0 {
				token := c.startRouteProbeLocked()
				attempt := subscriptionAttempt{token: token, proof: c.routeProof}
				c.routeMu.Unlock()
				return context.WithValue(ctx, subscriptionAttemptKey{}, attempt), token, nil
			}
			c.routeMu.Unlock()
			if !waitForRouteChange(ctx, changed, time.Time{}) {
				return ctx, 0, ctx.Err()
			}
			continue
		}

		quietUntil := c.routeStarted.Add(routeWaitLoud)
		if c.routeProbe == 0 && !now.Before(c.routeNextProbe) {
			token := c.startRouteProbeLocked()
			attempt := subscriptionAttempt{token: token, proof: c.routeProof}
			c.routeMu.Unlock()
			return context.WithValue(ctx, subscriptionAttemptKey{}, attempt), token, nil
		}
		if !now.Before(quietUntil) {
			err := c.routeLastErr
			c.routeMu.Unlock()
			if err == nil {
				err = errors.New("Matter IPv6 route is not available")
			}
			return ctx, 0, err
		}

		wakeAt := quietUntil
		if c.routeProbe == 0 && c.routeNextProbe.Before(wakeAt) {
			wakeAt = c.routeNextProbe
		}
		c.routeMu.Unlock()
		if !waitForRouteChange(ctx, changed, wakeAt) {
			return ctx, 0, ctx.Err()
		}
	}
}

func (c *Controller) startRouteProbeLocked() uint64 {
	c.routeGeneration++
	if c.routeGeneration == 0 { // reserve zero for an unrestricted attempt
		c.routeGeneration++
	}
	c.routeProbe = c.routeGeneration
	return c.routeProbe
}

func (c *Controller) routeChangeLocked() chan struct{} {
	if c.routeChanged == nil {
		c.routeChanged = make(chan struct{})
	}
	return c.routeChanged
}

func (c *Controller) signalRouteChangeLocked() {
	changed := c.routeChangeLocked()
	close(changed)
	c.routeChanged = make(chan struct{})
}

func waitForRouteChange(ctx context.Context, changed <-chan struct{}, wakeAt time.Time) bool {
	if wakeAt.IsZero() {
		select {
		case <-changed:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(max(time.Until(wakeAt), 0))
	defer timer.Stop()
	select {
	case <-changed:
		return true
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// finishSubscriptionAttempt records a route failure before another worker can
// start an expensive retry. Generic failures release the canary too: just as in
// the old loop, any error other than LEAN's explicit no-route result means the
// controller-wide route gate must not hold unrelated nodes back.
func (c *Controller) finishSubscriptionAttempt(ctx context.Context, token uint64, nodeID uint64, err error) time.Time {
	if isNoIPv6Route(err) {
		attempt, hasAttempt := ctx.Value(subscriptionAttemptKey{}).(subscriptionAttempt)
		c.routeMu.Lock()
		// A selected probe can become stale when another in-flight exchange has
		// already proved the route. Its later failure must not reopen the gate.
		if token != 0 && c.routeProbe != token {
			c.routeMu.Unlock()
			return time.Time{}
		}
		// Unrestricted attempts can overlap after the route was proven. If a
		// newer success completed after this attempt began, its proof epoch wins:
		// the older failure must not reopen a recovered controller-wide gate.
		if token == 0 && hasAttempt && attempt.proof != c.routeProof {
			c.routeMu.Unlock()
			return time.Time{}
		}
		now := time.Now()
		newEpisode := !c.routeRecovering
		if newEpisode {
			c.routeRecovering = true
			c.routeKnown = false
			c.routeStarted = now
			c.routeBackoff = routeRetryInitial
		}
		c.routeLastErr = err
		if newEpisode || token != 0 && c.routeProbe == token {
			c.routeProbe = 0
			c.routeNextProbe = now.Add(routeRecoveryDelay(c.routeBackoff))
			c.routeBackoff = min(c.routeBackoff*2, routeRetryMaximum)
		}
		c.signalRouteChangeLocked()
		started := c.routeStarted
		c.routeMu.Unlock()
		if newEpisode && c.logger != nil {
			// Eerlijke woorden: er ís nog geen subscription om vast te houden.
			c.logger.Debug("IPv6 route not up yet; Matter waits before connecting",
				"node", fmt.Sprintf("%016X", nodeID))
		}
		return started
	}

	if token == 0 {
		return time.Time{}
	}
	c.routeMu.Lock()
	if c.routeProbe != token {
		c.routeMu.Unlock()
		return time.Time{}
	}
	recovering, started := c.routeRecovering, c.routeStarted
	c.clearRouteRecoveryLocked()
	c.routeMu.Unlock()
	if recovering && c.logger != nil {
		c.logger.Info("IPv6 route is back; Matter is reconnecting",
			"node", fmt.Sprintf("%016X", nodeID),
			"waited", time.Since(started).Round(time.Second).String())
	}
	return time.Time{}
}

// subscriptionRouteReady releases both the initial canary and a recovery
// probe at the first successfully established subscription. It deliberately
// runs before initial report/store processing and the long report-wait loop.
func (c *Controller) subscriptionRouteReady(ctx context.Context, nodeID uint64) {
	attempt, _ := ctx.Value(subscriptionAttemptKey{}).(subscriptionAttempt)
	token := attempt.token
	c.routeMu.Lock()
	if token != 0 && c.routeProbe != token {
		c.routeMu.Unlock()
		return
	}
	recovering, started := c.routeRecovering, c.routeStarted
	c.clearRouteRecoveryLocked()
	c.routeMu.Unlock()
	if recovering && c.logger != nil {
		c.logger.Info("IPv6 route is back; Matter is reconnecting",
			"node", fmt.Sprintf("%016X", nodeID),
			"waited", time.Since(started).Round(time.Second).String())
	}
}

func (c *Controller) clearRouteRecoveryLocked() {
	c.routeProof++
	c.routeKnown = true
	c.routeRecovering = false
	c.routeStarted = time.Time{}
	c.routeNextProbe = time.Time{}
	c.routeBackoff = 0
	c.routeLastErr = nil
	c.routeProbe = 0
	c.signalRouteChangeLocked()
}

func (c *Controller) routeRecoveryStarted() time.Time {
	c.routeMu.Lock()
	defer c.routeMu.Unlock()
	if !c.routeRecovering {
		return time.Time{}
	}
	return c.routeStarted
}

func isNoIPv6Route(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no IPv6 route")
}

func routeRecoveryDelay(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		backoff = routeRetryInitial
	}
	spread := routeRetryJitter * (2*mathrand.Float64() - 1)
	return max(time.Duration(float64(backoff)*(1+spread)), time.Millisecond)
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
	session, err := c.session(ctx, info)
	if err != nil {
		return err
	}
	// Capability support grows without requiring users to remove our fabric and
	// commission the accessory again. Older builds omitted endpoints for which
	// they knew no capability at all, so their stored cluster lists alone cannot
	// upgrade them. Re-read Descriptor once per model version before subscribing.
	if modelRefreshRequired(devices) {
		refreshed, refreshErr := c.refreshNodeModel(ctx, nodeID, devices, info, session)
		if refreshErr != nil {
			// A route failure is controller-wide, not evidence that the stored
			// model is bad. Returning it immediately lets the shared gate back off;
			// falling through would send a second request over the same missing
			// route from Subscribe.
			if isNoIPv6Route(refreshErr) {
				c.expireSession(nodeID, session)
				return refreshErr
			}
			c.logger.Debug("Matter device-model refresh failed; subscribing to stored model",
				"node", fmt.Sprintf("%016X", nodeID), "error", refreshErr)
		} else {
			devices = refreshed
		}
	}
	attributes, events := subscriptionPaths(devices)
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
	// The Subscribe response already proves that IPv6 works. Wake the other
	// nodes before bridge report processing and store RPCs add unrelated delay.
	c.subscriptionRouteReady(ctx, nodeID)
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

// routeWaitLoud keeps the existing UX contract: a route that is still coming
// up stays quiet for two minutes, after which devices truthfully become
// unavailable. The attempts inside that window are now shared by the whole
// controller and use bounded exponential backoff with jitter.
var (
	routeWaitLoud     = 2 * time.Minute
	routeRetryInitial = time.Second
	// One shared CASE probe every five seconds is cheap and notices a Router
	// Advertisement promptly; the old cost came from doing that per node.
	routeRetryMaximum = 5 * time.Second
	routeRetryJitter  = 0.20
)

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
