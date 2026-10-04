package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/httpapi"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/payment/paymenttest"
	"merchant-payments/internal/provider"
	"merchant-payments/internal/provider/devsim"
	"merchant-payments/internal/provider/providertest"
	"merchant-payments/internal/requestid"
)

type apiClient struct {
	t   *testing.T
	srv *httptest.Server
	sim *devsim.Adapter
}

func newAPI(t *testing.T) *apiClient {
	return newAPIWith(t, io.Discard)
}

// newAPIWith builds a server with the simulator plus extra adapters, logging
// through requestid.Handler into logs.
func newAPIWith(t *testing.T, logs io.Writer, extra ...provider.Adapter) *apiClient {
	t.Helper()
	sim, err := devsim.New("test-secret-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := provider.NewRegistry(append([]provider.Adapter{sim}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(requestid.Handler{Handler: slog.NewJSONHandler(logs, nil)})
	svc := payment.NewService(paymenttest.NewMemStore(), reg, time.Second, log)
	srv := httptest.NewServer(httpapi.New(svc, func(context.Context) error { return nil }, log))
	t.Cleanup(srv.Close)
	return &apiClient{t: t, srv: srv, sim: sim}
}

func (c *apiClient) do(method, path, body string, header map[string]string) (int, map[string]any) {
	c.t.Helper()
	status, out, _ := c.doRaw(method, path, body, header)
	return status, out
}

// doRaw also returns the response headers.
func (c *apiClient) doRaw(method, path, body string, header map[string]string) (int, map[string]any, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	return resp.StatusCode, out, resp.Header
}

func (c *apiClient) webhook(n devsim.Notification, signature string) (int, map[string]any) {
	c.t.Helper()
	body, _ := json.Marshal(n)
	if signature == "" {
		signature = c.sim.Sign(body)
	}
	return c.do(http.MethodPost, "/api/v1/webhooks/devsim", string(body), map[string]string{devsim.SignatureHeader: signature})
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestOrderToPaidFlow(t *testing.T) {
	c := newAPI(t)

	status, order := c.do("POST", "/api/v1/orders", `{"merchant_reference":"INV-1","amount":150000,"currency":"IDR"}`, nil)
	if status != http.StatusCreated || order["status"] != "CREATED" {
		t.Fatalf("create order: %d %v", status, order)
	}
	orderID := order["id"].(string)

	idem := map[string]string{"Idempotency-Key": "checkout-INV-1-attempt-1"}
	status, pay := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"devsim","payment_method":"QRIS"}`, idem)
	if status != http.StatusCreated || pay["status"] != "PENDING" || pay["funding_source"] != "UNKNOWN" {
		t.Fatalf("create payment: %d %v", status, pay)
	}
	if _, hasAction := pay["action"]; hasAction {
		t.Fatal("simulator must not return a customer action (no fake QR codes)")
	}
	paymentID := pay["id"].(string)

	status, replay := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"devsim","payment_method":"QRIS"}`, idem)
	if status != http.StatusOK || replay["id"] != paymentID {
		t.Fatalf("idempotent replay: %d %v", status, replay)
	}

	n := devsim.Notification{
		EventID: "evt-1", ProviderReference: pay["provider_reference"].(string), MerchantReference: pay["merchant_reference"].(string),
		Status: "PAID", Amount: amount(150000), Currency: "IDR",
	}
	status, res := c.webhook(n, "")
	if status != http.StatusOK || res["status"] != "processed" || res["outcome"] != "APPLIED" {
		t.Fatalf("webhook: %d %v", status, res)
	}
	status, res = c.webhook(n, "")
	if status != http.StatusOK || res["status"] != "duplicate" {
		t.Fatalf("duplicate webhook: %d %v", status, res)
	}

	_, pay = c.do("GET", "/api/v1/payments/"+paymentID, "", nil)
	if pay["status"] != "PAID" || pay["paid_at"] == nil || pay["funding_source"] != "UNKNOWN" {
		t.Fatalf("payment after confirmation: %v", pay)
	}
	_, order = c.do("GET", "/api/v1/orders/"+orderID, "", nil)
	if order["status"] != "PAID" {
		t.Fatalf("order after confirmation: %v", order)
	}

	status, body := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"devsim","payment_method":"QRIS"}`,
		map[string]string{"Idempotency-Key": "checkout-INV-1-attempt-2"})
	if status != http.StatusConflict || errorCode(body) != "ORDER_ALREADY_PAID" {
		t.Fatalf("payment on paid order: %d %v", status, body)
	}
}

func TestWebhookRejections(t *testing.T) {
	c := newAPI(t)
	_, order := c.do("POST", "/api/v1/orders", `{"merchant_reference":"INV-2","amount":5000,"currency":"IDR"}`, nil)
	_, pay := c.do("POST", "/api/v1/orders/"+order["id"].(string)+"/payments", `{"provider":"devsim","payment_method":"EWALLET"}`,
		map[string]string{"Idempotency-Key": "checkout-INV-2"})
	n := devsim.Notification{
		EventID: "evt-1", ProviderReference: pay["provider_reference"].(string), MerchantReference: pay["merchant_reference"].(string),
		Status: "PAID", Amount: amount(5000), Currency: "IDR",
	}

	if status, body := c.webhook(n, "deadbeef"); status != http.StatusUnauthorized || errorCode(body) != "INVALID_SIGNATURE" {
		t.Fatalf("bad signature: %d %v", status, body)
	}

	tampered := n
	tampered.Amount = amount(1)
	if status, body := c.webhook(tampered, ""); status != http.StatusUnprocessableEntity || errorCode(body) != "PROVIDER_REPORT_REJECTED" {
		t.Fatalf("amount mismatch: %d %v", status, body)
	}

	status, body := c.do("POST", "/api/v1/webhooks/gopay", `{}`, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unregistered provider webhook: %d %v", status, body)
	}

	_, pay = c.do("GET", "/api/v1/payments/"+pay["id"].(string), "", nil)
	if pay["status"] != "PENDING" {
		t.Fatalf("rejected webhooks must not change payment: %v", pay)
	}
}

func TestRequestValidation(t *testing.T) {
	c := newAPI(t)
	_, order := c.do("POST", "/api/v1/orders", `{"merchant_reference":"INV-3","amount":5000,"currency":"IDR"}`, nil)
	orderPath := "/api/v1/orders/" + order["id"].(string) + "/payments"

	tests := []struct {
		name, method, path, body string
		header                   map[string]string
		status                   int
		code                     string
	}{
		{"invalid order fields", "POST", "/api/v1/orders", `{"merchant_reference":"","amount":0,"currency":"rupiah"}`, nil, 400, "VALIDATION_FAILED"},
		{"unknown json field", "POST", "/api/v1/orders", `{"merchant_reference":"X","amount":1,"currency":"IDR","status":"PAID"}`, nil, 400, "INVALID_JSON"},
		{"malformed json", "POST", "/api/v1/orders", `{"merchant_reference":`, nil, 400, "INVALID_JSON"},
		{"trailing json", "POST", "/api/v1/orders", `{"merchant_reference":"X","amount":1,"currency":"IDR"}{}`, nil, 400, "INVALID_JSON"},
		{"conflicting duplicate order", "POST", "/api/v1/orders", `{"merchant_reference":"INV-3","amount":9999,"currency":"IDR"}`, nil, 409, "DUPLICATE_ORDER"},
		{"missing idempotency key", "POST", orderPath, `{"provider":"devsim","payment_method":"QRIS"}`, nil, 400, "VALIDATION_FAILED"},
		{"unregistered provider", "POST", orderPath, `{"provider":"gopay","payment_method":"QRIS"}`, map[string]string{"Idempotency-Key": "abcdefgh"}, 422, "UNSUPPORTED_PROVIDER"},
		{"client cannot set amount", "POST", orderPath, `{"provider":"devsim","payment_method":"QRIS","amount":1}`, map[string]string{"Idempotency-Key": "abcdefgh"}, 400, "INVALID_JSON"},
		{"malformed order id", "GET", "/api/v1/orders/not-a-uuid", "", nil, 404, "NOT_FOUND"},
		{"unknown order", "GET", "/api/v1/orders/00000000-0000-4000-8000-000000000000", "", nil, 404, "NOT_FOUND"},
		{"unknown payment", "GET", "/api/v1/payments/00000000-0000-4000-8000-000000000000", "", nil, 404, "NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := c.do(tt.method, tt.path, tt.body, tt.header)
			if status != tt.status || errorCode(body) != tt.code {
				t.Fatalf("got %d %v, want %d %s", status, body, tt.status, tt.code)
			}
		})
	}
}

func TestBodyTooLarge(t *testing.T) {
	c := newAPI(t)
	big := `{"merchant_reference":"` + strings.Repeat("x", 70<<10) + `"}`
	status, body := c.do("POST", "/api/v1/orders", big, nil)
	if status != http.StatusRequestEntityTooLarge || errorCode(body) != "BODY_TOO_LARGE" {
		t.Fatalf("got %d %v", status, body)
	}
}

func amount(v int64) *int64 { return &v }

func (c *apiClient) createOrder(ref string) string {
	c.t.Helper()
	status, order := c.do("POST", "/api/v1/orders", `{"merchant_reference":"`+ref+`","amount":5000,"currency":"IDR"}`, nil)
	if status != http.StatusCreated {
		c.t.Fatalf("create order: %d %v", status, order)
	}
	return order["id"].(string)
}

func TestOrderRetryReturnsExistingOrder(t *testing.T) {
	c := newAPI(t)
	body := `{"merchant_reference":"INV-RETRY","amount":5000,"currency":"IDR","description":"d"}`
	status, first := c.do("POST", "/api/v1/orders", body, nil)
	if status != http.StatusCreated {
		t.Fatalf("first: %d %v", status, first)
	}
	status, again := c.do("POST", "/api/v1/orders", body, nil)
	if status != http.StatusOK || again["id"] != first["id"] {
		t.Fatalf("identical retry: %d %v; want 200 with id %v", status, again, first["id"])
	}
	status, conflict := c.do("POST", "/api/v1/orders", `{"merchant_reference":"INV-RETRY","amount":5001,"currency":"IDR","description":"d"}`, nil)
	if status != http.StatusConflict || errorCode(conflict) != "DUPLICATE_ORDER" {
		t.Fatalf("conflicting reuse: %d %v", status, conflict)
	}
}

func TestProviderOutcomeUnknownResponse(t *testing.T) {
	fake := providertest.New("acme", "", domain.PaymentMethodQRIS)
	fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, errors.New("upstream connection reset token=abc")
	}
	c := newAPIWith(t, io.Discard, fake)
	orderID := c.createOrder("INV-502")

	status, body := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"acme","payment_method":"QRIS"}`,
		map[string]string{"Idempotency-Key": "checkout-502"})
	e, _ := body["error"].(map[string]any)
	if status != http.StatusBadGateway || e["code"] != "PROVIDER_OUTCOME_UNKNOWN" {
		t.Fatalf("got %d %v", status, body)
	}
	paymentID, _ := e["payment_id"].(string)
	if paymentID == "" {
		t.Fatalf("payment_id missing: %v", body)
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "/api/v1/payments/{id}/inquiry") || strings.Contains(msg, "token=abc") {
		t.Fatalf("message must point at the inquiry route and not leak provider errors: %q", msg)
	}
	_, pay := c.do("GET", "/api/v1/payments/"+paymentID, "", nil)
	if pay["status"] != "CREATED" {
		t.Fatalf("unknown outcome must leave payment CREATED, got %v", pay["status"])
	}
}

func TestInquiryRoute(t *testing.T) {
	fake := providertest.New("acme", "acme-ref-1", domain.PaymentMethodQRIS)
	c := newAPIWith(t, io.Discard, fake)
	orderID := c.createOrder("INV-INQ")
	_, pay := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"acme","payment_method":"QRIS"}`,
		map[string]string{"Idempotency-Key": "checkout-inq"})
	paymentID := pay["id"].(string)

	fake.StatusFunc = func(_ context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
		return domain.StatusReport{}, errors.New("timeout")
	}
	if status, body := c.do("POST", "/api/v1/payments/"+paymentID+"/inquiry", "", nil); status != http.StatusBadGateway ||
		errorCode(body) != "PROVIDER_OUTCOME_UNKNOWN" {
		t.Fatalf("inquiry with provider failure: %d %v", status, body)
	}

	fake.StatusFunc = func(_ context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
		return domain.StatusReport{
			Provider: "acme", ProviderReference: req.ProviderReference, MerchantReference: req.MerchantReference,
			Status: domain.PaymentStatusPaid, Amount: amount(5000), Currency: "IDR",
		}, nil
	}
	status, body := c.do("POST", "/api/v1/payments/"+paymentID+"/inquiry", "", nil)
	if status != http.StatusOK || body["status"] != "PAID" || body["funding_source"] != "UNKNOWN" {
		t.Fatalf("inquiry: %d %v", status, body)
	}
	if _, order := c.do("GET", "/api/v1/orders/"+orderID, "", nil); order["status"] != "PAID" {
		t.Fatalf("order after inquiry: %v", order)
	}

	if status, body := c.do("POST", "/api/v1/payments/00000000-0000-4000-8000-000000000000/inquiry", "", nil); status != http.StatusNotFound {
		t.Fatalf("unknown payment: %d %v", status, body)
	}
}

func TestInquiryUnsupportedBySimulator(t *testing.T) {
	c := newAPI(t)
	orderID := c.createOrder("INV-SIM")
	_, pay := c.do("POST", "/api/v1/orders/"+orderID+"/payments", `{"provider":"devsim","payment_method":"QRIS"}`,
		map[string]string{"Idempotency-Key": "checkout-sim"})
	status, body := c.do("POST", "/api/v1/payments/"+pay["id"].(string)+"/inquiry", "", nil)
	if status != http.StatusNotImplemented || errorCode(body) != "INQUIRY_UNSUPPORTED" {
		t.Fatalf("got %d %v", status, body)
	}
	_, pay = c.do("GET", "/api/v1/payments/"+pay["id"].(string), "", nil)
	if pay["status"] != "PENDING" {
		t.Fatalf("payment must be unchanged: %v", pay["status"])
	}
}

func TestRequestIDInServiceLogs(t *testing.T) {
	var logs bytes.Buffer
	c := newAPIWith(t, &logs)
	n := devsim.Notification{EventID: "evt-1", ProviderReference: "x", MerchantReference: "y", Status: "PAID"}
	body, _ := json.Marshal(n)
	status, _, header := c.doRaw("POST", "/api/v1/webhooks/devsim", string(body),
		map[string]string{devsim.SignatureHeader: "deadbeefcafe"})
	if status != http.StatusUnauthorized {
		t.Fatalf("got %d", status)
	}
	reqID := header.Get("X-Request-ID")

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "webhook not accepted" {
			found = true
			if rec["level"] != "WARN" || rec["request_id"] != reqID || rec["reason"] != "invalid signature" {
				t.Fatalf("unexpected log record %v (request id %s)", rec, reqID)
			}
		}
	}
	if !found {
		t.Fatalf("no warning logged for failed signature: %s", logs.String())
	}
	if strings.Contains(logs.String(), "deadbeefcafe") || strings.Contains(logs.String(), "test-secret") {
		t.Fatal("signature or secret leaked into logs")
	}
}
