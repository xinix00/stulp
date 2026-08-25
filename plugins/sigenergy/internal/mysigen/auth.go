package mysigen

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	oauthClientID     = "sigen"
	oauthClientSecret = "sigen"
	passwordAESKey    = "sigensigensigenp"
	passwordAESIV     = "sigensigensigenp"
	refreshMargin     = time.Minute
)

// Tokens is the only authentication material that needs to survive a restart.
// The account password is used by Authenticate and is never retained.
// ExpiresAt is already moved one refreshMargin before the server's hard expiry.
type Tokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// TokenStore persists a refreshed token bundle in the plugin's private state.
// It must not put tokens in ordinary app settings or return them to the UI.
type TokenStore func(Tokens) error

var ErrNotAuthenticated = errors.New("mySigen is niet gekoppeld")

// EncryptPassword applies the encoding used by the official mySigen clients.
// This is protocol obfuscation, not storage encryption; callers must still
// treat both the clear text and the result as secrets.
func EncryptPassword(password string) (string, error) {
	block, err := aes.NewCipher([]byte(passwordAESKey))
	if err != nil {
		return "", err
	}
	plain := []byte(password)
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytesOf(byte(padding), padding)...)
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, []byte(passwordAESIV)).CryptBlocks(encrypted, plain)
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}

// Authenticate performs the one password-grant request used by mySigen. The
// password and its encoded form are local variables only; the returned value is
// safe to pass to New and persist in private plugin state.
func Authenticate(ctx context.Context, httpClient *http.Client, region Region, username, password string) (Tokens, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return Tokens{}, fmt.Errorf("vul het mySigen-account en wachtwoord in")
	}
	baseURL, regionHeader, err := regionEndpoint(region)
	if err != nil {
		return Tokens{}, err
	}
	encoded, err := EncryptPassword(password)
	if err != nil {
		return Tokens{}, fmt.Errorf("mySigen-wachtwoord coderen: %w", err)
	}
	deviceID, err := randomID()
	if err != nil {
		return Tokens{}, fmt.Errorf("mySigen-aanmelding voorbereiden: %w", err)
	}
	form := url.Values{
		"scope":        {"server"},
		"grant_type":   {"password"},
		"userDeviceId": {deviceID},
		"username":     {username},
		"password":     {encoded},
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultCallTime)
	defer cancel()
	return requestTokens(callCtx, httpClient, baseURL, regionHeader, form, "")
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	// UUID v4. mySigen only needs a stable-looking unique id for this login;
	// refresh requests do not carry it.
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

type tokenPayload struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
}

func requestTokens(ctx context.Context, httpClient *http.Client, baseURL, regionHeader string, form url.Values, previousRefresh string) (Tokens, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"auth/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	setCommonHeaders(req, regionHeader)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(oauthClientID, oauthClientSecret)

	response, err := httpClient.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("mySigen-aanmelding: %w", err)
	}
	defer response.Body.Close()
	raw, err := readResponse(response.Body)
	if err != nil {
		return Tokens{}, fmt.Errorf("mySigen-aanmelding lezen: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Tokens{}, &APIError{Status: response.StatusCode, Message: envelopeMessage(raw, "mySigen weigert de aanmelding")}
	}

	data, err := envelopeData(raw)
	if err != nil {
		return Tokens{}, fmt.Errorf("mySigen-aanmelding: %w", err)
	}
	var payload tokenPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return Tokens{}, fmt.Errorf("mySigen stuurde een onleesbaar token: %w", err)
	}
	seconds, err := jsonInteger(payload.ExpiresIn)
	if err != nil || seconds <= 0 || payload.AccessToken == "" {
		return Tokens{}, fmt.Errorf("mySigen stuurde een onvolledig token")
	}
	if payload.RefreshToken == "" {
		payload.RefreshToken = previousRefresh
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	if deadline.After(time.Now().Add(refreshMargin)) {
		deadline = deadline.Add(-refreshMargin)
	} else {
		deadline = time.Now()
	}
	return Tokens{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, ExpiresAt: deadline}, nil
}

func jsonInteger(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("getal ontbreekt")
	}
	var number json.Number
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
		number = json.Number(text)
	} else {
		number = json.Number(string(raw))
	}
	return strconv.ParseInt(number.String(), 10, 64)
}

type tokenSession struct {
	mu     sync.Mutex
	tokens Tokens
	store  TokenStore
}

func (s *tokenSession) accessToken(ctx context.Context, httpClient *http.Client, baseURL, regionHeader string, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens.AccessToken == "" {
		return "", ErrNotAuthenticated
	}
	if now.Before(s.tokens.ExpiresAt) {
		return s.tokens.AccessToken, nil
	}
	if s.tokens.RefreshToken == "" {
		return "", fmt.Errorf("%w: het token is verlopen; koppel opnieuw", ErrNotAuthenticated)
	}
	form := url.Values{
		"scope":         {"server"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.tokens.RefreshToken},
	}
	refreshed, err := requestTokens(ctx, httpClient, baseURL, regionHeader, form, s.tokens.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("mySigen-token vernieuwen: %w", err)
	}
	s.tokens = refreshed
	if s.store != nil {
		if err := s.store(refreshed); err != nil {
			return "", fmt.Errorf("vernieuwd mySigen-token bewaren: %w", err)
		}
	}
	return refreshed.AccessToken, nil
}

func (s *tokenSession) snapshot() Tokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}
