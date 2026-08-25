package mysigen

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestPasswordEncodingMatchesMySigen(t *testing.T) {
	got, err := EncryptPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2XgqjWOYS8wesGPH8+9zTA=="; got != want {
		t.Fatalf("EncryptPassword = %q, wil %q", got, want)
	}
}

func TestAuthenticateUsesPasswordOnceAndReturnsOnlyTokens(t *testing.T) {
	var form url.Values
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api-eu.sigencloud.com/auth/oauth/token" {
			t.Fatalf("token-URL = %s", request.URL)
		}
		if username, password, ok := request.BasicAuth(); !ok || username != "sigen" || password != "sigen" {
			t.Fatalf("basic auth = %q/%q, aanwezig %v", username, password, ok)
		}
		if request.Header.Get("User-Agent") != userAgent || request.Header.Get("client-server") != "eu" {
			t.Fatalf("protocolheaders ontbreken: %v", request.Header)
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "correct horse") {
			t.Fatal("het klare wachtwoord ging over de lijn")
		}
		form, err = url.ParseQuery(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		return jsonResponse(http.StatusOK, `{"code":0,"data":{"access_token":"access","refresh_token":"refresh","expires_in":3600}}`), nil
	})}

	before := time.Now()
	tokens, err := Authenticate(context.Background(), httpClient, RegionEU, " owner@example.test ", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("scope") != "server" || form.Get("grant_type") != "password" || form.Get("username") != "owner@example.test" {
		t.Fatalf("loginformulier = %v", form)
	}
	if form.Get("password") != "2XgqjWOYS8wesGPH8+9zTA==" {
		t.Fatalf("gecodeerd wachtwoord = %q", form.Get("password"))
	}
	if len(form.Get("userDeviceId")) != 36 {
		t.Fatalf("userDeviceId = %q", form.Get("userDeviceId"))
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" {
		t.Fatalf("tokens = %+v", tokens)
	}
	// The persisted deadline includes the one-minute safety margin.
	if tokens.ExpiresAt.Before(before.Add(58*time.Minute)) || tokens.ExpiresAt.After(before.Add(60*time.Minute)) {
		t.Fatalf("verversdeadline = %v", tokens.ExpiresAt)
	}
}

func TestExpiredTokenRefreshesBeforeStationListAndIsPersisted(t *testing.T) {
	var refreshCalls, stationCalls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth/oauth/token":
			refreshCalls++
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "old-refresh" {
				t.Fatalf("refreshformulier = %v", request.Form)
			}
			writeJSON(response, http.StatusOK, `{"code":0,"data":{"access_token":"new-access","refresh_token":"new-refresh","expires_in":"180"}}`)
		case "/device/owner/station/list":
			stationCalls++
			if request.Method != http.MethodGet {
				t.Fatalf("stationmethode = %s", request.Method)
			}
			if request.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("User-Agent") != userAgent || request.Header.Get("AUTH-CLIENT-ID") != "sigen" || request.Header.Get("sg-platform") != "web" {
				t.Fatalf("appheaders ontbreken: %v", request.Header)
			}
			writeJSON(response, http.StatusOK, `{"code":0,"data":{"batteryCapacityCount":24,"pvPowerGenerationDayCount":31.5,"stationCount":1,"stationList":[{"activationStatus":1,"batteryCapacity":24,"pvCapacity":12.5,"equivalentHours":2.52,"pvPowerGenerationDay":31.5,"sceneMode":4,"industryType":1,"stationId":12025061000219,"stationShowName":"Thuis","stationType":1,"status":1,"totalChargingPower":7.4,"totalChargingTimes":9}]}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var saved Tokens
	client := testClient(t, server, Tokens{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}, func(tokens Tokens) error {
		saved = tokens
		return nil
	})
	stations, err := client.Stations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls != 1 || stationCalls != 1 {
		t.Fatalf("refreshes = %d, stationvragen = %d", refreshCalls, stationCalls)
	}
	if saved.AccessToken != "new-access" || saved.RefreshToken != "new-refresh" {
		t.Fatalf("bewaarde tokens = %+v", saved)
	}
	if stations.StationCount != 1 || len(stations.Stations) != 1 || stations.Stations[0].ID != 12025061000219 || stations.Stations[0].Name != "Thuis" {
		t.Fatalf("stations = %+v", stations)
	}
	// A second read uses the fresh in-memory token, not another refresh.
	if _, err := client.Stations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshCalls != 1 || stationCalls != 2 {
		t.Fatalf("na tweede vraag: refreshes = %d, stations = %d", refreshCalls, stationCalls)
	}
}

func TestRegionHostsAreClosedSet(t *testing.T) {
	for region, want := range map[Region]string{
		RegionEU: "https://api-eu.sigencloud.com/", RegionAPAC: "https://api-apac.sigencloud.com/",
		RegionCN: "https://api-cn.sigenergy.com/", RegionUS: "https://api-us.sigencloud.com/",
		RegionAUS: "https://api-aus.sigencloud.com/", RegionJP: "https://api-jp.sigencloud.com/",
	} {
		got, _, err := regionEndpoint(region)
		if err != nil || got != want {
			t.Errorf("regio %q = %q, %v; wil %q", region, got, err, want)
		}
	}
	if _, err := New(Region("https://attacker.invalid"), Tokens{}, nil); err == nil {
		t.Fatal("een willekeurige API-host werd geaccepteerd")
	}
}

func testClient(t *testing.T, server *httptest.Server, tokens Tokens, store TokenStore) *Client {
	t.Helper()
	client, err := New(RegionEU, tokens, store)
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL + "/"
	client.http = server.Client()
	client.callTimeout = time.Second
	client.pollFloor = time.Millisecond
	return client
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func writeJSON(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body)
}
