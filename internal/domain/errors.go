package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors describing business-rule failures. Transport layers map
// these to client-facing responses; they never carry secrets or raw provider
// payloads.
var (
	ErrNotFound               = errors.New("not found")
	ErrOrderAlreadyPaid       = errors.New("order is already paid")
	ErrDuplicateOrder         = errors.New("an order with this merchant_reference already exists with different data")
	ErrActivePaymentExists    = errors.New("order already has an active payment")
	ErrIdempotencyConflict    = errors.New("idempotency key was already used with different parameters")
	ErrInvalidTransition      = errors.New("invalid payment status transition")
	ErrUnsupportedProvider    = errors.New("unsupported provider")
	ErrUnsupportedMethod      = errors.New("payment method not supported by provider")
	ErrConfirmationMismatch   = errors.New("provider confirmation does not match payment")
	ErrInvalidProviderReport  = errors.New("invalid provider report")
	ErrProviderOutcomeUnknown = errors.New("provider outcome unknown; payment requires inquiry")

	// Storage conflicts surfaced by repositories.
	ErrDuplicateEvent          = errors.New("provider event already recorded")
	ErrDuplicateIdempotencyKey = errors.New("idempotency key already used for this order")
)

// ValidationError lists request fields that failed validation.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, e.Fields[k]))
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// fieldErrors accumulates validation failures.
type fieldErrors map[string]string

func (f fieldErrors) add(field, msg string) {
	if _, exists := f[field]; !exists {
		f[field] = msg
	}
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}
