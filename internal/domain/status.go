package domain

import (
	"fmt"
	"time"
)

// PaymentStatus is the lifecycle state of a payment.
type PaymentStatus string

const (
	// PaymentStatusCreated: recorded locally; the provider has not (yet)
	// acknowledged it, or the outcome of the provider call is unknown.
	PaymentStatusCreated PaymentStatus = "CREATED"
	// PaymentStatusPending: the provider accepted the payment request and is
	// waiting for the customer.
	PaymentStatusPending PaymentStatus = "PENDING"
	// PaymentStatusPaid: the provider confirmed success. Terminal.
	PaymentStatusPaid PaymentStatus = "PAID"
	// PaymentStatusFailed: the provider rejected or failed the payment. Terminal.
	PaymentStatusFailed PaymentStatus = "FAILED"
	// PaymentStatusExpired: the provider expired the payment. Terminal.
	PaymentStatusExpired PaymentStatus = "EXPIRED"
)

// transitions lists every allowed status change. Terminal statuses have no
// outgoing transitions; reversals (refunds, chargebacks, reconciliation
// corrections) must be introduced as explicit new transitions.
var transitions = map[PaymentStatus][]PaymentStatus{
	PaymentStatusCreated: {PaymentStatusPending, PaymentStatusPaid, PaymentStatusFailed, PaymentStatusExpired},
	PaymentStatusPending: {PaymentStatusPaid, PaymentStatusFailed, PaymentStatusExpired},
}

// CanTransition reports whether a payment may move from one status to another.
func CanTransition(from, to PaymentStatus) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transitions are allowed from s.
func (s PaymentStatus) Terminal() bool { return len(transitions[s]) == 0 }

// Active reports whether a payment in status s may still be completed.
func (s PaymentStatus) Active() bool {
	return s == PaymentStatusCreated || s == PaymentStatusPending
}

// ReportedByProvider reports whether s is a status a provider report may carry.
func (s PaymentStatus) ReportedByProvider() bool {
	switch s {
	case PaymentStatusPending, PaymentStatusPaid, PaymentStatusFailed, PaymentStatusExpired:
		return true
	}
	return false
}

// MarkPending records the provider's acceptance of a payment request.
func (p *Payment) MarkPending(providerRef string, action CustomerAction, expiresAt *time.Time, now time.Time) error {
	if !CanTransition(p.Status, PaymentStatusPending) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, p.Status, PaymentStatusPending)
	}
	p.Status = PaymentStatusPending
	p.ProviderReference = providerRef
	p.Action = action
	p.ExpiresAt = expiresAt
	p.UpdatedAt = now
	return nil
}

// MarkFailed records a definitive provider rejection of the payment request.
func (p *Payment) MarkFailed(now time.Time) error {
	if !CanTransition(p.Status, PaymentStatusFailed) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, p.Status, PaymentStatusFailed)
	}
	p.Status = PaymentStatusFailed
	p.UpdatedAt = now
	return nil
}

// MarkExpired records that the payment passed its provider-issued expiry
// without a provider confirmation.
func (p *Payment) MarkExpired(now time.Time) error {
	if !CanTransition(p.Status, PaymentStatusExpired) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, p.Status, PaymentStatusExpired)
	}
	p.Status = PaymentStatusExpired
	p.UpdatedAt = now
	return nil
}
