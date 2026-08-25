package mysigen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	userAgent       = "Stulp-Sigenergy/1 mySigen-gateway"
	maxResponseSize = 1 << 20
	defaultCallTime = 10 * time.Second
)

// APIError is a definite HTTP or mySigen-envelope refusal. It deliberately
// omits response bodies, which can contain account or installation details.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	where := "mySigen"
	if e.Status != 0 {
		where += " HTTP " + strconv.Itoa(e.Status)
	}
	if e.Code != "" && e.Code != "0" {
		where += " code " + e.Code
	}
	if e.Message == "" {
		return where + " weigerde de vraag"
	}
	return where + ": " + e.Message
}

// Client is safe for concurrent reads. Gateway commands themselves are
// serialized so two confirmations cannot operate the contactor at once.
type Client struct {
	http         *http.Client
	baseURL      string
	regionHeader string
	tokens       tokenSession
	now          func() time.Time
	callTimeout  time.Duration
	pollFloor    time.Duration

	pendingMu sync.Mutex
	pending   map[string]pendingSwitch
	commandMu sync.Mutex
}

// New constructs a client from tokens kept in private plugin state. A nil
// store makes refreshed tokens memory-only and is mainly useful in tests.
func New(region Region, tokens Tokens, store TokenStore) (*Client, error) {
	baseURL, header, err := regionEndpoint(region)
	if err != nil {
		return nil, err
	}
	return &Client{
		http:         &http.Client{Timeout: defaultCallTime},
		baseURL:      baseURL,
		regionHeader: header,
		tokens:       tokenSession{tokens: tokens, store: store},
		now:          time.Now,
		callTimeout:  defaultCallTime,
		pollFloor:    150 * time.Millisecond,
		pending:      map[string]pendingSwitch{},
	}, nil
}

// Tokens returns a copy suitable for private state or a linked-status check.
// It must never be included in an app API response.
func (c *Client) Tokens() Tokens { return c.tokens.snapshot() }

func setCommonHeaders(req *http.Request, regionHeader string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("lang", "en")
	req.Header.Set("client-server", regionHeader)
	req.Header.Set("AUTH-CLIENT-ID", oauthClientID)
	req.Header.Set("sg-platform", "web")
}

func (c *Client) data(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	accessToken, err := c.tokens.accessToken(ctx, c.http, c.baseURL, c.regionHeader, c.now())
	if err != nil {
		return err
	}
	var encoded io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		encoded = bytes.NewReader(raw)
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, method, endpoint, encoded)
	if err != nil {
		return err
	}
	setCommonHeaders(req, c.regionHeader)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mySigen %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := readResponse(response.Body)
	if err != nil {
		return fmt.Errorf("mySigen-antwoord lezen: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{Status: response.StatusCode, Message: envelopeMessage(raw, "vraag geweigerd")}
	}
	data, err := envelopeData(raw)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(data) == 0 || string(data) == "null" {
		return fmt.Errorf("mySigen-antwoord mist data voor %s", path)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("mySigen stuurde onverwachte gegevens voor %s: %w", path, err)
	}
	return nil
}

func readResponse(body io.Reader) ([]byte, error) {
	limited := io.LimitReader(body, maxResponseSize+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseSize {
		return nil, fmt.Errorf("antwoord is groter dan %d bytes", maxResponseSize)
	}
	return raw, nil
}

type responseEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"msg"`
	Alt     string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func envelopeData(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("mySigen stuurde een leeg antwoord")
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("mySigen stuurde geen JSON: %w", err)
	}
	code := normalizeCode(envelope.Code)
	if code != "" && code != "0" {
		message := envelope.Message
		if message == "" {
			message = envelope.Alt
		}
		return nil, &APIError{Code: code, Message: message}
	}
	// Token responses in older clients were sometimes not enveloped. Supporting
	// that shape here keeps authentication compatible without making normal API
	// methods accept arbitrary response objects as data.
	if envelope.Data == nil {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err == nil {
			if _, token := object["access_token"]; token {
				return raw, nil
			}
		}
		return nil, nil
	}
	return envelope.Data, nil
}

func normalizeCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return string(raw)
}

func envelopeMessage(raw []byte, fallback string) string {
	var envelope responseEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return fallback
	}
	if envelope.Message != "" {
		return envelope.Message
	}
	if envelope.Alt != "" {
		return envelope.Alt
	}
	return fallback
}

// StationList is the data returned by GET /device/owner/station/list.
type StationList struct {
	BatteryCapacity   float64   `json:"batteryCapacityCount"`
	PVGenerationToday float64   `json:"pvPowerGenerationDayCount"`
	StationCount      float64   `json:"stationCount"`
	Stations          []Station `json:"stationList"`
}

type Station struct {
	ActivationStatus   int     `json:"activationStatus"`
	BatteryCapacity    float64 `json:"batteryCapacity"`
	PVCapacity         float64 `json:"pvCapacity"`
	EquivalentHours    float64 `json:"equivalentHours"`
	PVGenerationToday  float64 `json:"pvPowerGenerationDay"`
	SceneMode          int     `json:"sceneMode"`
	IndustryType       int     `json:"industryType"`
	ID                 int64   `json:"stationId"`
	Name               string  `json:"stationShowName"`
	Type               int     `json:"stationType"`
	Status             int     `json:"status"`
	TotalChargingPower float64 `json:"totalChargingPower"`
	TotalChargingTimes float64 `json:"totalChargingTimes"`
}

func (c *Client) Stations(ctx context.Context) (StationList, error) {
	var stations StationList
	err := c.data(ctx, http.MethodGet, "/device/owner/station/list", nil, nil, &stations)
	return stations, err
}
