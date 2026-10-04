package ovo

// End-to-end tests: the real payment engine (payment.Service with the
// in-memory store) driving this adapter against the fake OVO server.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/payment/paymenttest"
	"merchant-payments/internal/provider"
)

type engine struct {
	t     *testing.T
	f     *fakeOVO
	store *paymenttest.MemStore
	svc   *payment.Service
	logs  *syncBuffer

	mu        sync.Mutex
	inquiries map[string]func() (int, string) // by originalPartnerReferenceNo
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newEngine(t *testing.T) *engine {
	t.Helper()
	e := &engine{t: t, f: newFakeOVO(t), store: paymenttest.NewMemStore(), logs: &syncBuffer{},
		inquiries: map[string]func() (int, string){}}
	e.f.generate = func(r recorded, _ string) (int, string) {
		var req generateRequest
		_ = json.Unmarshal(r.body, &req)
		return 200, fmt.Sprintf(`{"responseCode":"2004700","responseMessage":"Successful","referenceNo":"ref-%s",
			"partnerReferenceNo":%q,"qrContent":"00020101021226580006id.ovo%s","additionalInfo":{"expiry":75}}`,
			req.PartnerReferenceNo, req.PartnerReferenceNo, req.PartnerReferenceNo)
	}
	e.f.query = func(r recorded, _ string) (int, string) {
		var req queryRequest
		_ = json.Unmarshal(r.body, &req)
		e.mu.Lock()
		h := e.inquiries[req.OriginalPartnerReferenceNo]
		e.mu.Unlock()
		if h == nil {
			return 404, `{"responseCode":"4045101","responseMessage":"Transaction Not Found"}`
		}
		return h()
	}
	reg, err := provider.NewRegistry(newTestAdapter(t, e.f))
	if err != nil {
		t.Fatal(err)
	}
	e.svc = payment.NewService(e.store, reg, 300*time.Millisecond, slog.New(slog.NewTextHandler(e.logs, nil)))
	return e
}

// onInquiry sets OVO's answer for one payment.
func (e *engine) onInquiry(p domain.Payment, h func() (int, string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inquiries[p.MerchantReference] = h
}

// answer is an inquiry response for p with the given fields.
func answer(p domain.Payment, status, value, currency, summary string) func() (int, string) {
	return func() (int, string) {
		amt := `{"value":"` + value + `"`
		if currency != "" {
			amt += `,"currency":"` + currency + `"`
		}
		amt += `}`
		return 200, `{"responseCode":"2005100","responseMessage":"Successful","originalReferenceNo":"` + p.ProviderReference +
			`","originalPartnerReferenceNo":"` + p.MerchantReference + `","serviceCode":"47","latestTransactionStatus":"` + status +
			`","paidTime":"2026-10-04T05:06:07.000+0000","amount":` + amt + `,"additionalInfo":{"transactionId":"7178",` +
			`"userName":"H**** L*******","phoneNumber":"********2580","transactionSummary":` + summary + `}}`
	}
}

const cashSummary = `{"points":"0","cash":"150000","payLater":"0"}`

func (e *engine) pendingPayment(ref string) domain.Payment {
	e.t.Helper()
	ctx := context.Background()
	o, _, err := e.svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: ref, Amount: 150000, Currency: "IDR"})
	if err != nil {
		e.t.Fatal(err)
	}
	p, _, err := e.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: Name, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-" + ref,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *engine) state(p domain.Payment) (domain.Payment, domain.Order) {
	e.t.Helper()
	got, err := e.svc.GetPayment(context.Background(), p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	o, err := e.svc.GetOrder(context.Background(), got.OrderID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got, o
}

func TestCreatePaymentReturnsQRAndStaysPending(t *testing.T) {
	e := newEngine(t)
	p := e.pendingPayment("INV-1")
	if p.Status != domain.PaymentStatusPending || p.PaidAt != nil || p.FundingSource != domain.FundingSourceUnknown {
		t.Fatalf("creating a QR must not confirm anything: %+v", p)
	}
	if p.ProviderReference != "ref-"+p.MerchantReference ||
		p.Action != (domain.CustomerAction{Type: domain.ActionQRString, Value: "00020101021226580006id.ovo" + p.MerchantReference}) {
		t.Fatalf("unexpected provider data %+v", p)
	}
	if p.ExpiresAt == nil || time.Until(*p.ExpiresAt) > 75*time.Second || time.Until(*p.ExpiresAt) < 70*time.Second {
		t.Fatalf("expires at = %v", p.ExpiresAt)
	}
}

func TestInquiryConfirmsPaymentAndOrder(t *testing.T) {
	tests := []struct {
		summary string
		want    domain.FundingSource
	}{
		{cashSummary, domain.FundingSourceOVOCash},
		{`{"points":"0","cash":"0","payLater":"150000"}`, domain.FundingSourceOVOPayLater},
		{`{"points":"150000","cash":"0","payLater":"0"}`, domain.FundingSourceUnknown},
	}
	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			e := newEngine(t)
			p := e.pendingPayment("INV-1")
			e.onInquiry(p, answer(p, "00", "150000.00", "IDR", tt.summary))

			res, err := e.svc.InquirePayment(context.Background(), p.ID)
			if err != nil || res.Outcome != domain.OutcomeApplied {
				t.Fatalf("inquiry = %+v, %v", res, err)
			}
			got, order := e.state(p)
			if got.Status != domain.PaymentStatusPaid || order.Status != domain.OrderStatusPaid || got.FundingSource != tt.want {
				t.Fatalf("payment=%s order=%s funding=%s", got.Status, order.Status, got.FundingSource)
			}
			if got.PaidAt == nil || order.PaidAt == nil || !got.PaidAt.Equal(*order.PaidAt) {
				t.Fatal("payment and order must be paid in the same transaction")
			}
			events := e.store.Events()
			ev := events[len(events)-1]
			if ev.Source != domain.SourceInquiry || ev.ProviderOccurredAt == nil ||
				!ev.ProviderOccurredAt.Equal(time.Date(2026, 10, 4, 5, 6, 7, 0, time.UTC)) {
				t.Fatalf("audit event %+v", ev)
			}
			if strings.Contains(string(ev.Payload), "userName") || strings.Contains(string(ev.Payload), "2580") {
				t.Fatalf("customer data in audit payload: %s", ev.Payload)
			}
		})
	}
}

func TestPaymentMethodNeverSetsFundingSource(t *testing.T) {
	e := newEngine(t)
	p := e.pendingPayment("INV-1")
	e.onInquiry(p, answer(p, "00", "150000.00", "IDR", `null`)) // no transactionSummary
	if _, err := e.svc.InquirePayment(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.state(p); got.Status != domain.PaymentStatusPaid || got.FundingSource != domain.FundingSourceUnknown {
		t.Fatalf("got %s / %s", got.Status, got.FundingSource)
	}
}

func TestInquiryFailureKeepsPaymentUnchanged(t *testing.T) {
	for name, h := range map[string]func() (int, string){
		"provider timeout": func() (int, string) { return 500, `{"responseCode":"5005104","responseMessage":"Timeout"}` },
		"not found": func() (int, string) {
			return 404, `{"responseCode":"4045101","responseMessage":"Transaction Not Found"}`
		},
		"refund status": nil, // set below
	} {
		t.Run(name, func(t *testing.T) {
			e := newEngine(t)
			p := e.pendingPayment("INV-1")
			if h == nil {
				h = answer(p, "04", "150000.00", "IDR", cashSummary)
			}
			e.onInquiry(p, h)
			before := len(e.store.Events())
			if _, err := e.svc.InquirePayment(context.Background(), p.ID); !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
				t.Fatalf("want ErrProviderOutcomeUnknown, got %v", err)
			}
			got, order := e.state(p)
			if got.Status != domain.PaymentStatusPending || order.Status != domain.OrderStatusCreated || len(e.store.Events()) != before {
				t.Fatalf("payment=%s order=%s events +%d", got.Status, order.Status, len(e.store.Events())-before)
			}
		})
	}
}

func TestInquiryEvidenceMustMatch(t *testing.T) {
	tests := []struct {
		name string
		mod  func(p domain.Payment) func() (int, string)
	}{
		{"amount mismatch", func(p domain.Payment) func() (int, string) {
			return answer(p, "00", "149999.00", "IDR", `{"points":"0","cash":"149999","payLater":"0"}`)
		}},
		{"currency missing", func(p domain.Payment) func() (int, string) {
			return answer(p, "00", "150000.00", "", cashSummary) // as in OVO's Postman example
		}},
		{"provider reference mismatch", func(p domain.Payment) func() (int, string) {
			q := p
			q.ProviderReference = "ref-somebody-else"
			return answer(q, "00", "150000.00", "IDR", cashSummary)
		}},
		{"merchant reference mismatch", func(p domain.Payment) func() (int, string) {
			q := p
			q.MerchantReference = "another-payment"
			return answer(q, "00", "150000.00", "IDR", cashSummary)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine(t)
			p := e.pendingPayment("INV-1")
			e.onInquiry(p, tt.mod(p))
			_, err := e.svc.InquirePayment(context.Background(), p.ID)
			if !errors.Is(err, domain.ErrConfirmationMismatch) {
				t.Fatalf("want ErrConfirmationMismatch, got %v", err)
			}
			got, order := e.state(p)
			if got.Status != domain.PaymentStatusPending || order.Status != domain.OrderStatusCreated {
				t.Fatalf("payment=%s order=%s; mismatched evidence must not confirm", got.Status, order.Status)
			}
			events := e.store.Events()
			if ev := events[len(events)-1]; ev.Outcome != domain.OutcomeRejected || ev.ReportedStatus != domain.PaymentStatusPaid {
				t.Fatalf("rejection not audited: %+v", ev)
			}
		})
	}
}

func TestUnknownCreateOutcomeNeverBecomesPaidOrExpired(t *testing.T) {
	e := newEngine(t)
	e.f.generate = func(recorded, string) (int, string) {
		time.Sleep(500 * time.Millisecond) // longer than the engine's provider timeout
		return 200, `{}`
	}
	ctx := context.Background()
	o, _, _ := e.svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-1", Amount: 150000, Currency: "IDR"})
	p, _, err := e.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{Provider: Name, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-INV-1"})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.svc.ExpireOverduePayments(ctx, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.InquirePayment(ctx, p.ID); !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("inquiry without OVO referenceNo: got %v", err)
	}
	if got, order := e.state(p); got.Status != domain.PaymentStatusCreated || order.Status != domain.OrderStatusCreated {
		t.Fatalf("payment=%s order=%s; an unknown outcome must stay CREATED", got.Status, order.Status)
	}
}

func TestExpirySweepInquiresBeforeExpiring(t *testing.T) {
	e := newEngine(t)
	paid, pending, failed, broken := e.pendingPayment("INV-PAID"), e.pendingPayment("INV-PENDING"),
		e.pendingPayment("INV-FAILED"), e.pendingPayment("INV-BROKEN")
	e.onInquiry(paid, answer(paid, "00", "150000.00", "IDR", cashSummary))
	e.onInquiry(pending, answer(pending, "03", "150000.00", "IDR", `null`))
	e.onInquiry(failed, answer(failed, "06", "150000.00", "IDR", `null`))
	e.onInquiry(broken, func() (int, string) {
		e.store.Events() // takes the store lock: deadlocks if the sweep held a transaction during the HTTP call
		return 500, `{"responseCode":"5005101","responseMessage":"Internal Server Error"}`
	})

	// A negative grace makes every QR (75s expiry) overdue now.
	expired, err := e.svc.ExpireOverduePayments(context.Background(), -time.Hour)
	if err != nil || expired != 0 {
		t.Fatalf("expired %d, %v; an inquiry-capable provider must never be expired locally", expired, err)
	}
	for p, want := range map[*domain.Payment][2]string{
		&paid:    {"PAID", "PAID"},
		&pending: {"PENDING", "CREATED"},
		&failed:  {"FAILED", "CREATED"},
		&broken:  {"PENDING", "CREATED"},
	} {
		got, order := e.state(*p)
		if string(got.Status) != want[0] || string(order.Status) != want[1] {
			t.Errorf("%s: payment=%s order=%s, want %v", p.MerchantReference, got.Status, order.Status, want)
		}
	}
}

func TestSecretsNeverInLogsOrErrors(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	var errs []error

	p := e.pendingPayment("INV-1")
	e.onInquiry(p, func() (int, string) { return 500, `{"responseCode":"5005100","responseMessage":"General Error"}` })
	_, err := e.svc.InquirePayment(ctx, p.ID)
	errs = append(errs, err)
	e.onInquiry(p, answer(p, "00", "1.00", "IDR", cashSummary)) // rejected report
	_, err = e.svc.InquirePayment(ctx, p.ID)
	errs = append(errs, err)

	e.f.generate = func(recorded, string) (int, string) { return 401, `{"responseCode":"4014701"}` }
	o, _, _ := e.svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-2", Amount: 1000, Currency: "IDR"})
	rejected, _, err := e.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{Provider: Name, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-INV-2"})
	if err != nil || rejected.Status != domain.PaymentStatusFailed {
		t.Fatalf("a definitive OVO rejection must record FAILED: %s, %v", rejected.Status, err)
	}

	e.f.mu.Lock()
	e.f.tokenFailure = true
	e.f.mu.Unlock()
	a := newTestAdapter(t, e.f)
	_, err = a.CreatePayment(ctx, createReq())
	errs = append(errs, err)

	var secrets []string
	secrets = append(secrets, testSecret, "BEGIN PRIVATE KEY", "MIIE")
	for i := 1; i <= e.f.tokens(); i++ {
		secrets = append(secrets, fmt.Sprintf("tok-%d", i))
	}
	for _, r := range e.f.received() {
		secrets = append(secrets, r.header.Get("X-SIGNATURE"))
	}
	all := e.logs.String()
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected every scenario to fail")
		}
		all += "\n" + err.Error()
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			t.Errorf("secret %q leaked into logs or errors", s[:min(len(s), 12)])
		}
	}
}
