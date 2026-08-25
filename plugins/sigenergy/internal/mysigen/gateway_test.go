package mysigen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const testStationID int64 = 12025061000219

func TestGatewaySwitchIsConfirmedOnceAndReadBackBoundedly(t *testing.T) {
	var mu sync.Mutex
	statusCalls, postCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			if request.URL.Query().Get("stationId") != fmt.Sprint(testStationID) {
				t.Fatalf("stationquery = %q", request.URL.RawQuery)
			}
			statusCalls++
			switch statusCalls {
			case 1, 2, 3:
				writeGatewayStatus(response, StatusOnGrid, map[int]int{3: ManualInProgress}[statusCalls], true, ControlModeOwner)
			default:
				writeGatewayStatus(response, StatusManualIsland, ManualIdle, true, ControlModeOwner)
			}
		case "/device/gateway/ongrid-state/update":
			postCalls++
			if request.Method != http.MethodPost {
				t.Fatalf("commandmethode = %s", request.Method)
			}
			var body struct {
				OnGridState int   `json:"onGridState"`
				StationID   int64 `json:"stationId"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.OnGridState != 1 || body.StationID != testStationID {
				t.Fatalf("commandbody = %+v", body)
			}
			writeJSON(response, http.StatusOK, `{"code":0,"data":0}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)

	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOffGrid)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AlreadyReached || plan.Confirmation == "" || plan.ConfirmationText == "" {
		t.Fatalf("voorbereiding = %+v", plan)
	}
	result, err := client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{Interval: time.Millisecond, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CommandSent || result.Status.OnOffGridStatus != StatusManualIsland {
		t.Fatalf("resultaat = %+v", result)
	}
	if postCalls != 1 || statusCalls != 4 {
		t.Fatalf("POSTs = %d, statusreads = %d", postCalls, statusCalls)
	}
	if _, err := client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("tweede uitvoering = %v, wil ErrConfirmation", err)
	}
	if postCalls != 1 {
		t.Fatalf("bevestiging opnieuw gebruiken stuurde %d POSTs", postCalls)
	}
}

func TestReconnectSendsZero(t *testing.T) {
	statusCalls, postCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			statusCalls++
			status := StatusManualIsland
			if statusCalls >= 3 {
				status = StatusOnGrid
			}
			writeGatewayStatus(response, status, ManualIdle, true, ControlModeOwner)
		case "/device/gateway/ongrid-state/update":
			postCalls++
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["onGridState"] != float64(0) {
				t.Fatalf("reconnect-body = %v", body)
			}
			writeJSON(response, http.StatusOK, `{"code":0,"data":0}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)
	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOnGrid)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{Interval: time.Millisecond, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.OnOffGridStatus != StatusOnGrid || postCalls != 1 {
		t.Fatalf("resultaat = %+v, POSTs = %d", result, postCalls)
	}
}

func TestSwitchPreconditionsFailClosed(t *testing.T) {
	baseSettings := GatewaySettings{StationID: testStationID, OffGridEnable: true}
	baseStatus := GatewayStatus{OnOffGridStatus: StatusOnGrid, ManualOffGridStatus: ManualIdle, ControlMode: ControlModeOwner, ShowButton: true}
	tests := []struct {
		name     string
		settings GatewaySettings
		status   GatewayStatus
		target   GridTarget
		want     error
	}{
		{"off-grid uitgezet", GatewaySettings{StationID: testStationID}, baseStatus, TargetOffGrid, nil},
		{"knop verborgen", baseSettings, GatewayStatus{OnOffGridStatus: StatusOnGrid, ManualOffGridStatus: ManualIdle, ControlMode: ControlModeOwner}, TargetOffGrid, nil},
		{"verkeerde controlmodus", baseSettings, GatewayStatus{OnOffGridStatus: StatusOnGrid, ManualOffGridStatus: ManualIdle, ControlMode: 0, ShowButton: true}, TargetOffGrid, nil},
		{"overgang bezig", baseSettings, GatewayStatus{OnOffGridStatus: StatusOnGrid, ManualOffGridStatus: ManualInProgress, ControlMode: ControlModeOwner, ShowButton: true}, TargetOffGrid, ErrTransitionBusy},
		{"automatisch eiland niet reconnecten", baseSettings, GatewayStatus{OnOffGridStatus: StatusAutomaticIsland, ManualOffGridStatus: ManualIdle, ControlMode: ControlModeOwner, ShowButton: true}, TargetOnGrid, ErrAutomaticOffGrid},
		{"generator niet als publiek net", baseSettings, GatewayStatus{OnOffGridStatus: StatusGeneratorGrid, ManualOffGridStatus: ManualIdle, ControlMode: ControlModeOwner, ShowButton: true}, TargetOnGrid, ErrUnsupportedStatus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSwitch(test.settings, test.status, testStationID, test.target)
			if err == nil {
				t.Fatal("onveilige stand werd toegelaten")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("fout = %v, wil %v", err, test.want)
			}
		})
	}
	for _, allowed := range []struct {
		status int
		target GridTarget
	}{{StatusOnGrid, TargetOffGrid}, {StatusGeneratorGrid, TargetOffGrid}, {StatusManualIsland, TargetOnGrid}} {
		status := baseStatus
		status.OnOffGridStatus = allowed.status
		if err := validateSwitch(baseSettings, status, testStationID, allowed.target); err != nil {
			t.Errorf("status %d naar %s: %v", allowed.status, allowed.target, err)
		}
	}
}

func TestExecuteRevalidatesBeforeItCanPost(t *testing.T) {
	statusCalls, postCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			statusCalls++
			// The button was available during Prepare, then mySigen withdrew
			// control before the user confirmed.
			writeGatewayStatus(response, StatusOnGrid, ManualIdle, statusCalls == 1, ControlModeOwner)
		case "/device/gateway/ongrid-state/update":
			postCalls++
			writeJSON(response, http.StatusOK, `{"code":0,"data":0}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)
	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOffGrid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{}); err == nil {
		t.Fatal("verdwenen bediening werd toch uitgevoerd")
	}
	if postCalls != 0 {
		t.Fatalf("hercontrole stuurde %d POSTs", postCalls)
	}
}

func TestConfirmationExpiresBeforeAnyFurtherRequest(t *testing.T) {
	now := time.Now()
	statusCalls, postCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			statusCalls++
			writeGatewayStatus(response, StatusOnGrid, ManualIdle, true, ControlModeOwner)
		case "/device/gateway/ongrid-state/update":
			postCalls++
			writeJSON(response, http.StatusOK, `{"code":0,"data":0}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)
	client.now = func() time.Time { return now }
	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOffGrid)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("verlopen bevestiging = %v", err)
	}
	if statusCalls != 1 || postCalls != 0 {
		t.Fatalf("na verlopen bevestiging: statusreads = %d, POSTs = %d", statusCalls, postCalls)
	}
}

func TestReadbackTimesOutWithoutRepeatingCommand(t *testing.T) {
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			writeGatewayStatus(response, StatusOnGrid, ManualIdle, true, ControlModeOwner)
		case "/device/gateway/ongrid-state/update":
			postCalls++
			writeJSON(response, http.StatusOK, `{"code":0,"data":0}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)
	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOffGrid)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{Interval: 2 * time.Millisecond, Timeout: 20 * time.Millisecond})
	var timeout *TransitionTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("fout = %v, wil TransitionTimeoutError", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("begrensde readback duurde %v", time.Since(started))
	}
	if postCalls != 1 {
		t.Fatalf("timeout stuurde %d commando's", postCalls)
	}
}

func TestDefiniteCommandErrorGetsOneReadAndNoReplay(t *testing.T) {
	postCalls, statusCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case fmt.Sprintf("/device/gateway/settings/%d", testStationID):
			writeGatewaySettings(response, true)
		case "/device/gateway/gateway-status":
			statusCalls++
			writeGatewayStatus(response, StatusOnGrid, ManualIdle, true, ControlModeOwner)
		case "/device/gateway/ongrid-state/update":
			postCalls++
			writeJSON(response, http.StatusConflict, `{"code":409,"msg":"busy"}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, freshTokens(), nil)
	plan, err := client.PrepareSwitch(context.Background(), testStationID, TargetOffGrid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ExecuteSwitch(context.Background(), plan.Confirmation, PollOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("fout = %v, wil APIError", err)
	}
	if postCalls != 1 || statusCalls != 3 {
		t.Fatalf("POSTs = %d, statusreads = %d; wil 1 en 3", postCalls, statusCalls)
	}
}

func freshTokens() Tokens {
	return Tokens{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
}

func writeGatewaySettings(response http.ResponseWriter, enabled bool) {
	writeJSON(response, http.StatusOK, fmt.Sprintf(`{"code":0,"data":{"stationId":%d,"offGridEnable":%t}}`, testStationID, enabled))
}

func writeGatewayStatus(response http.ResponseWriter, status, manual int, show bool, control int) {
	writeJSON(response, http.StatusOK, fmt.Sprintf(`{"code":0,"data":{"onOffGridStatus":%d,"manualOffGridStatus":%d,"isSupportSignal":true,"doorStatus":0,"onOffGridControlMode":%d,"showButton":%t}}`, status, manual, control, show))
}
