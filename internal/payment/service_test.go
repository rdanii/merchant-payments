package payment_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/payment/paymenttest"
	"merchant-payments/internal/provider"
	"merchant-payments/internal/provider/providertest"
)

const acme domain.Provider = "acme"

type fixture struct {
	t     *testing.T
	store *paymenttest.MemStore
	fake  *providertest.Fake
	svc   *payment.Service
	logs  *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fake := providertest.New(acme, "acme-ref-1", domain.PaymentMethodQRIS, domain.PaymentMethodEWallet)
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	store := paymenttest.NewMemStore()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	return &fixture{t: t, store: store, fake: fake, svc: payment.NewService(store, reg, 200*time.Millisecond, log), logs: logs}
}

func (f *fixture) order() domain.Order {
	f.t.Helper()
	o, _, err := f.svc.CreateOrder(context.Background(), domain.NewOrderInput{
		MerchantReference: "INV-" + time.Now().Format("150405.000000000"), Amount: 150000, Currency: "IDR",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

func (f *fixture) pendingPayment(o domain.Order) domain.Payment {
	f.t.Helper()
	p, created, err := f.svc.CreatePayment(context.Background(), o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if err != nil || !created {
		f.t.Fatalf("CreatePayment = %v, %v", created, err)
	}
	return p
}

// deliver simulates a verified webhook carrying report.
func (f *fixture) deliver(report domain.StatusReport) (payment.ReportResult, error) {
	f.fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) { return report, nil }
	return f.svc.HandleWebhook(context.Background(), acme, provider.Webhook{})
}

func paidReport(p domain.Payment, eventID string) domain.StatusReport {
	return domain.StatusReport{
		Provider: acme, ProviderEventID: eventID, ProviderReference: p.ProviderReference,
		MerchantReference: p.MerchantReference, Status: domain.PaymentStatusPaid,
		Amount: amount(p.Amount), Currency: p.Currency,
	}
}

func amount(v int64) *int64 { return &v }

func (f *fixture) mustState(paymentID string, wantPayment domain.PaymentStatus, wantOrder domain.OrderStatus) domain.Payment {
	f.t.Helper()
	ctx := context.Background()
	p, err := f.svc.GetPayment(ctx, paymentID)
	if err != nil {
		f.t.Fatal(err)
	}
	o, err := f.svc.GetOrder(ctx, p.OrderID)
	if err != nil {
		f.t.Fatal(err)
	}
	if p.Status != wantPayment || o.Status != wantOrder {
		f.t.Fatalf("payment=%s order=%s, want payment=%s order=%s", p.Status, o.Status, wantPayment, wantOrder)
	}
	return p
}

func TestCreateOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := domain.NewOrderInput{MerchantReference: "INV-1", Amount: 10000, Currency: "IDR"}

	o, created, err := f.svc.CreateOrder(ctx, in)
	if err != nil || !created {
		t.Fatalf("CreateOrder = %v, %v", created, err)
	}
	if o.ID == "" || o.Status != domain.OrderStatusCreated {
		t.Fatalf("unexpected order %+v", o)
	}
	var ve *domain.ValidationError
	if _, _, err := f.svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-2", Amount: -5, Currency: "IDR"}); !errors.As(err, &ve) {
		t.Fatalf("invalid order: want ValidationError, got %v", err)
	}
}

func TestCreatePaymentIsNeverPaid(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	p := f.pendingPayment(o)

	if p.Status != domain.PaymentStatusPending || p.PaidAt != nil {
		t.Fatalf("payment creation must not mark PAID; got %s", p.Status)
	}
	if p.FundingSource != domain.FundingSourceUnknown {
		t.Fatalf("funding source must be UNKNOWN before confirmation, got %s", p.FundingSource)
	}
	if p.ProviderReference != "acme-ref-1" || p.Amount != o.Amount || p.Currency != o.Currency {
		t.Fatalf("unexpected payment %+v", p)
	}
	calls := f.fake.CreateCalls()
	if len(calls) != 1 || calls[0].MerchantReference != p.MerchantReference || calls[0].Amount != o.Amount {
		t.Fatalf("provider create calls = %+v", calls)
	}
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
}

func TestCreatePaymentValidation(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	ctx := context.Background()

	tests := []struct {
		name    string
		orderID string
		in      domain.NewPaymentInput
		want    error
	}{
		{"missing idempotency key", o.ID, domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS}, nil},
		{"invalid method", o.ID, domain.NewPaymentInput{Provider: acme, Method: "CASH", IdempotencyKey: "idem-key-1"}, nil},
		{"unregistered provider", o.ID, domain.NewPaymentInput{Provider: "gopay", Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1"}, domain.ErrUnsupportedProvider},
		{"method not supported by provider", o.ID, domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodPayLater, IdempotencyKey: "idem-key-1"}, domain.ErrUnsupportedMethod},
		{"unknown order", "00000000-0000-4000-8000-000000000000", domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1"}, domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := f.svc.CreatePayment(ctx, tt.orderID, tt.in)
			if tt.want == nil {
				var ve *domain.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("want ValidationError, got %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}
	if n := len(f.fake.CreateCalls()); n != 0 {
		t.Fatalf("provider must not be called for invalid requests; got %d calls", n)
	}
}

func TestCreatePaymentIdempotency(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	ctx := context.Background()
	p := f.pendingPayment(o)

	again, created, err := f.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if err != nil || created || again.ID != p.ID {
		t.Fatalf("replay = %s, created=%v, err=%v; want same payment", again.ID, created, err)
	}
	if n := len(f.fake.CreateCalls()); n != 1 {
		t.Fatalf("replay must not call provider again; calls=%d", n)
	}

	_, _, err = f.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodEWallet, IdempotencyKey: "idem-key-1",
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("same key, different params: want ErrIdempotencyConflict, got %v", err)
	}

	_, _, err = f.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-2",
	})
	if !errors.Is(err, domain.ErrActivePaymentExists) {
		t.Fatalf("second active payment: want ErrActivePaymentExists, got %v", err)
	}
}

func TestCreatePaymentRejectedWhenOrderPaid(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	p := f.pendingPayment(o)
	if _, err := f.deliver(paidReport(p, "evt-1")); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.svc.CreatePayment(context.Background(), o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-2",
	})
	if !errors.Is(err, domain.ErrOrderAlreadyPaid) {
		t.Fatalf("want ErrOrderAlreadyPaid, got %v", err)
	}
}

func TestCreatePaymentProviderRejected(t *testing.T) {
	f := newFixture(t)
	f.fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, provider.ErrRejected
	}
	o := f.order()
	p, created, err := f.svc.CreatePayment(context.Background(), o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if err != nil || !created || p.Status != domain.PaymentStatusFailed {
		t.Fatalf("got status=%s created=%v err=%v; want FAILED", p.Status, created, err)
	}
	f.mustState(p.ID, domain.PaymentStatusFailed, domain.OrderStatusCreated)
}

func TestCreatePaymentProviderTimeout(t *testing.T) {
	f := newFixture(t)
	f.fake.CreateFunc = func(ctx context.Context, _ provider.CreateRequest) (provider.CreateResult, error) {
		<-ctx.Done() // provider hangs until the configured timeout fires
		return provider.CreateResult{}, ctx.Err()
	}
	o := f.order()
	start := time.Now()
	p, _, err := f.svc.CreatePayment(context.Background(), o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("want ErrProviderOutcomeUnknown, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("provider timeout not enforced: %s", elapsed)
	}
	// Outcome unknown: neither failed nor paid; it stays CREATED for inquiry.
	f.mustState(p.ID, domain.PaymentStatusCreated, domain.OrderStatusCreated)
}

func TestCreatePaymentSurvivesClientCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.fake.CreateFunc = func(pctx context.Context, _ provider.CreateRequest) (provider.CreateResult, error) {
		cancel() // client disconnects while the provider call is in flight
		if err := pctx.Err(); err != nil {
			return provider.CreateResult{}, err
		}
		return provider.CreateResult{ProviderReference: "acme-ref-1"}, nil
	}
	o := f.order()
	p, _, err := f.svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if err != nil || p.Status != domain.PaymentStatusPending {
		t.Fatalf("got %s, %v; provider result must be recorded despite client cancel", p.Status, err)
	}
}

func TestConfirmationMarksPaymentAndOrderPaid(t *testing.T) {
	f := newFixture(t)
	o := f.order()
	p := f.pendingPayment(o)

	report := paidReport(p, "evt-1")
	report.FundingSource = domain.FundingSourceEWalletBalance
	res, err := f.deliver(report)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.Outcome != domain.OutcomeApplied {
		t.Fatalf("unexpected result %+v", res)
	}

	paid := f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	if paid.FundingSource != domain.FundingSourceEWalletBalance || paid.PaidAt == nil {
		t.Fatalf("unexpected paid payment %+v", paid)
	}
	order, _ := f.svc.GetOrder(context.Background(), o.ID)
	if order.PaidAt == nil || !order.PaidAt.Equal(*paid.PaidAt) {
		t.Fatal("order paid_at must match payment paid_at")
	}
}

func TestConfirmationWithoutFundingSourceIsUnknown(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	if _, err := f.deliver(paidReport(p, "evt-1")); err != nil {
		t.Fatal(err)
	}
	paid := f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	if paid.FundingSource != domain.FundingSourceUnknown {
		t.Fatalf("want UNKNOWN, got %s", paid.FundingSource)
	}
}

func TestDuplicateWebhookIsIdempotent(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	report := paidReport(p, "evt-1")

	if _, err := f.deliver(report); err != nil {
		t.Fatal(err)
	}
	first := f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	eventsBefore := len(f.store.Events())

	for range 3 {
		res, err := f.deliver(report)
		if err != nil || !res.Duplicate {
			t.Fatalf("redelivery = %+v, %v; want duplicate", res, err)
		}
	}
	again := f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	if !again.PaidAt.Equal(*first.PaidAt) {
		t.Fatal("duplicate delivery changed paid_at")
	}
	if n := len(f.store.Events()); n != eventsBefore {
		t.Fatalf("duplicate deliveries recorded %d extra events", n-eventsBefore)
	}

	// A distinct event repeating the same status is recorded but changes nothing.
	res, err := f.deliver(paidReport(p, "evt-2"))
	if err != nil || res.Outcome != domain.OutcomeNoChange {
		t.Fatalf("repeat status = %+v, %v; want NO_CHANGE", res, err)
	}
}

func TestInvalidConfirmationsAreRejectedAndAudited(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*domain.StatusReport)
		want error
	}{
		{"amount mismatch", func(r *domain.StatusReport) { r.Amount = amount(*r.Amount + 1) }, domain.ErrConfirmationMismatch},
		{"currency mismatch", func(r *domain.StatusReport) { r.Currency = "USD" }, domain.ErrConfirmationMismatch},
		{"provider reference mismatch", func(r *domain.StatusReport) { r.ProviderReference = "forged" }, domain.ErrConfirmationMismatch},
		{"unknown merchant reference", func(r *domain.StatusReport) { r.MerchantReference = "nope" }, domain.ErrConfirmationMismatch},
		{"unrecognized status", func(r *domain.StatusReport) { r.Status = "SETTLED" }, domain.ErrInvalidProviderReport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.pendingPayment(f.order())
			r := paidReport(p, "evt-bad")
			tt.mod(&r)

			_, err := f.deliver(r)
			if !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)

			events := f.store.Events()
			last := events[len(events)-1]
			if last.Outcome != domain.OutcomeRejected || last.ProviderEventID != "evt-bad" {
				t.Fatalf("rejected report must be audited; last event %+v", last)
			}
		})
	}
}

func TestPaidCannotBeReversedByProviderReport(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	if _, err := f.deliver(paidReport(p, "evt-1")); err != nil {
		t.Fatal(err)
	}
	failed := paidReport(p, "evt-2")
	failed.Status = domain.PaymentStatusFailed
	if _, err := f.deliver(failed); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("PAID -> FAILED: want ErrInvalidTransition, got %v", err)
	}
	f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
}

func TestConfirmationRollsBackOnOrderUpdateFailure(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	eventsBefore := len(f.store.Events())

	f.store.FailUpdateOrder = errors.New("disk on fire")
	if _, err := f.deliver(paidReport(p, "evt-1")); err == nil {
		t.Fatal("want error")
	}
	// Payment must not be PAID while its order is not.
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	if n := len(f.store.Events()); n != eventsBefore {
		t.Fatal("failed transaction must not leave an event behind")
	}

	// The provider retries the same event after recovery and it is applied.
	f.store.FailUpdateOrder = nil
	if res, err := f.deliver(paidReport(p, "evt-1")); err != nil || res.Duplicate {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
}

func TestWebhookErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.HandleWebhook(ctx, "gopay", provider.Webhook{}); !errors.Is(err, domain.ErrUnsupportedProvider) {
		t.Fatalf("unregistered provider: got %v", err)
	}
	f.fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) {
		return domain.StatusReport{}, provider.ErrInvalidSignature
	}
	if _, err := f.svc.HandleWebhook(ctx, acme, provider.Webhook{}); !errors.Is(err, provider.ErrInvalidSignature) {
		t.Fatalf("bad signature: got %v", err)
	}
	p := f.pendingPayment(f.order())
	r := paidReport(p, "")
	if _, err := f.deliver(r); !errors.Is(err, domain.ErrInvalidProviderReport) {
		t.Fatalf("missing event id: got %v", err)
	}
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
}

func TestInquiryResolvesUnknownOutcome(t *testing.T) {
	f := newFixture(t)
	f.fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, errors.New("connection reset")
	}
	o := f.order()
	p, _, err := f.svc.CreatePayment(context.Background(), o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("got %v", err)
	}

	f.fake.StatusFunc = func(_ context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
		return domain.StatusReport{
			Provider: acme, ProviderReference: "acme-ref-9", MerchantReference: req.MerchantReference,
			Status: domain.PaymentStatusPaid, Amount: amount(o.Amount), Currency: o.Currency,
		}, nil
	}
	res, err := f.svc.InquirePayment(context.Background(), p.ID)
	if err != nil || res.Outcome != domain.OutcomeApplied {
		t.Fatalf("inquiry = %+v, %v", res, err)
	}
	paid := f.mustState(p.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	if paid.ProviderReference != "acme-ref-9" {
		t.Fatalf("provider reference not recorded: %q", paid.ProviderReference)
	}
}

func (f *fixture) lastEvent() domain.PaymentEvent {
	f.t.Helper()
	events := f.store.Events()
	if len(events) == 0 {
		f.t.Fatal("no events recorded")
	}
	return events[len(events)-1]
}

func TestCreateOrderRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := domain.NewOrderInput{MerchantReference: "INV-R", Amount: 10000, Currency: "IDR", Description: "x"}
	first, _, err := f.svc.CreateOrder(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	again, created, err := f.svc.CreateOrder(ctx, in)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("identical retry = %s, created=%v, err=%v; want existing order %s", again.ID, created, err, first.ID)
	}

	for name, mod := range map[string]func(*domain.NewOrderInput){
		"amount":      func(in *domain.NewOrderInput) { in.Amount++ },
		"currency":    func(in *domain.NewOrderInput) { in.Currency = "USD" },
		"description": func(in *domain.NewOrderInput) { in.Description = "y" },
	} {
		conflicting := in
		mod(&conflicting)
		if _, _, err := f.svc.CreateOrder(ctx, conflicting); !errors.Is(err, domain.ErrDuplicateOrder) {
			t.Errorf("different %s: want ErrDuplicateOrder, got %v", name, err)
		}
	}
	got, _ := f.store.GetOrderByMerchantReference(ctx, "INV-R")
	if got.ID != first.ID || got.Amount != 10000 {
		t.Fatalf("original order changed: %+v", got)
	}
}

// pendingWithExpiry creates a PENDING payment whose provider expiry is at.
func (f *fixture) pendingWithExpiry(at time.Time) domain.Payment {
	f.t.Helper()
	f.fake.CreateFunc = func(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "ref-" + req.MerchantReference, ExpiresAt: &at}, nil
	}
	return f.pendingPayment(f.order())
}

func TestExpireOverduePayments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	grace := 5 * time.Minute

	overdue := f.pendingWithExpiry(now.Add(-10 * time.Minute))
	inGrace := f.pendingWithExpiry(now.Add(-time.Minute))
	future := f.pendingWithExpiry(now.Add(time.Hour))
	paid := f.pendingWithExpiry(now.Add(-time.Hour))
	if _, err := f.deliver(paidReport(paid, "evt-paid")); err != nil {
		t.Fatal(err)
	}
	f.fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, errors.New("timeout") // outcome unknown: CREATED, no expiry
	}
	unknown, _, _ := f.svc.CreatePayment(ctx, f.order().ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-1",
	})

	n, err := f.svc.ExpireOverduePayments(ctx, grace)
	if err != nil || n != 1 {
		t.Fatalf("ExpireOverduePayments = %d, %v; want 1", n, err)
	}
	f.mustState(overdue.ID, domain.PaymentStatusExpired, domain.OrderStatusCreated)
	f.mustState(inGrace.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	f.mustState(future.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	f.mustState(paid.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	f.mustState(unknown.ID, domain.PaymentStatusCreated, domain.OrderStatusCreated)

	ev := f.lastEvent()
	if ev.Type != domain.EventPaymentExpired || ev.Source != domain.SourceSystem || ev.PaymentID != overdue.ID ||
		ev.FromStatus != domain.PaymentStatusPending || ev.ToStatus != domain.PaymentStatusExpired {
		t.Fatalf("unexpected expiry event %+v", ev)
	}

	// The order is no longer blocked by the expired attempt.
	f.fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "ref-retry"}, nil
	}
	if _, created, err := f.svc.CreatePayment(ctx, overdue.OrderID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-retry",
	}); err != nil || !created {
		t.Fatalf("new attempt after expiry: %v, %v", created, err)
	}

	// Running again is a no-op.
	if n, err := f.svc.ExpireOverduePayments(ctx, grace); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
}

func TestLatePaidAfterLocalExpiryIsRejectedAndAudited(t *testing.T) {
	f := newFixture(t)
	p := f.pendingWithExpiry(time.Now().Add(-time.Hour))
	if _, err := f.svc.ExpireOverduePayments(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deliver(paidReport(p, "evt-late")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("late PAID: want ErrInvalidTransition, got %v", err)
	}
	f.mustState(p.ID, domain.PaymentStatusExpired, domain.OrderStatusCreated)
	if ev := f.lastEvent(); ev.Outcome != domain.OutcomeRejected || ev.ReportedStatus != domain.PaymentStatusPaid {
		t.Fatalf("late PAID must be audited for reconciliation: %+v", ev)
	}
}

func TestInquiryUnsupportedLeavesPaymentUnchanged(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	f.fake.StatusFunc = func(context.Context, provider.StatusRequest) (domain.StatusReport, error) {
		return domain.StatusReport{}, provider.ErrInquiryUnsupported
	}
	_, err := f.svc.InquirePayment(context.Background(), p.ID)
	if !errors.Is(err, provider.ErrInquiryUnsupported) || errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("want ErrInquiryUnsupported only, got %v", err)
	}
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
}

func TestWebhookPrecheckRejectionsAreAudited(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())

	missingID := paidReport(p, "")
	if _, err := f.deliver(missingID); !errors.Is(err, domain.ErrInvalidProviderReport) {
		t.Fatalf("missing event id: got %v", err)
	}
	ev := f.lastEvent()
	if ev.Outcome != domain.OutcomeRejected || ev.PaymentID != p.ID || ev.Source != domain.SourceWebhook ||
		!strings.Contains(ev.Reason, "missing provider event id") {
		t.Fatalf("missing event id not audited: %+v", ev)
	}

	wrongProvider := paidReport(p, "evt-1")
	wrongProvider.Provider = "other"
	if _, err := f.deliver(wrongProvider); !errors.Is(err, domain.ErrInvalidProviderReport) {
		t.Fatalf("provider mismatch: got %v", err)
	}
	ev = f.lastEvent()
	if ev.Outcome != domain.OutcomeRejected || ev.Provider != acme || ev.ProviderEventID != "" {
		t.Fatalf("provider mismatch not audited against the route provider: %+v", ev)
	}
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)

	// The event id was not consumed, so a correct delivery still applies.
	if res, err := f.deliver(paidReport(p, "evt-1")); err != nil || res.Outcome != domain.OutcomeApplied {
		t.Fatalf("valid delivery after rejection = %+v, %v", res, err)
	}
}

func TestStorageRejectionInsideTransactionIsAudited(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	f.store.FailUpdatePayment = fmt.Errorf("%w: provider reference already used by another payment", domain.ErrConfirmationMismatch)

	if _, err := f.deliver(paidReport(p, "evt-1")); !errors.Is(err, domain.ErrConfirmationMismatch) {
		t.Fatalf("want ErrConfirmationMismatch, got %v", err)
	}
	f.mustState(p.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	ev := f.lastEvent()
	if ev.Outcome != domain.OutcomeRejected || ev.ProviderEventID != "evt-1" || ev.PaymentID != p.ID {
		t.Fatalf("rolled-back rejection not audited: %+v", ev)
	}

	f.store.FailUpdatePayment = nil
	if res, err := f.deliver(paidReport(p, "evt-1")); err != nil || !res.Duplicate {
		t.Fatalf("redelivery of a rejected event must stay idempotent: %+v, %v", res, err)
	}
}

func TestStorageInfrastructureErrorIsNotConsumed(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())
	eventsBefore := len(f.store.Events())
	f.store.FailUpdatePayment = errors.New("connection reset")

	if _, err := f.deliver(paidReport(p, "evt-1")); err == nil || errors.Is(err, domain.ErrConfirmationMismatch) {
		t.Fatalf("want infrastructure error, got %v", err)
	}
	if len(f.store.Events()) != eventsBefore {
		t.Fatal("an infrastructure failure must not record the event, so the provider retry is processed")
	}
}

func TestWebhookAuthFailureIsLoggedSafely(t *testing.T) {
	f := newFixture(t)
	f.fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) {
		return domain.StatusReport{}, provider.ErrInvalidSignature
	}
	w := provider.Webhook{
		Headers: map[string][]string{"X-Signature": {"sig-value-123"}},
		Body:    []byte(`{"secret_token":"tok-456"}`),
	}
	if _, err := f.svc.HandleWebhook(context.Background(), acme, w); !errors.Is(err, provider.ErrInvalidSignature) {
		t.Fatalf("got %v", err)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "invalid signature") {
		t.Fatalf("want a warning for the failed signature, got %q", logs)
	}
	if strings.Contains(logs, "sig-value-123") || strings.Contains(logs, "tok-456") {
		t.Fatalf("headers or body leaked into logs: %q", logs)
	}
	if len(f.store.Events()) != 0 {
		t.Fatal("unauthenticated webhooks must not be written to the audit trail")
	}
}

func TestReportAuditFields(t *testing.T) {
	f := newFixture(t)
	p := f.pendingPayment(f.order())

	providerTime := time.Date(2026, 10, 1, 8, 30, 0, 0, time.FixedZone("WIB", 7*3600))
	failed := paidReport(p, "evt-1")
	failed.Status = domain.PaymentStatusFailed
	failed.Amount = nil // provider did not report an amount
	failed.OccurredAt = providerTime
	if _, err := f.deliver(failed); err != nil {
		t.Fatal(err)
	}
	ev := f.lastEvent()
	if ev.ReportedAmount != nil {
		t.Fatalf("missing amount must be stored as nil, got %d", *ev.ReportedAmount)
	}
	if ev.ProviderOccurredAt == nil || !ev.ProviderOccurredAt.Equal(providerTime) {
		t.Fatalf("provider event time not preserved: %v", ev.ProviderOccurredAt)
	}
	if ev.CreatedAt.Equal(providerTime) {
		t.Fatal("local processing time must stay distinct from provider event time")
	}

	f2 := newFixture(t)
	p2 := f2.pendingPayment(f2.order())
	if _, err := f2.deliver(paidReport(p2, "evt-2")); err != nil {
		t.Fatal(err)
	}
	ev = f2.lastEvent()
	if ev.ReportedAmount == nil || *ev.ReportedAmount != p2.Amount || ev.ProviderOccurredAt != nil {
		t.Fatalf("reported amount / absent event time: %+v", ev)
	}
	paid, _ := f2.svc.GetPayment(context.Background(), p2.ID)
	if paid.PaidAt == nil {
		t.Fatal("paid_at must be set from local processing time")
	}
}

// statusReport answers an inquiry for p with status.
func statusReport(p domain.Payment, status domain.PaymentStatus) domain.StatusReport {
	r := paidReport(p, "")
	r.Status = status
	return r
}

func TestExpirySweepUsesInquiryResult(t *testing.T) {
	f := newFixture(t)
	past := time.Now().Add(-time.Hour)
	paid, pending, failed, broken := f.pendingWithExpiry(past), f.pendingWithExpiry(past), f.pendingWithExpiry(past), f.pendingWithExpiry(past)
	answers := map[string]func() (domain.StatusReport, error){
		paid.MerchantReference:    func() (domain.StatusReport, error) { return statusReport(paid, domain.PaymentStatusPaid), nil },
		pending.MerchantReference: func() (domain.StatusReport, error) { return statusReport(pending, domain.PaymentStatusPending), nil },
		failed.MerchantReference:  func() (domain.StatusReport, error) { return statusReport(failed, domain.PaymentStatusFailed), nil },
		broken.MerchantReference:  func() (domain.StatusReport, error) { return domain.StatusReport{}, errors.New("connection reset") },
	}
	f.fake.StatusFunc = func(_ context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
		return answers[req.MerchantReference]()
	}

	n, err := f.svc.ExpireOverduePayments(context.Background(), time.Minute)
	if err != nil || n != 0 {
		t.Fatalf("expired %d, %v; a provider that answers inquiries is never expired locally", n, err)
	}
	f.mustState(paid.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
	f.mustState(pending.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	f.mustState(failed.ID, domain.PaymentStatusFailed, domain.OrderStatusCreated)
	f.mustState(broken.ID, domain.PaymentStatusPending, domain.OrderStatusCreated)
	if !strings.Contains(f.logs.String(), "overdue payment not resolved") {
		t.Fatal("an unresolved overdue payment must be logged")
	}
}

func TestExpirySweepDoesNotStarveNewerPayments(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-2 * time.Hour)
	// More unresolved payments than one batch, all overdue earlier than the
	// payment that the provider reports as PAID.
	for i := range 120 {
		f.pendingWithExpiry(base.Add(time.Duration(i) * time.Second))
	}
	newest := f.pendingWithExpiry(base.Add(time.Hour))

	var calls int
	f.fake.StatusFunc = func(_ context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
		calls++
		if req.MerchantReference == newest.MerchantReference {
			return statusReport(newest, domain.PaymentStatusPaid), nil
		}
		return domain.StatusReport{}, errors.New("still unresolved")
	}
	if _, err := f.svc.ExpireOverduePayments(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls != 121 {
		t.Fatalf("inquired %d payments; want every overdue payment (121)", calls)
	}
	f.mustState(newest.ID, domain.PaymentStatusPaid, domain.OrderStatusPaid)
}
