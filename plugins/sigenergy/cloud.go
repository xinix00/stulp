package main

// De optionele mySigen-koppeling.
//
// Modbus blijft de lokale, snelle meetweg. Alleen de handmatige
// Gateway-schakeling zit niet in het publieke Modbus-contract en loopt daarom
// via het account dat de eigenaar ook in de mySigen-app gebruikt. Het
// wachtwoord wordt één keer aan Authenticate gegeven en nergens opgeslagen;
// alleen access- en refresh-token gaan in de private app-state.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xinix00/stulp/internal/appsdk"
	"github.com/xinix00/stulp/plugins/sigenergy/internal/mysigen"
)

const cloudCallTimeout = 30 * time.Second

// cloudClient is de smalle naad die de app en de Gateway-driver werkelijk
// gebruiken. De concrete client blijft in internal/mysigen; tests kunnen hier
// een in-memory antwoord achter zetten zonder een echt huis te schakelen.
type cloudClient interface {
	Stations(context.Context) (mysigen.StationList, error)
	GatewayStatus(context.Context, int64) (mysigen.GatewayStatus, error)
	PrepareSwitch(context.Context, int64, mysigen.GridTarget) (mysigen.PreparedSwitch, error)
	ExecuteSwitch(context.Context, string, mysigen.PollOptions) (mysigen.SwitchResult, error)
}

// storedCloud is de enige vorm die de procesgrens overleeft. Username en regio
// zijn nodig om op de configuratiepagina te laten zien welk account gekoppeld
// is; Tokens blijft private app-state en komt nooit in een API-antwoord.
type storedCloud struct {
	Version  int            `json:"version"`
	Region   mysigen.Region `json:"region,omitempty"`
	Username string         `json:"username,omitempty"`
	Tokens   mysigen.Tokens `json:"tokens,omitempty"`
}

func readStoredCloud(raw json.RawMessage) (storedCloud, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return storedCloud{}, nil
	}
	var stored storedCloud
	if err := json.Unmarshal(raw, &stored); err != nil {
		return storedCloud{}, fmt.Errorf("de bewaarde mySigen-koppeling is onleesbaar: %w", err)
	}
	if stored.Version != 0 && stored.Version != 1 {
		return storedCloud{}, fmt.Errorf("de bewaarde mySigen-koppeling heeft onbekende versie %d", stored.Version)
	}
	stored.Version = 1
	stored.Username = strings.TrimSpace(stored.Username)
	return stored, nil
}

func writeStoredCloud(stored storedCloud) (json.RawMessage, error) {
	stored.Version = 1
	return json.Marshal(stored)
}

func (a *app) restoreCloud(raw json.RawMessage) error {
	stored, err := readStoredCloud(raw)
	if err != nil {
		return err
	}
	if stored.Region == "" {
		stored.Region = mysigen.RegionEU
	}
	// Ook zonder token blijven regio en accountnaam staan na "koppeling
	// verbreken"; opnieuw koppelen vraagt dan alleen nog het wachtwoord.
	if stored.Tokens.AccessToken == "" {
		a.mu.Lock()
		a.cloudGeneration++
		a.cloudIdentity = stored
		a.cloud = nil
		a.mu.Unlock()
		return nil
	}
	return a.installCloud(stored, false)
}

// installCloud vervangt de actieve accountkoppeling. De generatie voorkomt dat
// een refresh van de oude client na uitloggen zijn token alsnog terugschrijft.
func (a *app) installCloud(stored storedCloud, persist bool) error {
	stored.Version = 1
	a.mu.Lock()

	generation := a.cloudGeneration + 1
	client, err := mysigen.New(stored.Region, stored.Tokens, func(tokens mysigen.Tokens) error {
		return a.persistCloudTokens(generation, tokens)
	})
	if err != nil {
		a.mu.Unlock()
		return err
	}
	if persist {
		raw, err := writeStoredCloud(stored)
		if err != nil {
			a.mu.Unlock()
			return err
		}
		if err := a.stulp.SetState(raw); err != nil {
			a.mu.Unlock()
			return fmt.Errorf("mySigen-koppeling bewaren: %w", err)
		}
	}
	a.cloudGeneration = generation
	a.cloudIdentity = stored
	a.cloud = client
	a.mu.Unlock()
	a.cancelGatewayCommands()
	return nil
}

func (a *app) persistCloudTokens(generation uint64, tokens mysigen.Tokens) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation != a.cloudGeneration || a.cloud == nil {
		return fmt.Errorf("deze mySigen-koppeling is intussen vervangen")
	}
	stored := a.cloudIdentity
	stored.Tokens = tokens
	raw, err := writeStoredCloud(stored)
	if err != nil {
		return err
	}
	if err := a.stulp.SetState(raw); err != nil {
		return err
	}
	a.cloudIdentity = stored
	return nil
}

func (a *app) apiCloud() (cloudClient, error) {
	a.mu.RLock()
	client := a.cloud
	a.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("koppel eerst het mySigen-account op de configuratiepagina")
	}
	return client, nil
}

func (a *app) cloudStatus() map[string]any {
	a.mu.RLock()
	stored, linked := a.cloudIdentity, a.cloud != nil
	a.mu.RUnlock()
	region := stored.Region
	if region == "" {
		region = mysigen.RegionEU
	}
	return map[string]any{
		"cloudLinked":   linked,
		"cloudRegion":   string(region),
		"cloudUsername": stored.Username,
	}
}

func (a *app) connectCloud(ctx context.Context, region mysigen.Region, username, password string) (mysigen.StationList, error) {
	username = strings.TrimSpace(username)
	tokens, err := mysigen.Authenticate(ctx, &http.Client{Timeout: 15 * time.Second}, region, username, password)
	if err != nil {
		return mysigen.StationList{}, err
	}
	// Eerst bewijzen dat dit token werkelijk bij het eigenaarsaccount kan. Een
	// geldige login zonder stations opslaan levert anders een "gekoppelde" app
	// op die niets kan tonen.
	probe, err := mysigen.New(region, tokens, nil)
	if err != nil {
		return mysigen.StationList{}, err
	}
	stations, err := probe.Stations(ctx)
	if err != nil {
		return mysigen.StationList{}, err
	}
	tokens = probe.Tokens()
	if err := a.installCloud(storedCloud{Version: 1, Region: region, Username: username, Tokens: tokens}, true); err != nil {
		return mysigen.StationList{}, err
	}
	a.refreshGateways()
	return stations, nil
}

func (a *app) disconnectCloud() error {
	a.mu.Lock()
	stored := a.cloudIdentity
	stored.Version = 1
	stored.Tokens = mysigen.Tokens{}
	raw, err := writeStoredCloud(stored)
	if err == nil {
		err = a.stulp.SetState(raw)
	}
	// In het geheugen altijd meteen uit. Als bewaren faalt vertelt de pagina dat
	// expliciet; de oude client mag in deze generatie in elk geval niets meer.
	a.cloudGeneration++
	a.cloudIdentity = stored
	a.cloud = nil
	a.mu.Unlock()
	a.cancelGatewayCommands()
	a.refreshGateways()
	if err != nil {
		return fmt.Errorf("mySigen-koppeling wissen: %w", err)
	}
	return nil
}

func (a *app) cancelGatewayCommands() {
	a.mu.RLock()
	gateways := make([]*gatewayDevice, 0, len(a.gateways))
	for _, gateway := range a.gateways {
		gateways = append(gateways, gateway)
	}
	a.mu.RUnlock()
	for _, gateway := range gateways {
		gateway.cancelCommand()
	}
}

// stopCloudRuntime bewaart de accountstate maar stopt alle pollers en maakt
// refresh-hooks uit deze procesgeneratie ongeldig.
func (a *app) stopCloudRuntime() {
	a.mu.Lock()
	a.cloudGeneration++
	a.cloud = nil
	gateways := make([]*gatewayDevice, 0, len(a.gateways))
	for _, gateway := range a.gateways {
		gateways = append(gateways, gateway)
	}
	a.gateways = map[string]*gatewayDevice{}
	a.mu.Unlock()
	for _, gateway := range gateways {
		gateway.halt()
	}
}

func stationSummary(list mysigen.StationList) []map[string]any {
	out := make([]map[string]any, 0, len(list.Stations))
	for _, station := range list.Stations {
		out = append(out, map[string]any{
			"id":         station.ID,
			"name":       station.Name,
			"status":     station.Status,
			"activation": station.ActivationStatus,
		})
	}
	return out
}

func describeCloudStations(ctx context.Context, client cloudClient, stations mysigen.StationList) map[string]any {
	items := stationSummary(stations)
	for index, station := range stations.Stations {
		status, statusErr := client.GatewayStatus(ctx, station.ID)
		if statusErr != nil {
			items[index]["gatewayError"] = statusErr.Error()
			continue
		}
		items[index]["gateway"] = status.KnownGridStatus()
		items[index]["gatewayControllable"] = status.ButtonVisible() && status.ControlMode == mysigen.ControlModeOwner
		items[index]["offGrid"] = status.OffGrid()
		items[index]["gridStatus"] = status.OnOffGridStatus
	}
	return map[string]any{"linked": true, "stations": items}
}

func (a *app) describeCloud(ctx context.Context) (map[string]any, error) {
	client, err := a.apiCloud()
	if err != nil {
		return nil, err
	}
	stations, err := client.Stations(ctx)
	if err != nil {
		return nil, err
	}
	return describeCloudStations(ctx, client, stations), nil
}

func (a *app) registerCloudAPI(stulp *appsdk.Stulp) {
	stulp.OnRequest("cloud_connect", func(_, body map[string]any) (any, error) {
		regionText, _ := body["region"].(string)
		username, _ := body["username"].(string)
		password, _ := body["password"].(string)
		region := mysigen.Region(strings.ToLower(strings.TrimSpace(regionText)))
		if region == "" {
			region = mysigen.RegionEU
		}
		ctx, cancel := context.WithTimeout(context.Background(), cloudCallTimeout)
		defer cancel()
		stations, err := a.connectCloud(ctx, region, username, password)
		if err != nil {
			return nil, err
		}
		// Meteen de Gateway-status meenemen. Alleen de stationlijst teruggeven
		// liet de configuratiepagina na correct aanmelden ten onrechte twijfelen
		// of er überhaupt een noodstroom-Gateway aanwezig was.
		client, err := a.apiCloud()
		if err != nil {
			return nil, err
		}
		return describeCloudStations(ctx, client, stations), nil
	})

	stulp.OnRequest("cloud_check", func(map[string]any, map[string]any) (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), cloudCallTimeout)
		defer cancel()
		return a.describeCloud(ctx)
	})

	stulp.OnRequest("cloud_disconnect", func(map[string]any, map[string]any) (any, error) {
		if err := a.disconnectCloud(); err != nil {
			return nil, err
		}
		return map[string]any{"linked": false}, nil
	})
}
