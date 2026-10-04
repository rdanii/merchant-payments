package devsim

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"merchant-payments/internal/provider"
)

// parseSigned parses body with a valid signature.
func parseSigned(a *Adapter, body string) error {
	h := http.Header{}
	h.Set(SignatureHeader, a.Sign([]byte(body)))
	_, err := a.ParseWebhook(context.Background(), provider.Webhook{Headers: h, Body: []byte(body)})
	return err
}

func TestParseWebhookOptionalFields(t *testing.T) {
	a, err := New("test-secret-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event_id":"e1","provider_reference":"r","merchant_reference":"m","status":"FAILED"}`)
	h := http.Header{}
	h.Set(SignatureHeader, a.Sign(body))
	r, err := a.ParseWebhook(context.Background(), provider.Webhook{Headers: h, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if r.Amount != nil || !r.OccurredAt.IsZero() {
		t.Fatalf("absent amount/occurred_at must stay absent: amount=%v occurred_at=%v", r.Amount, r.OccurredAt)
	}

	body = []byte(`{"event_id":"e2","provider_reference":"r","merchant_reference":"m","status":"PAID","amount":0,"currency":"IDR","occurred_at":"2026-10-01T08:30:00+07:00"}`)
	h.Set(SignatureHeader, a.Sign(body))
	r, err = a.ParseWebhook(context.Background(), provider.Webhook{Headers: h, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if r.Amount == nil || *r.Amount != 0 {
		t.Fatalf("explicit zero amount must be preserved, got %v", r.Amount)
	}
	if want := time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC); !r.OccurredAt.Equal(want) {
		t.Fatalf("occurred_at = %v, want %v", r.OccurredAt, want)
	}

	if err := parseSigned(a, `{"event_id":"e3","status":"PAID","occurred_at":"yesterday"}`); !errors.Is(err, provider.ErrMalformedWebhook) {
		t.Fatalf("bad occurred_at: want ErrMalformedWebhook, got %v", err)
	}
}

func TestInquiryUnsupported(t *testing.T) {
	a, _ := New("test-secret-0123456789")
	if _, err := a.GetPaymentStatus(context.Background(), provider.StatusRequest{}); !errors.Is(err, provider.ErrInquiryUnsupported) {
		t.Fatalf("got %v", err)
	}
}
