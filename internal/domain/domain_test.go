package domain

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func TestNewOrderValidation(t *testing.T) {
	valid := NewOrderInput{MerchantReference: "INV-2026-0001", Amount: 150000, Currency: "IDR"}

	o, err := NewOrder("id-1", valid, now)
	if err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	if o.Status != OrderStatusCreated || o.PaidAt != nil {
		t.Fatalf("new order must be CREATED and unpaid, got %s", o.Status)
	}

	tests := []struct {
		name  string
		mod   func(*NewOrderInput)
		field string
	}{
		{"missing reference", func(in *NewOrderInput) { in.MerchantReference = "" }, "merchant_reference"},
		{"reference with spaces", func(in *NewOrderInput) { in.MerchantReference = "INV 1" }, "merchant_reference"},
		{"zero amount", func(in *NewOrderInput) { in.Amount = 0 }, "amount"},
		{"negative amount", func(in *NewOrderInput) { in.Amount = -1 }, "amount"},
		{"lowercase currency", func(in *NewOrderInput) { in.Currency = "idr" }, "currency"},
		{"missing currency", func(in *NewOrderInput) { in.Currency = "" }, "currency"},
		{"long description", func(in *NewOrderInput) { in.Description = string(make([]byte, 256)) }, "description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := valid
			tt.mod(&in)
			_, err := NewOrder("id", in, now)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if _, ok := ve.Fields[tt.field]; !ok {
				t.Fatalf("want error on %q, got %v", tt.field, ve.Fields)
			}
		})
	}
}

func TestOrderMarkPaidTwice(t *testing.T) {
	o, _ := NewOrder("id", NewOrderInput{MerchantReference: "R1", Amount: 1, Currency: "IDR"}, now)
	if err := o.MarkPaid(now); err != nil {
		t.Fatal(err)
	}
	if err := o.MarkPaid(now); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Fatalf("want ErrOrderAlreadyPaid, got %v", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	all := []PaymentStatus{PaymentStatusCreated, PaymentStatusPending, PaymentStatusPaid, PaymentStatusFailed, PaymentStatusExpired}
	allowed := map[[2]PaymentStatus]bool{
		{PaymentStatusCreated, PaymentStatusPending}: true,
		{PaymentStatusCreated, PaymentStatusPaid}:    true,
		{PaymentStatusCreated, PaymentStatusFailed}:  true,
		{PaymentStatusCreated, PaymentStatusExpired}: true,
		{PaymentStatusPending, PaymentStatusPaid}:    true,
		{PaymentStatusPending, PaymentStatusFailed}:  true,
		{PaymentStatusPending, PaymentStatusExpired}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := CanTransition(from, to), allowed[[2]PaymentStatus{from, to}]; got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range []PaymentStatus{PaymentStatusPaid, PaymentStatusFailed, PaymentStatusExpired} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
}

func newTestPayment(t *testing.T, method PaymentMethod) Payment {
	t.Helper()
	o, _ := NewOrder("order-1", NewOrderInput{MerchantReference: "R1", Amount: 150000, Currency: "IDR"}, now)
	p, err := NewPayment("pay-1", o, NewPaymentInput{Provider: "acme", Method: method, IdempotencyKey: "key-00001"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.MarkPending("prov-ref-1", CustomerAction{}, nil, now); err != nil {
		t.Fatal(err)
	}
	return p
}

func paidReport() StatusReport {
	return StatusReport{
		Provider: "acme", ProviderEventID: "evt-1", ProviderReference: "prov-ref-1",
		MerchantReference: "pay-1", Status: PaymentStatusPaid, Amount: amount(150000), Currency: "IDR",
	}
}

func TestNewPayment(t *testing.T) {
	o, _ := NewOrder("order-1", NewOrderInput{MerchantReference: "R1", Amount: 150000, Currency: "IDR"}, now)
	in := NewPaymentInput{Provider: "acme", Method: PaymentMethodQRIS, IdempotencyKey: "key-00001"}

	p, err := NewPayment("pay-1", o, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != PaymentStatusCreated || p.PaidAt != nil {
		t.Fatalf("new payment must be CREATED, got %s", p.Status)
	}
	if p.FundingSource != FundingSourceUnknown {
		t.Fatalf("new payment funding source must be UNKNOWN, got %s", p.FundingSource)
	}
	if p.Amount != o.Amount || p.Currency != o.Currency {
		t.Fatal("payment amount/currency must come from the order")
	}

	bad := []NewPaymentInput{
		{Provider: "", Method: PaymentMethodQRIS, IdempotencyKey: "key-00001"},
		{Provider: "Acme!", Method: PaymentMethodQRIS, IdempotencyKey: "key-00001"},
		{Provider: "acme", Method: "CASH", IdempotencyKey: "key-00001"},
		{Provider: "acme", Method: PaymentMethodQRIS, IdempotencyKey: ""},
		{Provider: "acme", Method: PaymentMethodQRIS, IdempotencyKey: "short"},
	}
	for _, in := range bad {
		var ve *ValidationError
		if _, err := NewPayment("p", o, in, now); !errors.As(err, &ve) {
			t.Errorf("%+v: want ValidationError, got %v", in, err)
		}
	}

	_ = o.MarkPaid(now)
	if _, err := NewPayment("pay-2", o, in, now); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Fatalf("want ErrOrderAlreadyPaid, got %v", err)
	}
}

func TestApplyReportSuccess(t *testing.T) {
	p := newTestPayment(t, PaymentMethodEWallet)
	r := paidReport()
	r.FundingSource = FundingSourceEWalletBalance

	changed, err := p.ApplyReport(r, now)
	if err != nil || !changed {
		t.Fatalf("ApplyReport = %v, %v", changed, err)
	}
	if p.Status != PaymentStatusPaid || p.PaidAt == nil {
		t.Fatalf("want PAID with paid_at, got %s", p.Status)
	}
	if p.FundingSource != FundingSourceEWalletBalance {
		t.Fatalf("want reported funding source, got %s", p.FundingSource)
	}

	// Repeating the confirmation is a no-op.
	changed, err = p.ApplyReport(r, now)
	if err != nil || changed {
		t.Fatalf("repeat ApplyReport = %v, %v; want no-op", changed, err)
	}
}

func TestApplyReportFundingSourceNeverInferred(t *testing.T) {
	for _, fs := range []FundingSource{"", "SOMETHING_NEW", FundingSourceUnknown} {
		// A PAYLATER *method* must not imply a PAYLATER *funding source*.
		p := newTestPayment(t, PaymentMethodPayLater)
		r := paidReport()
		r.FundingSource = fs
		if _, err := p.ApplyReport(r, now); err != nil {
			t.Fatal(err)
		}
		if p.FundingSource != FundingSourceUnknown {
			t.Errorf("reported %q: want UNKNOWN, got %s", fs, p.FundingSource)
		}
	}
}

func TestApplyReportRejections(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*StatusReport)
		want error
	}{
		{"amount mismatch", func(r *StatusReport) { r.Amount = amount(149999) }, ErrConfirmationMismatch},
		{"missing amount", func(r *StatusReport) { r.Amount = nil }, ErrConfirmationMismatch},
		{"explicit zero amount", func(r *StatusReport) { r.Amount = amount(0) }, ErrConfirmationMismatch},
		{"currency mismatch", func(r *StatusReport) { r.Currency = "USD" }, ErrConfirmationMismatch},
		{"provider mismatch", func(r *StatusReport) { r.Provider = "other" }, ErrConfirmationMismatch},
		{"merchant reference mismatch", func(r *StatusReport) { r.MerchantReference = "pay-2" }, ErrConfirmationMismatch},
		{"provider reference mismatch", func(r *StatusReport) { r.ProviderReference = "prov-ref-2" }, ErrConfirmationMismatch},
		{"missing provider reference", func(r *StatusReport) { r.ProviderReference = "" }, ErrInvalidProviderReport},
		{"unknown status", func(r *StatusReport) { r.Status = "SUCCESS" }, ErrInvalidProviderReport},
		{"CREATED is not a provider status", func(r *StatusReport) { r.Status = PaymentStatusCreated }, ErrInvalidProviderReport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPayment(t, PaymentMethodQRIS)
			r := paidReport()
			tt.mod(&r)
			if _, err := p.ApplyReport(r, now); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			if p.Status != PaymentStatusPending || p.PaidAt != nil {
				t.Fatalf("rejected report must not change payment, got %s", p.Status)
			}
		})
	}
}

func TestApplyReportCannotLeavePaid(t *testing.T) {
	for _, to := range []PaymentStatus{PaymentStatusPending, PaymentStatusFailed, PaymentStatusExpired} {
		p := newTestPayment(t, PaymentMethodQRIS)
		if _, err := p.ApplyReport(paidReport(), now); err != nil {
			t.Fatal(err)
		}
		r := paidReport()
		r.Status = to
		if _, err := p.ApplyReport(r, now); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("PAID -> %s: want ErrInvalidTransition, got %v", to, err)
		}
		if p.Status != PaymentStatusPaid {
			t.Errorf("payment left PAID: %s", p.Status)
		}
	}
}

func TestMarkPendingAfterPaidRejected(t *testing.T) {
	p := newTestPayment(t, PaymentMethodQRIS)
	if _, err := p.ApplyReport(paidReport(), now); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkPending("x", CustomerAction{}, nil, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

func amount(v int64) *int64 { return &v }

func TestMarkExpired(t *testing.T) {
	p := newTestPayment(t, PaymentMethodQRIS)
	if err := p.MarkExpired(now); err != nil || p.Status != PaymentStatusExpired {
		t.Fatalf("PENDING -> EXPIRED: %v, %s", err, p.Status)
	}

	paid := newTestPayment(t, PaymentMethodQRIS)
	if _, err := paid.ApplyReport(paidReport(), now); err != nil {
		t.Fatal(err)
	}
	if err := paid.MarkExpired(now); !errors.Is(err, ErrInvalidTransition) || paid.Status != PaymentStatusPaid {
		t.Fatalf("PAID -> EXPIRED must be rejected: %v, %s", err, paid.Status)
	}
}

func TestNonSuccessReportAmountIsOptional(t *testing.T) {
	r := paidReport()
	r.Status = PaymentStatusFailed
	r.Amount, r.Currency = nil, ""

	p := newTestPayment(t, PaymentMethodQRIS)
	if _, err := p.ApplyReport(r, now); err != nil || p.Status != PaymentStatusFailed {
		t.Fatalf("FAILED without amount: %v, %s", err, p.Status)
	}

	// An explicit zero is a reported amount, not a missing one.
	r.Amount = amount(0)
	p = newTestPayment(t, PaymentMethodQRIS)
	if _, err := p.ApplyReport(r, now); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("FAILED with amount 0: want ErrConfirmationMismatch, got %v", err)
	}
}

func TestNormalizeOVOFundingSources(t *testing.T) {
	for _, fs := range []FundingSource{FundingSourceOVOCash, FundingSourceOVOPayLater} {
		if NormalizeFundingSource(fs) != fs {
			t.Errorf("%s must be kept", fs)
		}
	}
	if NormalizeFundingSource("OVO_POINTS") != FundingSourceUnknown {
		t.Error("undefined provider funding sources must become UNKNOWN")
	}
}
