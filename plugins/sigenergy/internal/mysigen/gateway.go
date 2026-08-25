package mysigen

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	StatusOnGrid          = 0
	StatusAutomaticIsland = 1
	StatusManualIsland    = 2
	StatusGeneratorGrid   = 3

	ManualIdle       = 0
	ManualInProgress = 1
	ManualConfirming = 2
	ManualErrorFirst = 3
	ManualErrorLast  = 5

	// The official owner UI enables its switch at control mode 1 and shows a
	// notice without sending a command at mode 0. Unknown modes fail closed.
	ControlModeOwner = 1
)

type flag bool

func (f *flag) UnmarshalJSON(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch v := value.(type) {
	case bool:
		*f = flag(v)
	case float64:
		if v != 0 && v != 1 {
			return fmt.Errorf("verwachte 0 of 1, kreeg %v", v)
		}
		*f = flag(v == 1)
	case string:
		if v != "0" && v != "1" && v != "true" && v != "false" {
			return fmt.Errorf("verwachte boolean, kreeg %q", v)
		}
		*f = flag(v == "1" || v == "true")
	default:
		return fmt.Errorf("verwachte boolean")
	}
	return nil
}

type GatewaySettings struct {
	StationID     int64  `json:"stationId"`
	OnGridMode    string `json:"onGridMode"`
	OffGridMode   string `json:"offGridMode"`
	Value         string `json:"value"`
	OffGridEnable flag   `json:"offGridEnable"`
}

func (s GatewaySettings) Enabled() bool { return bool(s.OffGridEnable) }

type GatewayStatus struct {
	OnOffGridStatus     int  `json:"onOffGridStatus"`
	ManualOffGridStatus int  `json:"manualOffGridStatus"`
	IsSupportSignal     flag `json:"isSupportSignal"`
	DoorStatus          int  `json:"doorStatus"`
	ControlMode         int  `json:"onOffGridControlMode"`
	ShowButton          flag `json:"showButton"`
}

func (s GatewayStatus) OffGrid() bool {
	return s.OnOffGridStatus == StatusAutomaticIsland || s.OnOffGridStatus == StatusManualIsland
}

func (s GatewayStatus) ButtonVisible() bool { return bool(s.ShowButton) }

func (c *Client) GatewaySettings(ctx context.Context, stationID int64) (GatewaySettings, error) {
	if stationID <= 0 {
		return GatewaySettings{}, fmt.Errorf("ongeldig mySigen-station-id %d", stationID)
	}
	settings := GatewaySettings{StationID: -1}
	err := c.data(ctx, http.MethodGet, "/device/gateway/settings/"+strconv.FormatInt(stationID, 10), nil, nil, &settings)
	return settings, err
}

func (c *Client) GatewayStatus(ctx context.Context, stationID int64) (GatewayStatus, error) {
	if stationID <= 0 {
		return GatewayStatus{}, fmt.Errorf("ongeldig mySigen-station-id %d", stationID)
	}
	status := GatewayStatus{OnOffGridStatus: -1, ManualOffGridStatus: -1, DoorStatus: -1, ControlMode: -1}
	query := url.Values{"stationId": {strconv.FormatInt(stationID, 10)}}
	err := c.data(ctx, http.MethodGet, "/device/gateway/gateway-status", query, nil, &status)
	return status, err
}

type GridTarget string

const (
	TargetOffGrid GridTarget = "off_grid"
	TargetOnGrid  GridTarget = "on_grid"
)

func (t GridTarget) valid() bool { return t == TargetOffGrid || t == TargetOnGrid }

func (t GridTarget) reached(status GatewayStatus) bool {
	if t == TargetOffGrid {
		return status.OffGrid()
	}
	return status.OnOffGridStatus == StatusOnGrid
}

// PreparedSwitch is a read-only preflight result. Confirmation is random,
// one-use, scoped to this client process and expires after two minutes.
type PreparedSwitch struct {
	StationID        int64         `json:"stationId"`
	Target           GridTarget    `json:"target"`
	Current          GatewayStatus `json:"current"`
	AlreadyReached   bool          `json:"alreadyReached"`
	Confirmation     string        `json:"confirmation,omitempty"`
	ConfirmationText string        `json:"confirmationText,omitempty"`
	ExpiresAt        *time.Time    `json:"expiresAt,omitempty"`
}

type pendingSwitch struct {
	stationID int64
	target    GridTarget
	expires   time.Time
}

var (
	ErrConfirmation      = errors.New("de mySigen-schakeling is niet of niet meer bevestigd")
	ErrTransitionBusy    = errors.New("de Sigenergy Gateway is al bezig met een netovergang")
	ErrAutomaticOffGrid  = errors.New("de Gateway staat automatisch off-grid; opnieuw verbinden kan pas wanneer het net terug is")
	ErrUnsupportedStatus = errors.New("de Gateway meldt een onbekende netstand")
)

func (c *Client) PrepareSwitch(ctx context.Context, stationID int64, target GridTarget) (PreparedSwitch, error) {
	if !target.valid() {
		return PreparedSwitch{}, fmt.Errorf("onbekend mySigen-doel %q", target)
	}
	settings, status, err := c.preflight(ctx, stationID)
	if err != nil {
		return PreparedSwitch{}, err
	}
	plan := PreparedSwitch{StationID: stationID, Target: target, Current: status, AlreadyReached: target.reached(status)}
	if plan.AlreadyReached {
		return plan, nil
	}
	if err := validateSwitch(settings, status, stationID, target); err != nil {
		return PreparedSwitch{}, err
	}
	token, err := confirmationToken()
	if err != nil {
		return PreparedSwitch{}, err
	}
	plan.Confirmation = token
	expires := c.now().Add(2 * time.Minute)
	plan.ExpiresAt = &expires
	if target == TargetOffGrid {
		plan.ConfirmationText = "Scheid de installatie van het openbare net en ga handmatig off-grid."
	} else {
		plan.ConfirmationText = "Verbind de installatie opnieuw met het openbare net."
	}
	c.pendingMu.Lock()
	for key, pending := range c.pending {
		if !c.now().Before(pending.expires) || pending.stationID == stationID {
			delete(c.pending, key)
		}
	}
	c.pending[token] = pendingSwitch{stationID: stationID, target: target, expires: expires}
	c.pendingMu.Unlock()
	return plan, nil
}

func confirmationToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("bevestiging maken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (c *Client) consumeConfirmation(token string) (pendingSwitch, error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	pending, ok := c.pending[token]
	delete(c.pending, token)
	if !ok || token == "" || !c.now().Before(pending.expires) {
		return pendingSwitch{}, ErrConfirmation
	}
	return pending, nil
}

func (c *Client) preflight(ctx context.Context, stationID int64) (GatewaySettings, GatewayStatus, error) {
	settings, err := c.GatewaySettings(ctx, stationID)
	if err != nil {
		return GatewaySettings{}, GatewayStatus{}, err
	}
	status, err := c.GatewayStatus(ctx, stationID)
	return settings, status, err
}

func validateSwitch(settings GatewaySettings, status GatewayStatus, stationID int64, target GridTarget) error {
	if settings.StationID > 0 && settings.StationID != stationID {
		return fmt.Errorf("mySigen antwoordde met station %d in plaats van %d", settings.StationID, stationID)
	}
	if !settings.Enabled() {
		return fmt.Errorf("off-gridbediening staat niet aan in de Gateway-instellingen")
	}
	if !status.ButtonVisible() {
		return fmt.Errorf("mySigen biedt de Go-Off-Grid-knop niet aan voor dit station")
	}
	if status.ControlMode != ControlModeOwner {
		return fmt.Errorf("Gateway-controlmodus %d staat geen eigenaarsschakeling toe", status.ControlMode)
	}
	if status.ManualOffGridStatus == ManualInProgress || status.ManualOffGridStatus == ManualConfirming {
		return ErrTransitionBusy
	}
	if status.ManualOffGridStatus >= ManualErrorFirst && status.ManualOffGridStatus <= ManualErrorLast {
		return fmt.Errorf("Gateway meldt foutstatus %d van de vorige handmatige overgang", status.ManualOffGridStatus)
	}
	if status.ManualOffGridStatus != ManualIdle {
		return fmt.Errorf("Gateway meldt onbekende handmatige status %d", status.ManualOffGridStatus)
	}
	switch target {
	case TargetOffGrid:
		if status.OnOffGridStatus != StatusOnGrid && status.OnOffGridStatus != StatusGeneratorGrid {
			return fmt.Errorf("%w: %d", ErrUnsupportedStatus, status.OnOffGridStatus)
		}
	case TargetOnGrid:
		if status.OnOffGridStatus == StatusAutomaticIsland {
			return ErrAutomaticOffGrid
		}
		if status.OnOffGridStatus != StatusManualIsland {
			return fmt.Errorf("%w: %d", ErrUnsupportedStatus, status.OnOffGridStatus)
		}
	}
	return nil
}

type PollOptions struct {
	Interval time.Duration
	Timeout  time.Duration
}

func (c *Client) normalizePoll(options PollOptions) PollOptions {
	if options.Interval <= 0 {
		options.Interval = time.Second
	}
	if options.Interval < c.pollFloor {
		options.Interval = c.pollFloor
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.Timeout > 45*time.Second {
		options.Timeout = 45 * time.Second
	}
	return options
}

type SwitchResult struct {
	Status      GatewayStatus `json:"status"`
	CommandSent bool          `json:"commandSent"`
}

type TransitionTimeoutError struct {
	Target GridTarget
	Last   GatewayStatus
}

func (e *TransitionTimeoutError) Error() string {
	return fmt.Sprintf("Gateway bereikte %s niet binnen de begrensde wachttijd (laatste status %d, handmatig %d)", e.Target, e.Last.OnOffGridStatus, e.Last.ManualOffGridStatus)
}

// ExecuteSwitch consumes a previous confirmation and sends at most one
// state-changing request. It revalidates all preconditions immediately before
// that request, then only performs bounded GET readback.
func (c *Client) ExecuteSwitch(ctx context.Context, confirmation string, options PollOptions) (SwitchResult, error) {
	pending, err := c.consumeConfirmation(confirmation)
	if err != nil {
		return SwitchResult{}, err
	}
	c.commandMu.Lock()
	defer c.commandMu.Unlock()

	settings, status, err := c.preflight(ctx, pending.stationID)
	if err != nil {
		return SwitchResult{}, err
	}
	if pending.target.reached(status) {
		return SwitchResult{Status: status}, nil
	}
	if err := validateSwitch(settings, status, pending.stationID, pending.target); err != nil {
		return SwitchResult{}, err
	}
	desired := 0
	if pending.target == TargetOffGrid {
		desired = 1
	}
	body := struct {
		OnGridState int   `json:"onGridState"`
		StationID   int64 `json:"stationId"`
	}{desired, pending.stationID}
	err = c.data(ctx, http.MethodPost, "/device/gateway/ongrid-state/update", nil, body, nil)
	if err != nil {
		var definite *APIError
		if errors.As(err, &definite) {
			// A single read can still establish that the server applied the
			// request before returning its error. Never replay the POST.
			readback, readErr := c.GatewayStatus(ctx, pending.stationID)
			if readErr == nil && pending.target.reached(readback) {
				return SwitchResult{Status: readback, CommandSent: true}, nil
			}
			return SwitchResult{Status: readback, CommandSent: true}, err
		}
		// A timeout or broken connection leaves delivery uncertain. Reconcile
		// through bounded reads, still without ever resending the command.
		readback, pollErr := c.pollTarget(ctx, pending.stationID, pending.target, options)
		if pollErr == nil {
			return SwitchResult{Status: readback, CommandSent: true}, nil
		}
		return SwitchResult{Status: readback, CommandSent: true}, fmt.Errorf("uitkomst van het eenmalige mySigen-commando is onzeker: %w (readback: %v)", err, pollErr)
	}
	readback, err := c.pollTarget(ctx, pending.stationID, pending.target, options)
	return SwitchResult{Status: readback, CommandSent: true}, err
}

func (c *Client) pollTarget(ctx context.Context, stationID int64, target GridTarget, options PollOptions) (GatewayStatus, error) {
	options = c.normalizePoll(options)
	pollCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	last := GatewayStatus{OnOffGridStatus: -1, ManualOffGridStatus: -1, ControlMode: -1}
	timer := time.NewTimer(options.Interval)
	defer timer.Stop()
	for {
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, &TransitionTimeoutError{Target: target, Last: last}
		case <-timer.C:
		}
		status, err := c.GatewayStatus(pollCtx, stationID)
		if err != nil {
			if pollCtx.Err() != nil {
				if ctx.Err() != nil {
					return last, ctx.Err()
				}
				return last, &TransitionTimeoutError{Target: target, Last: last}
			}
			return last, err
		}
		last = status
		if target.reached(status) {
			return status, nil
		}
		if status.ManualOffGridStatus >= ManualErrorFirst && status.ManualOffGridStatus <= ManualErrorLast {
			return status, fmt.Errorf("Gateway stopte de overgang met foutstatus %d", status.ManualOffGridStatus)
		}
		if target == TargetOnGrid && status.OnOffGridStatus == StatusAutomaticIsland {
			return status, ErrAutomaticOffGrid
		}
		timer.Reset(options.Interval)
	}
}
