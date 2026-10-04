// Package provider defines the boundary between the payment engine and
// external payment providers. Every provider-specific detail (endpoints,
// authentication, signature schemes, payload formats, status codes) lives in
// an adapter implementing Adapter; nothing outside an adapter may depend on
// provider-specific behaviour.
//
// No production adapters exist yet. To add one, create a package under
// internal/provider/<name>, implement Adapter from the provider's official
// API documentation, and register it in cmd/server/main.go.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"merchant-payments/internal/domain"
)

// Errors adapters return so the engine can decide what happened.
var (
	// ErrRejected: the provider definitively refused the request; it is safe
	// to treat the payment as failed.
	ErrRejected = errors.New("provider rejected request")
	// ErrInvalidSignature: a webhook failed authentication.
	ErrInvalidSignature = errors.New("invalid webhook signature")
	// ErrMalformedWebhook: a webhook could not be parsed.
	ErrMalformedWebhook = errors.New("malformed webhook")
	// ErrInquiryUnsupported: the adapter cannot query payment status. The
	// payment is unchanged; it can only be resolved by a webhook.
	ErrInquiryUnsupported = errors.New("provider does not support status inquiry")
	// Any other error (including context deadline errors) means the outcome
	// is unknown and the payment must be resolved through inquiry.
)

// CreateRequest asks the provider to start a payment.
type CreateRequest struct {
	MerchantReference string
	Amount            int64
	Currency          string
	Method            domain.PaymentMethod
}

// CreateResult is the provider's acceptance of a payment request. It never
// confirms payment success.
type CreateResult struct {
	ProviderReference string
	Action            domain.CustomerAction
	ExpiresAt         *time.Time
}

// StatusRequest identifies a payment to inquire about.
type StatusRequest struct {
	MerchantReference string
	ProviderReference string // may be empty if the create call's outcome was unknown
}

// Webhook is a raw inbound notification. Adapters must authenticate it
// before trusting any field.
type Webhook struct {
	Headers http.Header
	Body    []byte
}

// Adapter is implemented once per provider. Errors returned by an adapter are
// logged, so they must not contain credentials, signed URLs or tokens.
type Adapter interface {
	// Name is the provider code used in API requests and webhook URLs.
	Name() domain.Provider
	// SupportsMethod reports whether this adapter can process the method.
	// Adapters must only return true for methods the provider has been
	// verified to support.
	SupportsMethod(domain.PaymentMethod) bool
	// CreatePayment asks the provider to start a payment.
	CreatePayment(ctx context.Context, req CreateRequest) (CreateResult, error)
	// GetPaymentStatus queries the provider for the authoritative status.
	// The returned report has an empty ProviderEventID.
	GetPaymentStatus(ctx context.Context, req StatusRequest) (domain.StatusReport, error)
	// ParseWebhook authenticates and translates a webhook. It must return
	// ErrInvalidSignature when authentication fails and must set a stable,
	// unique ProviderEventID so duplicate deliveries can be detected.
	ParseWebhook(ctx context.Context, w Webhook) (domain.StatusReport, error)
}

// Registry holds the adapters enabled in this process.
type Registry struct {
	adapters map[domain.Provider]Adapter
}

// NewRegistry returns a registry containing adapters. Duplicate names are an
// error.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[domain.Provider]Adapter, len(adapters))}
	for _, a := range adapters {
		if _, dup := r.adapters[a.Name()]; dup {
			return nil, fmt.Errorf("provider %q registered twice", a.Name())
		}
		r.adapters[a.Name()] = a
	}
	return r, nil
}

// Get returns the adapter for name or domain.ErrUnsupportedProvider.
func (r *Registry) Get(name domain.Provider) (Adapter, error) {
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", domain.ErrUnsupportedProvider, name)
	}
	return a, nil
}

// Names lists registered providers in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, string(n))
	}
	sort.Strings(names)
	return names
}
