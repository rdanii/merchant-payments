package ovo

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SNAP transport: B2B access token, request signing and the HTTP exchange.
// Signature details follow OVO's official Postman collection ("[SNAP]
// Dynamic QRIS V2"): both signatures are hex encoded, and the symmetric
// signature hashes the exact body bytes that are sent.

const (
	pathAccessToken = "/OVOSNAP/v1.0/access-token/b2b"

	// timestampLayout is the documented X-TIMESTAMP format
	// yyyy-MM-ddTHH:mm:ssTZD, rendered in the configured local time zone
	// ("Client's current local time").
	timestampLayout = "2006-01-02T15:04:05-07:00"

	// defaultTokenLifetime is OVO's documented B2B token expiry ("900"),
	// used only if a token response omits expiresIn.
	defaultTokenLifetime = 900 * time.Second
	// tokenRefreshMargin renews a token this long before it expires.
	tokenRefreshMargin = 60 * time.Second

	maxResponseBytes = 1 << 20
)

// signAccessToken returns the asymmetric X-SIGNATURE for the B2B token
// request: SHA256withRSA over "clientID|timestamp", hex encoded.
func signAccessToken(key *rsa.PrivateKey, clientID, timestamp string) (string, error) {
	digest := sha256.Sum256([]byte(clientID + "|" + timestamp))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("ovo: signing token request: %w", err)
	}
	return hex.EncodeToString(sig), nil
}

// signTransaction returns the symmetric X-SIGNATURE for transactional
// requests: HMAC-SHA512, keyed with the client secret, over
// "METHOD:path:accessToken:lowercase(hex(SHA-256(body))):timestamp", hex
// encoded.
func signTransaction(secret, method, path, accessToken string, body []byte, timestamp string) string {
	bodyHash := sha256.Sum256(body)
	msg := method + ":" + path + ":" + accessToken + ":" + hex.EncodeToString(bodyHash[:]) + ":" + timestamp
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// snapResult is a raw provider response.
type snapResult struct {
	status int
	body   []byte
}

// responseHeader holds the fields every SNAP response carries.
type responseHeader struct {
	ResponseCode    string `json:"responseCode"`
	ResponseMessage string `json:"responseMessage"`
}

// isSuccessCode reports whether a SNAP responseCode ("HTTP status + service
// code + case code") is a 200 success with case 00.
func isSuccessCode(code string) bool {
	return len(code) == 7 && code[:3] == "200" && code[5:] == "00"
}

// providerError describes a non-success OVO response. It never contains
// request headers, tokens, signatures or secrets.
type providerError struct {
	op      string
	status  int
	code    string
	message string
}

func (e *providerError) Error() string {
	msg := e.message
	if len(msg) > 80 {
		msg = msg[:80]
	}
	return fmt.Sprintf("ovo: %s: HTTP %d, responseCode %q (%s)", e.op, e.status, e.code, msg)
}

func newProviderError(op string, res snapResult) *providerError {
	var h responseHeader
	_ = json.Unmarshal(res.body, &h)
	return &providerError{op: op, status: res.status, code: h.ResponseCode, message: h.ResponseMessage}
}

// accessToken returns a cached B2B token, fetching a new one when none is
// cached or the cached one is within tokenRefreshMargin of expiry. Holding
// the mutex during the fetch means concurrent callers share one refresh.
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token != "" && a.now().Before(a.tokenExpiry.Add(-tokenRefreshMargin)) {
		return a.token, nil
	}
	requestedAt := a.now()
	token, lifetime, err := a.fetchAccessToken(ctx, requestedAt)
	if err != nil {
		return "", err
	}
	a.token, a.tokenExpiry = token, requestedAt.Add(lifetime)
	return token, nil
}

// invalidateToken drops token if it is still the cached one, so the next
// call fetches a new token.
func (a *Adapter) invalidateToken(token string) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token == token {
		a.token = ""
	}
}

type tokenResponse struct {
	responseHeader
	AccessToken string          `json:"accessToken"`
	TokenType   string          `json:"tokenType"`
	ExpiresIn   json.RawMessage `json:"expiresIn"`
}

func (a *Adapter) fetchAccessToken(ctx context.Context, now time.Time) (string, time.Duration, error) {
	timestamp := a.localTime(now).Format(timestampLayout)
	signature, err := signAccessToken(a.key, a.cfg.ClientID, timestamp)
	if err != nil {
		return "", 0, err
	}
	body := []byte(`{"grantType":"client_credentials"}`)
	headers := http.Header{}
	headers.Set("X-CLIENT-KEY", a.cfg.ClientID)
	headers.Set("X-TIMESTAMP", timestamp)
	headers.Set("X-SIGNATURE", signature)
	headers.Set("X-EXTERNAL-ID", a.newExternalID())
	headers.Set("CHANNEL-ID", a.cfg.ChannelID)

	res, err := a.send(ctx, "access token", pathAccessToken, body, headers)
	if err != nil {
		return "", 0, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(res.body, &tr); err != nil || res.status != http.StatusOK ||
		!isSuccessCode(tr.ResponseCode) || tr.AccessToken == "" {
		return "", 0, newProviderError("access token", res)
	}
	return tr.AccessToken, tokenLifetime(tr.ExpiresIn), nil
}

// tokenLifetime parses expiresIn, documented as the string "900"; a number
// is accepted too. Missing or invalid values fall back to the documented 900
// seconds.
func tokenLifetime(raw json.RawMessage) time.Duration {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return defaultTokenLifetime
		}
		s = n.String()
	}
	secs, err := strconv.Atoi(s)
	if err != nil || secs <= 0 {
		return defaultTokenLifetime
	}
	return time.Duration(secs) * time.Second
}

// transact sends a signed transactional request. On HTTP 401 (documented as
// "Invalid Token (B2B)") it drops the token, fetches a new one and retries
// exactly once with fresh headers.
//
// Each attempt reads the clock once, in the configured local time zone.
// build receives that instant, so any time-derived body field
// (transactionDate) and the X-TIMESTAMP header always come from the same
// instant and cannot fall on different sides of local midnight.
func (a *Adapter) transact(ctx context.Context, op, path string, build func(now time.Time) any) (snapResult, error) {
	for attempt := 1; ; attempt++ {
		token, err := a.accessToken(ctx)
		if err != nil {
			return snapResult{}, err
		}
		now := a.localTime(a.now())
		body, err := json.Marshal(build(now))
		if err != nil {
			return snapResult{}, fmt.Errorf("ovo: %s: encoding request: %w", op, err)
		}
		timestamp := now.Format(timestampLayout)
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+token)
		headers.Set("X-TIMESTAMP", timestamp)
		headers.Set("X-SIGNATURE", signTransaction(a.cfg.ClientSecret, http.MethodPost, path, token, body, timestamp))
		headers.Set("X-PARTNER-ID", a.cfg.PartnerID)
		headers.Set("X-EXTERNAL-ID", a.newExternalID())
		headers.Set("CHANNEL-ID", a.cfg.ChannelID)

		res, err := a.send(ctx, op, path, body, headers)
		if err != nil {
			return snapResult{}, err
		}
		if res.status == http.StatusUnauthorized && attempt == 1 {
			a.invalidateToken(token)
			continue
		}
		return res, nil
	}
}

// send performs one POST. Errors carry the operation and URL only.
func (a *Adapter) send(ctx context.Context, op, path string, body []byte, headers http.Header) (snapResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return snapResult{}, fmt.Errorf("ovo: %s: building request: %w", op, err)
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return snapResult{}, fmt.Errorf("ovo: %s: %w", op, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return snapResult{}, fmt.Errorf("ovo: %s: reading response: %w", op, err)
	}
	return snapResult{status: resp.StatusCode, body: respBody}, nil
}
