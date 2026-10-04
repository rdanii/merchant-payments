package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/id"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/provider"
	"merchant-payments/internal/provider/providertest"
	"merchant-payments/internal/store/postgres"
)

// These tests need a disposable PostgreSQL database; every table is
// truncated. Example:
//
//	TEST_DATABASE_URL=postgres://payments:payments@localhost:5432/payments_test?sslmode=disable go test ./...
//
// Set REQUIRE_DB_TESTS=1 (as CI does) to fail instead of skip when
// TEST_DATABASE_URL is missing.
func setup(t *testing.T) (*pgxpool.Pool, *postgres.Store) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("REQUIRE_DB_TESTS is set but TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE payment_events, payments, orders`); err != nil {
		t.Fatal(err)
	}
	return pool, postgres.New(pool)
}

const acme domain.Provider = "acme"

func newService(t *testing.T, store payment.Store) (*payment.Service, *providertest.Fake) {
	t.Helper()
	fake := providertest.New(acme, "", domain.PaymentMethodQRIS)
	fake.CreateFunc = func(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "acme-" + req.MerchantReference}, nil
	}
	reg, _ := provider.NewRegistry(fake)
	return payment.NewService(store, reg, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil))), fake
}

func createPending(t *testing.T, svc *payment.Service, ref string) (domain.Order, domain.Payment) {
	t.Helper()
	ctx := context.Background()
	o, _, err := svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: ref, Amount: 250000, Currency: "IDR"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-" + ref})
	if err != nil {
		t.Fatal(err)
	}
	return o, p
}

func paidReport(p domain.Payment, eventID string) domain.StatusReport {
	return domain.StatusReport{
		Provider: acme, ProviderEventID: eventID, ProviderReference: p.ProviderReference,
		MerchantReference: p.MerchantReference, Status: domain.PaymentStatusPaid,
		Amount: &p.Amount, Currency: p.Currency, FundingSource: domain.FundingSourceBankAccount,
		Payload: []byte(`{"event":"` + eventID + `"}`),
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool, _ := setup(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func TestConfirmationFlowPersists(t *testing.T) {
	_, store := setup(t)
	svc, fake := newService(t, store)
	ctx := context.Background()
	o, p := createPending(t, svc, "INV-PG-1")

	if p.Status != domain.PaymentStatusPending || p.ProviderReference == "" {
		t.Fatalf("unexpected payment %+v", p)
	}

	fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) {
		return paidReport(p, "evt-1"), nil
	}
	if _, err := svc.HandleWebhook(ctx, acme, provider.Webhook{}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetPayment(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	order, err := svc.GetOrder(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PaymentStatusPaid || order.Status != domain.OrderStatusPaid {
		t.Fatalf("payment=%s order=%s", got.Status, order.Status)
	}
	if got.FundingSource != domain.FundingSourceBankAccount || got.PaidAt == nil || order.PaidAt == nil {
		t.Fatalf("unexpected paid payment %+v", got)
	}
}

func TestConcurrentDuplicateWebhooks(t *testing.T) {
	pool, store := setup(t)
	svc, fake := newService(t, store)
	ctx := context.Background()
	_, p := createPending(t, svc, "INV-PG-2")
	fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) {
		return paidReport(p, "evt-dup"), nil
	}

	const n = 10
	var wg sync.WaitGroup
	results := make([]payment.ReportResult, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.HandleWebhook(ctx, acme, provider.Webhook{})
		}()
	}
	wg.Wait()

	applied := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("delivery %d: %v", i, errs[i])
		}
		if !results[i].Duplicate {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("want exactly one applied delivery, got %d", applied)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_events WHERE provider_event_id = 'evt-dup'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("want 1 event row, got %d", rows)
	}
}

func TestConcurrentPaymentCreationAllowsOneActive(t *testing.T) {
	_, store := setup(t)
	svc, _ := newService(t, store)
	ctx := context.Background()
	o, _, err := svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-PG-3", Amount: 1000, Currency: "IDR"})
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
				Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: fmt.Sprintf("idem-key-%d", i),
			})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, domain.ErrActivePaymentExists):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("want exactly one payment created, got %d", ok)
	}
}

func TestSchemaConstraints(t *testing.T) {
	pool, store := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	o, _ := domain.NewOrder(id.New(), domain.NewOrderInput{MerchantReference: "INV-PG-4", Amount: 1000, Currency: "IDR"}, now)
	if err := store.CreateOrder(ctx, o); err != nil {
		t.Fatal(err)
	}
	dup := o
	dup.ID = id.New()
	if err := store.CreateOrder(ctx, dup); !errors.Is(err, domain.ErrDuplicateOrder) {
		t.Fatalf("duplicate order: got %v", err)
	}

	in := domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-key-a"}
	p1, _ := domain.NewPayment(id.New(), o, in, now)
	if err := store.CreatePayment(ctx, p1); err != nil {
		t.Fatal(err)
	}
	p2, _ := domain.NewPayment(id.New(), o, in, now)
	if err := store.CreatePayment(ctx, p2); !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate idempotency key: got %v", err)
	}
	in.IdempotencyKey = "idem-key-b"
	p3, _ := domain.NewPayment(id.New(), o, in, now)
	if err := store.CreatePayment(ctx, p3); !errors.Is(err, domain.ErrActivePaymentExists) {
		t.Fatalf("second active payment: got %v", err)
	}

	ev := domain.PaymentEvent{ID: id.New(), PaymentID: p1.ID, Type: domain.EventProviderReport, Source: domain.SourceWebhook,
		Outcome: domain.OutcomeNoChange, Provider: acme, ProviderEventID: "evt-x", Payload: []byte("not json"), CreatedAt: now}
	if err := store.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("insert event with non-JSON payload: %v", err)
	}
	ev.ID = id.New()
	if err := store.InsertEvent(ctx, ev); !errors.Is(err, domain.ErrDuplicateEvent) {
		t.Fatalf("duplicate provider event: got %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE payment_events SET reason = 'tampered'`); err == nil {
		t.Fatal("payment_events must be append-only")
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET status = 'PAID' WHERE id = $1`, p1.ID); err == nil {
		t.Fatal("PAID without paid_at must violate a check constraint")
	}
}

func TestTransactionRollback(t *testing.T) {
	_, store := setup(t)
	ctx := context.Background()
	o, _ := domain.NewOrder(id.New(), domain.NewOrderInput{MerchantReference: "INV-PG-5", Amount: 1000, Currency: "IDR"}, time.Now().UTC())
	if err := store.CreateOrder(ctx, o); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := store.InTx(ctx, func(r payment.Repository) error {
		locked, err := r.GetOrder(ctx, o.ID, true)
		if err != nil {
			return err
		}
		if err := locked.MarkPaid(time.Now().UTC()); err != nil {
			return err
		}
		if err := r.UpdateOrder(ctx, locked); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	got, _ := store.GetOrder(ctx, o.ID, false)
	if got.Status != domain.OrderStatusCreated {
		t.Fatalf("rolled-back update persisted: %s", got.Status)
	}
}

func TestOrderRetryPersists(t *testing.T) {
	pool, store := setup(t)
	svc, _ := newService(t, store)
	ctx := context.Background()
	in := domain.NewOrderInput{MerchantReference: "INV-PG-RETRY", Amount: 1000, Currency: "IDR"}

	first, created, err := svc.CreateOrder(ctx, in)
	if err != nil || !created {
		t.Fatalf("first = %v, %v", created, err)
	}
	again, created, err := svc.CreateOrder(ctx, in)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry = %s, %v, %v", again.ID, created, err)
	}
	in.Amount = 2000
	if _, _, err := svc.CreateOrder(ctx, in); !errors.Is(err, domain.ErrDuplicateOrder) {
		t.Fatalf("conflicting reuse: got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE merchant_reference = 'INV-PG-RETRY'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("want exactly 1 order row, got %d (%v)", n, err)
	}
}

func TestExpiryPersists(t *testing.T) {
	pool, store := setup(t)
	svc, fake := newService(t, store)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	fake.CreateFunc = func(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "acme-" + req.MerchantReference, ExpiresAt: &past}, nil
	}
	o, p := createPending(t, svc, "INV-PG-EXP")
	if p.ExpiresAt == nil {
		t.Fatal("expires_at not stored")
	}
	// An unknown-outcome payment has no provider expiry and must never be
	// expired locally: it may have succeeded at the provider.
	fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, errors.New("timeout")
	}
	ou, _, err := svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-PG-EXP-U", Amount: 1000, Currency: "IDR"})
	if err != nil {
		t.Fatal(err)
	}
	unknown, _, err := svc.CreatePayment(ctx, ou.ID, domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-unknown-1"})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("got %v", err)
	}

	n, err := svc.ExpireOverduePayments(ctx, 5*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("ExpireOverduePayments = %d, %v", n, err)
	}
	got, _ := svc.GetPayment(ctx, p.ID)
	if got.Status != domain.PaymentStatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
	if u, _ := svc.GetPayment(ctx, unknown.ID); u.Status != domain.PaymentStatusCreated {
		t.Fatalf("unknown-outcome payment must stay CREATED, got %s", u.Status)
	}
	var evType, source string
	if err := pool.QueryRow(ctx, `SELECT event_type, source FROM payment_events
		WHERE payment_id = $1 ORDER BY created_at DESC LIMIT 1`, p.ID).Scan(&evType, &source); err != nil {
		t.Fatal(err)
	}
	if evType != string(domain.EventPaymentExpired) || source != string(domain.SourceSystem) {
		t.Fatalf("expiry event = %s/%s", evType, source)
	}

	// The partial unique index no longer blocks a new attempt.
	fake.CreateFunc = func(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "acme-" + req.MerchantReference}, nil
	}
	if _, created, err := svc.CreatePayment(ctx, o.ID, domain.NewPaymentInput{
		Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-after-expiry",
	}); err != nil || !created {
		t.Fatalf("new attempt after expiry: %v, %v", created, err)
	}
}

func TestEventAmountAndProviderTimePersistence(t *testing.T) {
	pool, store := setup(t)
	ctx := context.Background()
	providerTime := time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC)
	zero := int64(0)

	events := []domain.PaymentEvent{
		{ID: id.New(), Type: domain.EventProviderReport, Source: domain.SourceWebhook, Outcome: domain.OutcomeRejected,
			Provider: acme, ProviderEventID: "evt-none", CreatedAt: time.Now().UTC()},
		{ID: id.New(), Type: domain.EventProviderReport, Source: domain.SourceWebhook, Outcome: domain.OutcomeRejected,
			Provider: acme, ProviderEventID: "evt-zero", ReportedAmount: &zero, ProviderOccurredAt: &providerTime,
			CreatedAt: time.Now().UTC()},
	}
	for _, e := range events {
		if err := store.InsertEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	var amount *int64
	var occurred *time.Time
	if err := pool.QueryRow(ctx, `SELECT reported_amount, provider_occurred_at FROM payment_events
		WHERE provider_event_id = 'evt-none'`).Scan(&amount, &occurred); err != nil {
		t.Fatal(err)
	}
	if amount != nil || occurred != nil {
		t.Fatalf("unreported amount/time must be NULL, got %v / %v", amount, occurred)
	}
	if err := pool.QueryRow(ctx, `SELECT reported_amount, provider_occurred_at FROM payment_events
		WHERE provider_event_id = 'evt-zero'`).Scan(&amount, &occurred); err != nil {
		t.Fatal(err)
	}
	if amount == nil || *amount != 0 || occurred == nil || !occurred.Equal(providerTime) {
		t.Fatalf("explicit zero / provider time not preserved: %v / %v", amount, occurred)
	}
}

func TestPaymentCurrencyConstraint(t *testing.T) {
	pool, store := setup(t)
	svc, _ := newService(t, store)
	_, p := createPending(t, svc, "INV-PG-CUR")
	for _, bad := range []string{"idr", "I1R"} {
		if _, err := pool.Exec(context.Background(), `UPDATE payments SET currency = $2 WHERE id = $1`, p.ID, bad); err == nil {
			t.Errorf("currency %q accepted", bad)
		}
	}
}

func TestProviderReferenceCollisionIsAudited(t *testing.T) {
	pool, store := setup(t)
	svc, fake := newService(t, store)
	ctx := context.Background()

	// Payment A owns provider reference "acme-shared".
	fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "acme-shared"}, nil
	}
	createPending(t, svc, "INV-PG-A")

	// Payment B's provider outcome is unknown, so it has no reference yet.
	fake.CreateFunc = func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{}, errors.New("timeout")
	}
	ob, _, err := svc.CreateOrder(ctx, domain.NewOrderInput{MerchantReference: "INV-PG-B", Amount: 250000, Currency: "IDR"})
	if err != nil {
		t.Fatal(err)
	}
	pb, _, err := svc.CreatePayment(ctx, ob.ID, domain.NewPaymentInput{Provider: acme, Method: domain.PaymentMethodQRIS, IdempotencyKey: "idem-b-0001"})
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("got %v", err)
	}

	// A confirmation for B carrying A's provider reference passes validation
	// but violates the unique index; it must be rolled back and audited.
	report := paidReport(pb, "evt-collide")
	report.ProviderReference = "acme-shared"
	fake.WebhookFunc = func(context.Context, provider.Webhook) (domain.StatusReport, error) { return report, nil }
	if _, err := svc.HandleWebhook(ctx, acme, provider.Webhook{}); !errors.Is(err, domain.ErrConfirmationMismatch) {
		t.Fatalf("want ErrConfirmationMismatch, got %v", err)
	}
	got, _ := svc.GetPayment(ctx, pb.ID)
	order, _ := svc.GetOrder(ctx, ob.ID)
	if got.Status != domain.PaymentStatusCreated || order.Status != domain.OrderStatusCreated {
		t.Fatalf("payment=%s order=%s; want unchanged", got.Status, order.Status)
	}
	var outcome string
	var paymentID *string
	if err := pool.QueryRow(ctx, `SELECT outcome, payment_id::text FROM payment_events WHERE provider_event_id = 'evt-collide'`).
		Scan(&outcome, &paymentID); err != nil {
		t.Fatalf("rejection not audited: %v", err)
	}
	if outcome != "REJECTED" || paymentID == nil || *paymentID != pb.ID {
		t.Fatalf("audit = %s / %v", outcome, paymentID)
	}
	if res, err := svc.HandleWebhook(ctx, acme, provider.Webhook{}); err != nil || !res.Duplicate {
		t.Fatalf("redelivery = %+v, %v; want duplicate", res, err)
	}
}

func TestOverdueCursorPaging(t *testing.T) {
	_, store := setup(t)
	svc, fake := newService(t, store)
	ctx := context.Background()
	// Identical expiry times force the id tie-breaker.
	same := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	fake.CreateFunc = func(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
		return provider.CreateResult{ProviderReference: "acme-" + req.MerchantReference, ExpiresAt: &same}, nil
	}
	want := map[string]bool{}
	for i := range 5 {
		_, p := createPending(t, svc, fmt.Sprintf("INV-PG-CUR-%d", i))
		want[p.ID] = true
	}

	var got []string
	var cursor payment.OverdueCursor
	for range 10 {
		page, err := store.ListOverduePayments(ctx, time.Now(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			got = append(got, p.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = payment.OverdueCursor{ExpiresAt: *last.ExpiresAt, ID: last.ID}
	}
	if len(got) != 5 {
		t.Fatalf("paged %d payments, want 5: %v", len(got), got)
	}
	for i, id := range got {
		if !want[id] || i > 0 && got[i-1] >= id {
			t.Fatalf("pages must cover each payment once, ordered by id: %v", got)
		}
	}
}

func TestOVOFundingSourceConstraint(t *testing.T) {
	pool, store := setup(t)
	svc, _ := newService(t, store)
	_, p := createPending(t, svc, "INV-PG-FS")
	ctx := context.Background()
	for _, ok := range []string{"OVO_CASH", "OVO_PAYLATER", "UNKNOWN", "PAYLATER"} {
		if _, err := pool.Exec(ctx, `UPDATE payments SET funding_source = $2 WHERE id = $1`, p.ID, ok); err != nil {
			t.Errorf("funding source %s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"OVO_POINTS", "ovo_cash", ""} {
		if _, err := pool.Exec(ctx, `UPDATE payments SET funding_source = $2 WHERE id = $1`, p.ID, bad); err == nil {
			t.Errorf("funding source %q accepted", bad)
		}
	}
}
