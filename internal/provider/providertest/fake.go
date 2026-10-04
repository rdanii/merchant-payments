// Package providertest provides a scriptable provider.Adapter for tests.
// It must not be registered by the server.
package providertest

import (
	"context"
	"errors"
	"sync"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/provider"
)

// Fake is a provider.Adapter whose behaviour is controlled by its fields.
type Fake struct {
	ProviderName domain.Provider
	Methods      []domain.PaymentMethod
	CreateFunc   func(context.Context, provider.CreateRequest) (provider.CreateResult, error)
	StatusFunc   func(context.Context, provider.StatusRequest) (domain.StatusReport, error)
	WebhookFunc  func(context.Context, provider.Webhook) (domain.StatusReport, error)

	mu          sync.Mutex
	createCalls []provider.CreateRequest
}

// CreateCalls returns the create requests received so far.
func (f *Fake) CreateCalls() []provider.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.CreateRequest(nil), f.createCalls...)
}

// New returns a fake named name that supports methods and accepts every
// create request with providerRef.
func New(name domain.Provider, providerRef string, methods ...domain.PaymentMethod) *Fake {
	return &Fake{
		ProviderName: name,
		Methods:      methods,
		CreateFunc: func(context.Context, provider.CreateRequest) (provider.CreateResult, error) {
			return provider.CreateResult{ProviderReference: providerRef}, nil
		},
	}
}

func (f *Fake) Name() domain.Provider { return f.ProviderName }

func (f *Fake) SupportsMethod(m domain.PaymentMethod) bool {
	for _, s := range f.Methods {
		if s == m {
			return true
		}
	}
	return false
}

func (f *Fake) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, req)
	f.mu.Unlock()
	return f.CreateFunc(ctx, req)
}

func (f *Fake) GetPaymentStatus(ctx context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
	if f.StatusFunc == nil {
		// A fake without a status function behaves like a provider that has
		// no inquiry API.
		return domain.StatusReport{}, provider.ErrInquiryUnsupported
	}
	return f.StatusFunc(ctx, req)
}

func (f *Fake) ParseWebhook(ctx context.Context, w provider.Webhook) (domain.StatusReport, error) {
	if f.WebhookFunc == nil {
		return domain.StatusReport{}, errors.New("providertest: WebhookFunc not set")
	}
	return f.WebhookFunc(ctx, w)
}
