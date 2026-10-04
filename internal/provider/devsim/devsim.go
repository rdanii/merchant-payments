// Package devsim is a DEVELOPMENT-ONLY provider simulator. It lets the
// order → payment → confirmation flow be exercised locally without a real
// provider. It does not represent, imitate or approximate any real provider's
// API; its webhook format is defined here and nowhere else.
//
// It never confirms a payment by itself and never returns a QR code or other
// customer action: confirmation only happens when a developer posts a webhook
// signed with the configured secret. The server refuses to enable it when
// APP_ENV=production.
package devsim

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/provider"
)

// Name is the provider code of the simulator.
const Name domain.Provider = "devsim"

// SignatureHeader carries hex(HMAC-SHA256(secret, body)).
const SignatureHeader = "X-Devsim-Signature"

// Adapter implements provider.Adapter for local development.
type Adapter struct {
	secret []byte
}

// New returns a simulator that authenticates webhooks with secret.
func New(secret string) (*Adapter, error) {
	if len(secret) < 16 {
		return nil, errors.New("devsim: webhook secret must be at least 16 characters")
	}
	return &Adapter{secret: []byte(secret)}, nil
}

func (a *Adapter) Name() domain.Provider { return Name }

func (a *Adapter) SupportsMethod(m domain.PaymentMethod) bool { return m.Valid() }

func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.CreateResult{}, err
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return provider.CreateResult{}, err
	}
	return provider.CreateResult{
		ProviderReference: "devsim_" + hex.EncodeToString(b),
		Action:            domain.CustomerAction{Type: domain.ActionNone},
	}, nil
}

// GetPaymentStatus is not supported because the simulator keeps no state.
func (a *Adapter) GetPaymentStatus(context.Context, provider.StatusRequest) (domain.StatusReport, error) {
	return domain.StatusReport{}, provider.ErrInquiryUnsupported
}

// Notification is the simulator's webhook body.
type Notification struct {
	EventID           string `json:"event_id"`
	ProviderReference string `json:"provider_reference"`
	MerchantReference string `json:"merchant_reference"`
	Status            string `json:"status"`
	Amount            *int64 `json:"amount,omitempty"`
	Currency          string `json:"currency,omitempty"`
	FundingSource     string `json:"funding_source,omitempty"`
	OccurredAt        string `json:"occurred_at,omitempty"` // RFC 3339
}

func (a *Adapter) ParseWebhook(_ context.Context, w provider.Webhook) (domain.StatusReport, error) {
	if !a.validSignature(w.Body, w.Headers.Get(SignatureHeader)) {
		return domain.StatusReport{}, provider.ErrInvalidSignature
	}
	var n Notification
	dec := json.NewDecoder(bytes.NewReader(w.Body))
	dec.DisallowUnknownFields()
	err := dec.Decode(&n)
	if err != nil {
		return domain.StatusReport{}, fmt.Errorf("%w: %v", provider.ErrMalformedWebhook, err)
	}
	if n.EventID == "" {
		return domain.StatusReport{}, fmt.Errorf("%w: event_id required", provider.ErrMalformedWebhook)
	}
	var occurredAt time.Time
	if n.OccurredAt != "" {
		if occurredAt, err = time.Parse(time.RFC3339, n.OccurredAt); err != nil {
			return domain.StatusReport{}, fmt.Errorf("%w: occurred_at must be RFC 3339", provider.ErrMalformedWebhook)
		}
	}
	payload, err := json.Marshal(n) // re-encoded from known fields only
	if err != nil {
		return domain.StatusReport{}, err
	}
	return domain.StatusReport{
		Provider:          Name,
		ProviderEventID:   n.EventID,
		ProviderReference: n.ProviderReference,
		MerchantReference: n.MerchantReference,
		Status:            domain.PaymentStatus(n.Status),
		Amount:            n.Amount,
		Currency:          n.Currency,
		FundingSource:     domain.FundingSource(n.FundingSource),
		OccurredAt:        occurredAt,
		Payload:           payload,
	}, nil
}

// Sign returns the signature header value for body. Exposed for tests and
// local tooling.
func (a *Adapter) Sign(body []byte) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Adapter) validSignature(body []byte, sig string) bool {
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) == 0 {
		return false
	}
	want, _ := hex.DecodeString(a.Sign(body))
	return hmac.Equal(got, want)
}
