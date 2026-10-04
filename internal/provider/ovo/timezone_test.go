package ovo

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sentRequest is a request exactly as it left the adapter.
type sentRequest struct {
	path   string
	header http.Header
	body   []byte
}

// capturingTransport records outgoing requests before passing them on.
type capturingTransport struct {
	mu   sync.Mutex
	sent []sentRequest
}

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	c.mu.Lock()
	c.sent = append(c.sent, sentRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

// generateIn creates one QR at instant now with the adapter in loc and
// returns the token and generate requests as sent.
func generateIn(t *testing.T, loc *time.Location, now time.Time) (token, generate sentRequest) {
	t.Helper()
	f := newFakeOVO(t)
	f.generate = generateOK("ref-1", "qr", 75)
	tr := &capturingTransport{}
	a := newTestAdapter(t, f, func(c *Config) {
		c.Location = loc
		c.HTTPClient = &http.Client{Transport: tr}
	})
	a.now = func() time.Time { return now }
	if _, err := a.CreatePayment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != 2 || tr.sent[0].path != pathAccessToken || tr.sent[1].path != pathGenerateQR {
		t.Fatalf("unexpected requests: %+v", tr.sent)
	}
	return tr.sent[0], tr.sent[1]
}

func transactionDate(t *testing.T, body []byte) string {
	t.Helper()
	var req generateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	return req.AdditionalInfo.TransactionDate
}

func TestTimestampAndTransactionDateUseConfiguredZone(t *testing.T) {
	// 17:30 UTC on 4 October is already 5 October at UTC+07:00, so the date
	// boundary shows which zone was used.
	instant := time.Date(2026, 10, 4, 17, 30, 0, 0, time.UTC)
	tests := []struct {
		name          string
		loc           *time.Location
		wantTimestamp string
		wantDate      string
	}{
		{"UTC+07:00 crosses midnight", time.FixedZone("UTC+7", 7*3600), "2026-10-05T00:30:00+07:00", "261005"},
		{"UTC-05:00", time.FixedZone("UTC-5", -5*3600), "2026-10-04T12:30:00-05:00", "261004"},
		{"UTC", time.UTC, "2026-10-04T17:30:00+00:00", "261004"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, generate := generateIn(t, tt.loc, instant)
			for _, r := range []sentRequest{token, generate} {
				if got := r.header.Get("X-TIMESTAMP"); got != tt.wantTimestamp {
					t.Errorf("%s X-TIMESTAMP = %q, want %q", r.path, got, tt.wantTimestamp)
				}
			}
			if got := transactionDate(t, generate.body); got != tt.wantDate {
				t.Errorf("transactionDate = %q, want %q", got, tt.wantDate)
			}
		})
	}
}

func TestTransactionDateDateBoundary(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	// Local 23:59:59 and 00:00:00 one second later.
	before := time.Date(2026, 10, 4, 16, 59, 59, 0, time.UTC)
	_, g1 := generateIn(t, loc, before)
	_, g2 := generateIn(t, loc, before.Add(time.Second))
	if d1, d2 := transactionDate(t, g1.body), transactionDate(t, g2.body); d1 != "261004" || d2 != "261005" {
		t.Fatalf("transactionDate across local midnight = %s, %s; want 261004, 261005", d1, d2)
	}
	if ts := g2.header.Get("X-TIMESTAMP"); ts != "2026-10-05T00:00:00+07:00" {
		t.Fatalf("X-TIMESTAMP = %q", ts)
	}
}

func TestSignaturesUseEmittedTimestamp(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	token, generate := generateIn(t, loc, time.Date(2026, 10, 4, 17, 30, 0, 0, time.UTC))

	// Token: SHA256withRSA over "clientID|<X-TIMESTAMP as sent>".
	key, err := parsePrivateKey(readFile(t, "test_private_key_pkcs8.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ts := token.header.Get("X-TIMESTAMP")
	sig, _ := hex.DecodeString(token.header.Get("X-SIGNATURE"))
	digest := sha256.Sum256([]byte(testClientID + "|" + ts))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Fatal("token signature does not cover the emitted X-TIMESTAMP")
	}

	// Transaction: HMAC over the emitted X-TIMESTAMP and the exact body sent.
	ts = generate.header.Get("X-TIMESTAMP")
	accessToken := strings.TrimPrefix(generate.header.Get("Authorization"), "Bearer ")
	want := signTransaction(testSecret, http.MethodPost, pathGenerateQR, accessToken, generate.body, ts)
	if generate.header.Get("X-SIGNATURE") != want {
		t.Fatal("transaction signature does not cover the emitted X-TIMESTAMP")
	}
	// The same request signed with the UTC rendering of that instant must
	// not match: the signature is bound to the local-time bytes.
	utc := time.Date(2026, 10, 4, 17, 30, 0, 0, time.UTC).Format(timestampLayout)
	if signTransaction(testSecret, http.MethodPost, pathGenerateQR, accessToken, generate.body, utc) == want {
		t.Fatal("signature unexpectedly independent of the timestamp")
	}
}

func TestNoUTCWhenAnotherZoneIsConfigured(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta") // a named zone, not a fixed offset
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}
	token, generate := generateIn(t, jakarta, time.Date(2026, 10, 4, 17, 30, 0, 0, time.UTC))
	for _, r := range []sentRequest{token, generate} {
		ts := r.header.Get("X-TIMESTAMP")
		if strings.HasSuffix(ts, "+00:00") || strings.HasSuffix(ts, "Z") || !strings.HasSuffix(ts, "+07:00") {
			t.Errorf("%s X-TIMESTAMP = %q; want the configured zone's offset, not UTC", r.path, ts)
		}
	}
	if d := transactionDate(t, generate.body); d != "261005" {
		t.Errorf("transactionDate = %q; want the configured zone's date 261005", d)
	}
}

// tickingClock returns a later instant on every read, so separate clock
// reads within one request would observe different times.
type tickingClock struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

func (c *tickingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.next
	c.next = c.next.Add(c.step)
	return t
}

// dateOfTimestamp is the local calendar date (YYMMDD) of an X-TIMESTAMP.
func dateOfTimestamp(t *testing.T, ts string) string {
	t.Helper()
	parsed, err := time.Parse(timestampLayout, ts)
	if err != nil {
		t.Fatalf("X-TIMESTAMP %q: %v", ts, err)
	}
	return parsed.Format("060102")
}

func TestTimestampAndTransactionDateComeFromOneInstant(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	midnight := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)

	// Frozen clock on either side of local midnight.
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{midnight.Add(-time.Millisecond), "261004"}, // 23:59:59.999
		{midnight, "261005"},                        // 00:00:00.000
	} {
		_, g := generateIn(t, loc, c.at)
		ts := g.header.Get("X-TIMESTAMP")
		if dateOfTimestamp(t, ts) != c.want || transactionDate(t, g.body) != c.want {
			t.Errorf("at %s: X-TIMESTAMP %s, transactionDate %s; want both on %s",
				c.at.Format(time.RFC3339Nano), ts, transactionDate(t, g.body), c.want)
		}
	}

	// A clock that advances on every read, started at different offsets so
	// that local midnight falls between every pair of reads, with and
	// without a token-refresh retry.
	const step = 400 * time.Millisecond
	for _, retry := range []bool{false, true} {
		for k := 0; k <= 10; k++ {
			f := newFakeOVO(t)
			ok := generateOK("ref-1", "qr", 75)
			f.generate = func(r recorded, token string) (int, string) {
				if retry && token == "tok-1" {
					return 401, `{"responseCode":"4014701","responseMessage":"Invalid Token (B2B)"}`
				}
				return ok(r, token)
			}
			a := newTestAdapter(t, f, func(c *Config) { c.Location = loc })
			clock := &tickingClock{next: midnight.Add(-time.Duration(k) * step), step: step}
			a.now = clock.now
			if _, err := a.CreatePayment(context.Background(), createReq()); err != nil {
				t.Fatal(err)
			}
			reqs := f.received()
			if want := map[bool]int{false: 1, true: 2}[retry]; len(reqs) != want {
				t.Fatalf("retry=%v: %d generate requests, want %d", retry, len(reqs), want)
			}
			for i, r := range reqs {
				ts := r.header.Get("X-TIMESTAMP")
				if d := transactionDate(t, r.body); dateOfTimestamp(t, ts) != d {
					t.Errorf("retry=%v offset=%d attempt=%d: X-TIMESTAMP %s but transactionDate %s",
						retry, k, i+1, ts, d)
				}
			}
		}
	}
}
