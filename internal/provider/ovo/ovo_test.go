package ovo

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/provider"
)

const (
	testClientID = "test-client-id"
	testSecret   = "test-client-secret-value"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recorded is one transactional request received by the fake.
type recorded struct {
	path   string
	header http.Header
	body   []byte
}

// fakeOVO is an httptest OVO server. It verifies every signature it
// receives, so all tests also exercise signing.
type fakeOVO struct {
	t      *testing.T
	srv    *httptest.Server
	pub    *rsa.PublicKey
	secret string

	mu           sync.Mutex
	tokenCalls   int
	tokenDelay   time.Duration
	tokenFailure bool // respond to token requests with an error
	requests     []recorded
	generate     func(r recorded, token string) (int, string)
	query        func(r recorded, token string) (int, string)
}

func newFakeOVO(t *testing.T) *fakeOVO {
	t.Helper()
	key, err := parsePrivateKey(readFile(t, "test_private_key_pkcs8.pem"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOVO{t: t, pub: &key.PublicKey, secret: testSecret}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOVO) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		f.t.Errorf("%s %s: unexpected method or content type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
	}
	ts := r.Header.Get("X-TIMESTAMP")
	if _, err := time.Parse(timestampLayout, ts); err != nil {
		f.t.Errorf("%s: X-TIMESTAMP %q not in yyyy-MM-ddTHH:mm:ssTZD format", r.URL.Path, ts)
	}
	if r.URL.Path == pathAccessToken {
		f.serveToken(w, r, body, ts)
		return
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	want := hmacHex(f.secret, "POST:"+r.URL.Path+":"+token+":"+sha256Hex(body)+":"+ts)
	if r.Header.Get("X-SIGNATURE") != want {
		f.t.Errorf("%s: X-SIGNATURE does not verify", r.URL.Path)
	}
	rec := recorded{path: r.URL.Path, header: r.Header.Clone(), body: body}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	handler := f.generate
	if r.URL.Path == pathQueryQR {
		handler = f.query
	}
	f.mu.Unlock()
	if handler == nil {
		f.t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	status, resp := handler(rec, token)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, resp)
}

func (f *fakeOVO) serveToken(w http.ResponseWriter, r *http.Request, body []byte, ts string) {
	sig, err := hex.DecodeString(r.Header.Get("X-SIGNATURE"))
	digest := sha256.Sum256([]byte(r.Header.Get("X-CLIENT-KEY") + "|" + ts))
	if err != nil || rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, digest[:], sig) != nil {
		f.t.Errorf("token X-SIGNATURE does not verify")
	}
	if string(body) != `{"grantType":"client_credentials"}` {
		f.t.Errorf("token body = %s", body)
	}
	f.mu.Lock()
	f.tokenCalls++
	n, delay, fail := f.tokenCalls, f.tokenDelay, f.tokenFailure
	f.mu.Unlock()
	time.Sleep(delay)
	if fail {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"responseCode":"4007301","responseMessage":"Invalid Field Format"}`)
		return
	}
	fmt.Fprintf(w, `{"responseCode":"2007300","responseMessage":"Successful","accessToken":"tok-%d","tokenType":"Bearer","expiresIn":"900","additionalInfo":{}}`, n)
}

func (f *fakeOVO) tokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func (f *fakeOVO) received() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacHex(secret, msg string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func testConfig(baseURL string) Config {
	return Config{
		BaseURL: baseURL, ClientID: testClientID, ClientSecret: testSecret,
		PartnerID: "test-partner-id", ChannelID: "test-channel", MerchantID: "DBCFJKTLPKN0001",
		TerminalID: "13000004", DeviceID: "12345679237", AllowedSources: "100",
		Location: time.UTC, // explicit; time-zone behavior is covered in timezone_test.go
	}
}

func newTestAdapter(t *testing.T, f *fakeOVO, mods ...func(*Config)) *Adapter {
	t.Helper()
	cfg := testConfig(f.srv.URL)
	cfg.PrivateKeyPEM = readFile(t, "test_private_key_pkcs8.pem")
	for _, m := range mods {
		m(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func generateOK(referenceNo, qr string, expiry int) func(recorded, string) (int, string) {
	return func(r recorded, _ string) (int, string) {
		var req generateRequest
		_ = json.Unmarshal(r.body, &req)
		return 200, fmt.Sprintf(`{"responseCode":"2004700","responseMessage":"Successful","referenceNo":%q,
			"partnerReferenceNo":%q,"qrContent":%q,"additionalInfo":{"batchNo":"cF9M96","expiry":%d}}`,
			referenceNo, req.PartnerReferenceNo, qr, expiry)
	}
}

func createReq() provider.CreateRequest {
	return provider.CreateRequest{
		MerchantReference: "6f1c1a52-4d1e-4b8a-9d4c-0d7f0e1a2b3c", Amount: 150000,
		Currency: "IDR", Method: domain.PaymentMethodQRIS,
	}
}

// --- signing ---

func TestSignTransactionKnownAnswer(t *testing.T) {
	// Expected values computed independently with Python's hashlib/hmac,
	// following the official Postman pre-request script.
	body := []byte(`{"originalReferenceNo":"H2H7a2617ccf0fe4ba2b7295b7c1f99","serviceCode":"47"}`)
	if got := sha256Hex(body); got != "fd058c6b7fbb1f1b4ec9b1279f08b245c60edb0fa7d1aba23f153a818c8418c4" {
		t.Fatalf("body hash = %s", got)
	}
	got := signTransaction("test-client-secret", "POST", "/OVOSNAP/v2.0/qr/qr-mpm-query", "test-access-token",
		body, "2026-10-04T12:00:00+00:00")
	const want = "959d1154ec4b4b49830d4b149d0f1a24c8b8111c2db6779931d86a5ed38bf869272e9c68edb6f9344986e76123c0dcde5351b18bbbee11dde9f0c8257152eea8"
	if got != want {
		t.Fatalf("signature = %s\nwant        %s", got, want)
	}
}

func TestSignAccessTokenKnownAnswer(t *testing.T) {
	// Expected value computed independently with:
	//   printf '%s' 'test-client-id|2026-10-04T12:00:00+00:00' |
	//     openssl dgst -sha256 -sign testdata/test_private_key_pkcs8.pem | od -An -tx1
	const want = "4833c480c85ec7d54b6e210930c3c40a47acbb88dcd974bdaa94caaac3555a756e7c400bf6474e3d8c893fd623b7f8b64b0c9b1b6477595099e5fb73e494d509" +
		"8f3e0e91a09e7de1e9fe35a5e4612e8d422ad9495d6ef794eeefc9cbd8e982d9619635058baf11fdd68f19ef249ca129d7131af9e9005720b12db5475f36aab5" +
		"d823372732bb62334c39477769d1bbf434457d28f9e22eadbb51973936fe3e47fa4439ca88357270fbc6937d49ac12f2c3eb04531f1533846ef9e763fe2af8bc" +
		"de783652fd6a0c700a1b521899890de229e8c0a481cc02fb758ff0b6ac25f8ca4ec501a2dadd202ba18f3aae5740b8053e1b12622881c975cd1df28b20a3947c"
	for _, file := range []string{"test_private_key_pkcs8.pem", "test_private_key_pkcs1.pem"} {
		key, err := parsePrivateKey(readFile(t, file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := signAccessToken(key, "test-client-id", "2026-10-04T12:00:00+00:00")
		if err != nil || got != want {
			t.Fatalf("%s: signature mismatch (err=%v)", file, err)
		}
	}
}

// --- token ---

func TestTokenReuseAndProactiveRefresh(t *testing.T) {
	f := newFakeOVO(t)
	f.generate = generateOK("ref-1", "qr", 75)
	a := newTestAdapter(t, f)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if _, err := a.CreatePayment(ctx, createReq()); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokens() != 1 {
		t.Fatalf("token fetched %d times; want reuse", f.tokens())
	}

	now = now.Add(900*time.Second - tokenRefreshMargin - time.Second) // still valid
	if _, err := a.CreatePayment(ctx, createReq()); err != nil || f.tokens() != 1 {
		t.Fatalf("token refreshed too early: %d fetches, %v", f.tokens(), err)
	}
	now = now.Add(2 * time.Second) // inside the refresh margin
	if _, err := a.CreatePayment(ctx, createReq()); err != nil || f.tokens() != 2 {
		t.Fatalf("token not refreshed proactively: %d fetches, %v", f.tokens(), err)
	}
	reqs := f.received()
	if got := reqs[len(reqs)-1].header.Get("Authorization"); got != "Bearer tok-2" {
		t.Fatalf("refreshed token not used: %q", got)
	}
}

func TestTokenSingleRefreshUnderConcurrency(t *testing.T) {
	f := newFakeOVO(t)
	f.tokenDelay = 50 * time.Millisecond
	a := newTestAdapter(t, f)
	var wg sync.WaitGroup
	tokens := make([]string, 20)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], _ = a.accessToken(context.Background())
		}()
	}
	wg.Wait()
	if f.tokens() != 1 {
		t.Fatalf("concurrent callers fetched %d tokens; want 1", f.tokens())
	}
	for _, tok := range tokens {
		if tok != "tok-1" {
			t.Fatalf("caller got %q", tok)
		}
	}
}

func TestReactiveRefreshOnce(t *testing.T) {
	f := newFakeOVO(t)
	ok := generateOK("ref-1", "qr", 75)
	f.generate = func(r recorded, token string) (int, string) {
		if token == "tok-1" { // the cached token was revoked by OVO
			return 401, `{"responseCode":"4014701","responseMessage":"Invalid Token (B2B)"}`
		}
		return ok(r, token)
	}
	a := newTestAdapter(t, f)
	res, err := a.CreatePayment(context.Background(), createReq())
	if err != nil || res.ProviderReference != "ref-1" {
		t.Fatalf("got %+v, %v", res, err)
	}
	reqs := f.received()
	if f.tokens() != 2 || len(reqs) != 2 {
		t.Fatalf("tokens=%d requests=%d; want one refresh and one retry", f.tokens(), len(reqs))
	}
	if reqs[0].header.Get("X-EXTERNAL-ID") == reqs[1].header.Get("X-EXTERNAL-ID") {
		t.Fatal("retry must use a new X-EXTERNAL-ID")
	}

	// A token that is still rejected after one refresh is not retried again.
	f2 := newFakeOVO(t)
	f2.generate = func(recorded, string) (int, string) {
		return 401, `{"responseCode":"4014701","responseMessage":"Invalid Token (B2B)"}`
	}
	a2 := newTestAdapter(t, f2)
	_, err = a2.CreatePayment(context.Background(), createReq())
	if !errors.Is(err, provider.ErrRejected) || f2.tokens() != 2 || len(f2.received()) != 2 {
		t.Fatalf("err=%v tokens=%d requests=%d; want rejection after exactly one retry", err, f2.tokens(), len(f2.received()))
	}
}

// --- generate QR ---

func TestGenerateQRRequest(t *testing.T) {
	f := newFakeOVO(t)
	f.generate = generateOK("ref-1", "qr", 75)
	now := time.Date(2026, 10, 4, 23, 30, 0, 0, time.UTC)
	a := newTestAdapter(t, f, func(c *Config) { c.QRExpirySeconds = 300 })
	a.now = func() time.Time { return now }
	if _, err := a.CreatePayment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreatePayment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	reqs := f.received()
	r := reqs[0]
	if r.path != "/OVOSNAP/v2.0/qr/qr-mpm-generate" {
		t.Fatalf("path = %s", r.path)
	}
	for h, want := range map[string]string{
		"Authorization": "Bearer tok-1", "X-PARTNER-ID": "test-partner-id", "CHANNEL-ID": "test-channel",
		"X-TIMESTAMP": "2026-10-04T23:30:00+00:00",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	ext := r.header.Get("X-EXTERNAL-ID")
	if ext == "" || len(ext) > 36 || ext == reqs[1].header.Get("X-EXTERNAL-ID") {
		t.Errorf("X-EXTERNAL-ID %q must be unique and at most 36 characters", ext)
	}

	var body map[string]any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"partnerReferenceNo": "6f1c1a52-4d1e-4b8a-9d4c-0d7f0e1a2b3c",
		"terminalId":         "13000004",
		"merchantId":         "DBCFJKTLPKN0001",
		"amount":             map[string]any{"value": "150000.00", "currency": "IDR"},
		"additionalInfo": map[string]any{
			"deviceId": "12345679237", "channel": "OVO", "transactionDate": "261004", "allowedSources": "100",
		},
		"expiry": float64(300),
	}
	if got, _ := json.Marshal(body); string(got) != mustJSON(t, want) {
		t.Fatalf("body = %s\nwant   %s", got, mustJSON(t, want))
	}

	// Without a configured expiry the field is omitted (OVO default 75s).
	f2 := newFakeOVO(t)
	f2.generate = generateOK("ref-2", "qr", 75)
	if _, err := newTestAdapter(t, f2).CreatePayment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(f2.received()[0].body), `"expiry"`) {
		t.Fatal("expiry must be omitted when not configured")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerateQRPostmanResponse(t *testing.T) {
	fixture := readFile(t, "postman_qr_generate_response.json")
	var sample struct {
		ReferenceNo, PartnerReferenceNo, QRContent string
	}
	if err := json.Unmarshal(fixture, &sample); err != nil {
		t.Fatal(err)
	}
	f := newFakeOVO(t)
	f.generate = func(recorded, string) (int, string) { return 200, string(fixture) }
	a := newTestAdapter(t, f)
	sentAt := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return sentAt }

	req := createReq()
	req.MerchantReference = sample.PartnerReferenceNo // the sample echoes this reference
	res, err := a.CreatePayment(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderReference != sample.ReferenceNo {
		t.Fatalf("provider reference = %q", res.ProviderReference)
	}
	if res.Action.Type != domain.ActionQRString || res.Action.Value != sample.QRContent {
		t.Fatalf("QR content must be returned unchanged; got %+v", res.Action)
	}
	if res.ExpiresAt == nil || !res.ExpiresAt.Equal(sentAt.Add(75*time.Second)) {
		t.Fatalf("expires at = %v", res.ExpiresAt)
	}
}

func TestGenerateQRExpiry(t *testing.T) {
	sentAt := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		configured int
		response   string
		want       time.Duration
	}{
		{"provider value", 0, `300`, 300 * time.Second},
		{"provider upper bound", 0, `86400`, 86400 * time.Second},
		{"missing, documented default", 0, `null`, 75 * time.Second},
		{"missing, configured value", 600, `null`, 600 * time.Second},
		{"below bound ignored", 600, `10`, 600 * time.Second},
		{"above bound ignored", 0, `100000`, 75 * time.Second},
		{"not a number ignored", 0, `"soon"`, 75 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOVO(t)
			f.generate = func(r recorded, _ string) (int, string) {
				var req generateRequest
				_ = json.Unmarshal(r.body, &req)
				return 200, `{"responseCode":"2004700","referenceNo":"ref","partnerReferenceNo":"` + req.PartnerReferenceNo +
					`","qrContent":"qr","additionalInfo":{"expiry":` + tt.response + `}}`
			}
			a := newTestAdapter(t, f, func(c *Config) { c.QRExpirySeconds = tt.configured })
			a.now = func() time.Time { return sentAt }
			res, err := a.CreatePayment(context.Background(), createReq())
			if err != nil || !res.ExpiresAt.Equal(sentAt.Add(tt.want)) {
				t.Fatalf("expires at = %v (%v); want %v", res.ExpiresAt, err, sentAt.Add(tt.want))
			}
		})
	}
}

func TestGenerateQRErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		rejected bool
	}{
		{"bad request", 400, `{"responseCode":"4004701","responseMessage":"Invalid Field Format"}`, true},
		{"invalid merchant", 404, `{"responseCode":"4044708","responseMessage":"Invalid Merchant"}`, true},
		{"pending limit", 429, `{"responseCode":"4294700","responseMessage":"Too Many Request"}`, true},
		{"duplicate may already exist", 409, `{"responseCode":"4094700","responseMessage":"Duplicate partnerReferenceNo"}`, false},
		{"provider timeout", 500, `{"responseCode":"5004704","responseMessage":"Timeout"}`, false},
		{"general error", 500, `{"responseCode":"5004700","responseMessage":"General Error"}`, false},
		{"200 without success code", 200, `{"responseCode":"2024700","responseMessage":"Accepted"}`, false},
		{"missing referenceNo", 200, `{"responseCode":"2004700","qrContent":"qr"}`, false},
		{"missing qrContent", 200, `{"responseCode":"2004700","referenceNo":"ref"}`, false},
		{"foreign partnerReferenceNo", 200, `{"responseCode":"2004700","referenceNo":"ref","qrContent":"qr","partnerReferenceNo":"other"}`, false},
		{"not JSON", 502, `<html>bad gateway</html>`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOVO(t)
			f.generate = func(recorded, string) (int, string) { return tt.status, tt.body }
			_, err := newTestAdapter(t, f).CreatePayment(context.Background(), createReq())
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, provider.ErrRejected) != tt.rejected {
				t.Fatalf("rejected = %v, want %v (%v)", errors.Is(err, provider.ErrRejected), tt.rejected, err)
			}
		})
	}
}

func TestGenerateQRTransportFailuresAreUnknown(t *testing.T) {
	f := newFakeOVO(t)
	f.generate = func(recorded, string) (int, string) {
		time.Sleep(200 * time.Millisecond)
		return 200, `{}`
	}
	a := newTestAdapter(t, f)
	if _, err := a.accessToken(context.Background()); err != nil { // warm the token
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := a.CreatePayment(ctx, createReq())
	if err == nil || errors.Is(err, provider.ErrRejected) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must be an unknown outcome, got %v", err)
	}

	closed := newFakeOVO(t)
	a2 := newTestAdapter(t, closed)
	closed.srv.Close()
	if _, err := a2.CreatePayment(context.Background(), createReq()); err == nil || errors.Is(err, provider.ErrRejected) {
		t.Fatalf("network failure must be an unknown outcome, got %v", err)
	}
}

func TestCreatePaymentUnsupportedRequests(t *testing.T) {
	f := newFakeOVO(t)
	a := newTestAdapter(t, f)
	for _, m := range []domain.PaymentMethod{domain.PaymentMethodEWallet, domain.PaymentMethodPayLater} {
		if a.SupportsMethod(m) {
			t.Errorf("method %s must not be supported", m)
		}
	}
	if !a.SupportsMethod(domain.PaymentMethodQRIS) {
		t.Error("QRIS must be supported")
	}
	req := createReq()
	req.Currency = "USD"
	if _, err := a.CreatePayment(context.Background(), req); !errors.Is(err, provider.ErrRejected) {
		t.Fatalf("USD: got %v", err)
	}
	if f.tokens() != 0 || len(f.received()) != 0 {
		t.Fatal("unsupported requests must not reach OVO")
	}
}

// --- inquiry ---

func inquiryBody(status, value, currency, summary string) string {
	amount := `{"value":"` + value + `"`
	if currency != "" {
		amount += `,"currency":"` + currency + `"`
	}
	amount += `}`
	return `{"responseCode":"2005100","responseMessage":"Successful","originalReferenceNo":"ref-1",
		"originalPartnerReferenceNo":"pref-1","serviceCode":"47","latestTransactionStatus":"` + status + `",
		"paidTime":"2026-10-04T12:01:02.000+0000","amount":` + amount + `,
		"additionalInfo":{"transactionId":"7178","userName":"H**** L*******","phoneNumber":"********2580",
		"customerPan":"","merchantPan":"9630570000051061610","deviceId":"dev","transactionSummary":` + summary + `}}`
}

func statusReq() provider.StatusRequest {
	return provider.StatusRequest{MerchantReference: "pref-1", ProviderReference: "ref-1"}
}

func TestInquiryRequest(t *testing.T) {
	f := newFakeOVO(t)
	f.query = func(recorded, string) (int, string) {
		return 200, inquiryBody("03", "10000.00", "IDR", `{"points":"0","cash":"0","payLater":"0"}`)
	}
	if _, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(), statusReq()); err != nil {
		t.Fatal(err)
	}
	r := f.received()[0]
	if r.path != "/OVOSNAP/v2.0/qr/qr-mpm-query" {
		t.Fatalf("path = %s", r.path)
	}
	want := `{"originalReferenceNo":"ref-1","originalPartnerReferenceNo":"pref-1","serviceCode":"47",` +
		`"merchantId":"DBCFJKTLPKN0001","additionalInfo":{"deviceId":"12345679237","channel":"OVO","terminalId":"13000004"}}`
	if string(r.body) != want {
		t.Fatalf("body = %s\nwant   %s", r.body, want)
	}
}

func TestInquiryStatusMapping(t *testing.T) {
	summary := `{"points":"0","cash":"10000","payLater":"0"}`
	tests := []struct {
		code string
		want domain.PaymentStatus
	}{
		{"00", domain.PaymentStatusPaid},
		{"03", domain.PaymentStatusPending},
		{"06", domain.PaymentStatusFailed},
		{"01", ""}, // refund pending: unsupported
		{"04", ""}, // refund success: unsupported
		{"99", ""}, // undocumented
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			f := newFakeOVO(t)
			f.query = func(recorded, string) (int, string) { return 200, inquiryBody(tt.code, "10000.00", "IDR", summary) }
			r, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(), statusReq())
			if tt.want == "" {
				if !errors.Is(err, errStatusUnsupported) {
					t.Fatalf("want unsupported status error, got %+v, %v", r, err)
				}
				return
			}
			if err != nil || r.Status != tt.want {
				t.Fatalf("got %s, %v", r.Status, err)
			}
			if r.Provider != Name || r.ProviderReference != "ref-1" || r.MerchantReference != "pref-1" ||
				*r.Amount != 10000 || r.Currency != "IDR" || r.ProviderEventID != "" {
				t.Fatalf("unexpected report %+v", r)
			}
			if wantFS := domain.FundingSourceOVOCash; tt.want == domain.PaymentStatusPaid && r.FundingSource != wantFS ||
				tt.want != domain.PaymentStatusPaid && r.FundingSource != domain.FundingSourceUnknown {
				t.Fatalf("funding source %s for status %s", r.FundingSource, tt.want)
			}
		})
	}
}

func TestInquiryAmountParsing(t *testing.T) {
	for value, want := range map[string]int64{"10000.00": 10000, "3000": 3000, "10000.0": 10000, "0.00": 0} {
		if got, err := parseAmount(value); err != nil || got != want {
			t.Errorf("parseAmount(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, bad := range []string{"10000.50", "10000.5", "10000.505", "1e4", "-1", "10,000.00", " 1000", ""} {
		if _, err := parseAmount(bad); err == nil {
			t.Errorf("parseAmount(%q) must fail", bad)
		}
	}
	if s, err := formatAmount(150000); err != nil || s != "150000.00" {
		t.Errorf("formatAmount = %q, %v", s, err)
	}

	// Inquiry with a fractional rupiah amount is unusable, not guessed.
	f := newFakeOVO(t)
	f.query = func(recorded, string) (int, string) {
		return 200, inquiryBody("00", "10000.50", "IDR", `{"points":"0","cash":"10000","payLater":"0"}`)
	}
	if _, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(), statusReq()); err == nil {
		t.Fatal("invalid amount precision must fail")
	}
}

func TestPaidTimeParsing(t *testing.T) {
	tests := map[string]time.Time{
		"2023-05-02T10:37:53.371+0000": time.Date(2023, 5, 2, 10, 37, 53, 371e6, time.UTC), // Postman example
		"2023-05-09T18:02:27+07:00":    time.Date(2023, 5, 9, 11, 2, 27, 0, time.UTC),
		"2023-05-09T11:02:27Z":         time.Date(2023, 5, 9, 11, 2, 27, 0, time.UTC),
		"2023-05-09T18:02:27":          {}, // documented example without a zone: not converted
		"20230509180227":               {}, // documented yyyyMMddHHmmss without a zone: not converted
		"":                             {},
		"yesterday":                    {},
	}
	for in, want := range tests {
		if got := parsePaidTime(in); !got.Equal(want) {
			t.Errorf("parsePaidTime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFundingSourceMapping(t *testing.T) {
	const amt = 10000
	tests := []struct {
		name    string
		top     string // top-level transactionSummary, "" = absent
		nested  string // additionalInfo.transactionSummary, "" = absent
		want    domain.FundingSource
		comment string
	}{
		{"cash only", "", `{"points":"0","cash":"10000","payLater":"0"}`, domain.FundingSourceOVOCash, ""},
		{"payLater only", "", `{"points":"0","cash":"0","payLater":"10000"}`, domain.FundingSourceOVOPayLater, ""},
		{"payLater with decimals", "", `{"points":"0.00","cash":"0.00","payLater":"10000.00"}`, domain.FundingSourceOVOPayLater, ""},
		{"top-level location", `{"points":"0","cash":"0","payLater":"10000"}`, "", domain.FundingSourceOVOPayLater, ""},
		{"both locations agree", `{"points":"0","cash":"10000","payLater":"0"}`, `{"points":"0","cash":"10000","payLater":"0"}`, domain.FundingSourceOVOCash, ""},
		{"both locations conflict", `{"points":"0","cash":"10000","payLater":"0"}`, `{"points":"0","cash":"0","payLater":"10000"}`, domain.FundingSourceUnknown, ""},
		{"points only", "", `{"points":"10000","cash":"0","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"mixed cash and points", "", `{"points":"2000","cash":"8000","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"mixed cash and payLater", "", `{"points":"0","cash":"5000","payLater":"5000"}`, domain.FundingSourceUnknown, ""},
		{"all zero", "", `{"points":"0","cash":"0","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"missing component", "", `{"cash":"10000","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"missing summary", "", "", domain.FundingSourceUnknown, ""},
		{"number instead of string", "", `{"points":"0","cash":10000,"payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"malformed value", "", `{"points":"0","cash":"ten thousand","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"fractional value", "", `{"points":"0","cash":"10000.50","payLater":"0"}`, domain.FundingSourceUnknown, ""},
		{"does not reconcile", "", `{"points":"0","cash":"20000","payLater":"0"}`, domain.FundingSourceUnknown,
			"as in OVO's documented example: cash 20000 for amount 10000.00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp queryResponse
			resp.ResponseCode, resp.LatestTransactionStatus = "2005100", "00"
			resp.OriginalReferenceNo, resp.OriginalPartnerReferenceNo = "ref-1", "pref-1"
			resp.Amount = &amount{Value: "10000.00", Currency: "IDR"}
			raw := `{"additionalInfo":{}}`
			if tt.top != "" || tt.nested != "" {
				raw = `{"transactionSummary":` + orNull(tt.top) + `,"additionalInfo":{"transactionSummary":` + orNull(tt.nested) + `}}`
			}
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				t.Fatal(err)
			}
			r, err := reportFromInquiry(resp)
			if err != nil || *r.Amount != amt || r.FundingSource != tt.want {
				t.Fatalf("funding source = %s (%v); want %s %s", r.FundingSource, err, tt.want, tt.comment)
			}
		})
	}
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func TestInquiryPostmanResponse(t *testing.T) {
	fixture := readFile(t, "postman_inquiry_response.json")
	f := newFakeOVO(t)
	f.query = func(recorded, string) (int, string) { return 200, string(fixture) }
	r, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(),
		provider.StatusRequest{MerchantReference: "TRX-812323019050", ProviderReference: "H2H7a2617ccf0fe4ba2b7295b7c1f99"})
	if err != nil {
		t.Fatal(err)
	}
	// The official sample uses responseCode 2005300 (the documentation says
	// 2005100) and omits amount.currency.
	if r.Status != domain.PaymentStatusPaid || r.ProviderReference != "H2H7a2617ccf0fe4ba2b7295b7c1f99" ||
		r.MerchantReference != "TRX-812323019050" || *r.Amount != 3000 || r.Currency != "" {
		t.Fatalf("unexpected report %+v", r)
	}
	if r.FundingSource != domain.FundingSourceOVOCash {
		t.Fatalf("funding source = %s", r.FundingSource)
	}
	if !r.OccurredAt.Equal(time.Date(2023, 5, 2, 10, 37, 53, 371e6, time.UTC)) {
		t.Fatalf("occurred at = %v", r.OccurredAt)
	}
	payload := string(r.Payload)
	for _, leaked := range []string{"NAME-REDACTED", "userName", "phoneNumber", "2580", "merchantPan", "936009120000003547", "customerPan", "0001100009537395"} {
		if strings.Contains(payload, leaked) {
			t.Errorf("audit payload contains %q: %s", leaked, payload)
		}
	}
	for _, kept := range []string{`"transactionId":"6983"`, `"latestTransactionStatus":"00"`, `"cash":"3000"`, `"paidTime":"2023-05-02T10:37:53.371+0000"`} {
		if !strings.Contains(payload, kept) {
			t.Errorf("audit payload lacks %s: %s", kept, payload)
		}
	}
}

func TestInquiryFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"transaction not found", 404, `{"responseCode":"4045101","responseMessage":"Transaction Not Found"}`},
		{"provider timeout", 500, `{"responseCode":"5005104","responseMessage":"Timeout"}`},
		{"200 without success code", 200, `{"responseCode":"2025100"}`},
		{"missing references", 200, `{"responseCode":"2005100","latestTransactionStatus":"00","amount":{"value":"1.00","currency":"IDR"}}`},
		{"not JSON", 200, `oops`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOVO(t)
			f.query = func(recorded, string) (int, string) { return tt.status, tt.body }
			if _, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(), statusReq()); err == nil {
				t.Fatal("want error")
			}
		})
	}

	f := newFakeOVO(t)
	_, err := newTestAdapter(t, f).GetPaymentStatus(context.Background(), provider.StatusRequest{MerchantReference: "pref-1"})
	if err == nil || len(f.received()) != 0 || f.tokens() != 0 {
		t.Fatalf("inquiry without OVO referenceNo must fail locally: %v", err)
	}
}

// --- callbacks and configuration ---

func TestParseWebhookUnsupported(t *testing.T) {
	a := newTestAdapter(t, newFakeOVO(t))
	_, err := a.ParseWebhook(context.Background(), provider.Webhook{Body: []byte(`{"latestTransactionStatus":"00"}`)})
	if !errors.Is(err, provider.ErrInvalidSignature) {
		t.Fatalf("got %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	key := readFile(t, "test_private_key_pkcs1.pem")
	base := testConfig("https://ovo.example")
	base.PrivateKeyPEM = key
	if _, err := New(base); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := map[string]func(*Config){
		"missing secret":        func(c *Config) { c.ClientSecret = "" },
		"missing merchant":      func(c *Config) { c.MerchantID = "" },
		"missing device":        func(c *Config) { c.DeviceID = "" },
		"base URL with path":    func(c *Config) { c.BaseURL = "https://ovo.example/OVOSNAP" },
		"relative base URL":     func(c *Config) { c.BaseURL = "ovo.example" },
		"allowed sources chars": func(c *Config) { c.AllowedSources = "1a0" },
		"no allowed source":     func(c *Config) { c.AllowedSources = "000" },
		"expiry too short":      func(c *Config) { c.QRExpirySeconds = 74 },
		"expiry too long":       func(c *Config) { c.QRExpirySeconds = 86401 },
		"not a PEM key":         func(c *Config) { c.PrivateKeyPEM = []byte(testSecret) },
		"missing time zone":     func(c *Config) { c.Location = nil },
	}
	for name, mod := range tests {
		cfg := base
		mod(&cfg)
		_, err := New(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
}
