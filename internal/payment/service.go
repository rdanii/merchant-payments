// Package payment is the payment engine: it orchestrates orders, payments,
// provider calls and provider reports, and owns transactional consistency.
package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/id"
	"merchant-payments/internal/provider"
)

// Repository is the persistence contract. Implementations must enforce the
// uniqueness rules documented on each method at the storage level.
type Repository interface {
	// CreateOrder fails with domain.ErrDuplicateOrder on a repeated
	// merchant reference.
	CreateOrder(ctx context.Context, o domain.Order) error
	// GetOrder returns domain.ErrNotFound when missing. forUpdate locks the
	// row until the surrounding transaction ends.
	GetOrder(ctx context.Context, id string, forUpdate bool) (domain.Order, error)
	GetOrderByMerchantReference(ctx context.Context, ref string) (domain.Order, error)
	UpdateOrder(ctx context.Context, o domain.Order) error

	// CreatePayment fails with domain.ErrActivePaymentExists when the order
	// already has a CREATED/PENDING payment and with
	// domain.ErrDuplicateIdempotencyKey on a repeated (order, key) pair.
	CreatePayment(ctx context.Context, p domain.Payment) error
	GetPayment(ctx context.Context, id string, forUpdate bool) (domain.Payment, error)
	GetPaymentByMerchantReference(ctx context.Context, prov domain.Provider, ref string) (domain.Payment, error)
	GetPaymentByIdempotencyKey(ctx context.Context, orderID, key string) (domain.Payment, error)
	UpdatePayment(ctx context.Context, p domain.Payment) error
	// ListOverduePayments returns up to limit CREATED/PENDING payments whose
	// expires_at is before cutoff and that sort after the cursor, ordered by
	// (expires_at, id).
	ListOverduePayments(ctx context.Context, cutoff time.Time, after OverdueCursor, limit int) ([]domain.Payment, error)

	// InsertEvent fails with domain.ErrDuplicateEvent on a repeated
	// (provider, provider event id) pair.
	InsertEvent(ctx context.Context, e domain.PaymentEvent) error
	ProviderEventExists(ctx context.Context, prov domain.Provider, eventID string) (bool, error)
}

// Store is a Repository that can run a function inside a transaction. If fn
// returns an error, every write made through the passed Repository is rolled
// back.
type Store interface {
	Repository
	InTx(ctx context.Context, fn func(Repository) error) error
}

// Service implements the payment use cases.
type Service struct {
	store           Store
	providers       *provider.Registry
	providerTimeout time.Duration
	log             *slog.Logger
	now             func() time.Time
	newID           func() string
}

// NewService builds a Service. providerTimeout bounds every provider call.
func NewService(store Store, providers *provider.Registry, providerTimeout time.Duration, log *slog.Logger) *Service {
	return &Service{
		store:           store,
		providers:       providers,
		providerTimeout: providerTimeout,
		log:             log,
		now:             func() time.Time { return time.Now().UTC() },
		newID:           id.New,
	}
}

// CreateOrder validates and stores a new order. Retrying with the same
// merchant reference and identical data returns the existing order with
// created=false; reusing the reference with different data fails with
// domain.ErrDuplicateOrder.
func (s *Service) CreateOrder(ctx context.Context, in domain.NewOrderInput) (o domain.Order, created bool, err error) {
	o, err = domain.NewOrder(s.newID(), in, s.now())
	if err != nil {
		return domain.Order{}, false, err
	}
	err = s.store.CreateOrder(ctx, o)
	if err == nil {
		return o, true, nil
	}
	if !errors.Is(err, domain.ErrDuplicateOrder) {
		return domain.Order{}, false, err
	}
	existing, err := s.store.GetOrderByMerchantReference(ctx, in.MerchantReference)
	if err != nil {
		return domain.Order{}, false, err
	}
	if existing.Amount != in.Amount || existing.Currency != in.Currency || existing.Description != in.Description {
		return domain.Order{}, false, domain.ErrDuplicateOrder
	}
	return existing, false, nil
}

// GetOrder returns an order by id.
func (s *Service) GetOrder(ctx context.Context, id string) (domain.Order, error) {
	return s.store.GetOrder(ctx, id, false)
}

// GetPayment returns a payment by id.
func (s *Service) GetPayment(ctx context.Context, id string) (domain.Payment, error) {
	return s.store.GetPayment(ctx, id, false)
}

// CreatePayment records a payment for an order and asks the provider to start
// it. The result is at most PENDING: success is only ever set by a verified
// provider report.
//
// created is false when the idempotency key replays an earlier request. When
// the provider call's outcome is unknown, the payment is returned together
// with domain.ErrProviderOutcomeUnknown and stays CREATED until an inquiry or
// webhook resolves it.
func (s *Service) CreatePayment(ctx context.Context, orderID string, in domain.NewPaymentInput) (p domain.Payment, created bool, err error) {
	if err := in.Validate(); err != nil {
		return domain.Payment{}, false, err
	}
	adapter, err := s.providers.Get(in.Provider)
	if err != nil {
		return domain.Payment{}, false, err
	}
	if !adapter.SupportsMethod(in.Method) {
		return domain.Payment{}, false, fmt.Errorf("%w: %s via %s", domain.ErrUnsupportedMethod, in.Method, in.Provider)
	}

	replay := false
	err = s.store.InTx(ctx, func(r Repository) error {
		order, err := r.GetOrder(ctx, orderID, true) // serializes payment creation per order
		if err != nil {
			return err
		}
		existing, err := r.GetPaymentByIdempotencyKey(ctx, orderID, in.IdempotencyKey)
		switch {
		case err == nil:
			if existing.Provider != in.Provider || existing.Method != in.Method {
				return domain.ErrIdempotencyConflict
			}
			p, replay = existing, true
			return nil
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}

		p, err = domain.NewPayment(s.newID(), order, in, s.now())
		if err != nil {
			return err
		}
		if err := r.CreatePayment(ctx, p); err != nil {
			return err
		}
		return r.InsertEvent(ctx, s.event(p, domain.EventPaymentCreated, domain.SourceAPI, domain.OutcomeApplied, "", p.Status))
	})
	if err != nil {
		return domain.Payment{}, false, err
	}
	if replay {
		return p, false, nil
	}

	// The provider call and the recording of its result must not be aborted
	// by the client disconnecting: once a request may have reached the
	// provider, we need to record what happened.
	detached := context.WithoutCancel(ctx)
	callCtx, cancel := context.WithTimeout(detached, s.providerTimeout)
	defer cancel()
	res, callErr := adapter.CreatePayment(callCtx, provider.CreateRequest{
		MerchantReference: p.MerchantReference,
		Amount:            p.Amount,
		Currency:          p.Currency,
		Method:            p.Method,
	})

	p, err = s.recordCreateResult(detached, p.ID, res, callErr)
	return p, true, err
}

func (s *Service) recordCreateResult(ctx context.Context, paymentID string, res provider.CreateResult, callErr error) (domain.Payment, error) {
	if callErr == nil && res.ProviderReference == "" {
		callErr = errors.New("provider accepted request without a provider reference")
	}
	var p domain.Payment
	var resultErr error
	err := s.store.InTx(ctx, func(r Repository) error {
		var err error
		if p, err = r.GetPayment(ctx, paymentID, true); err != nil {
			return err
		}
		from := p.Status
		var ev domain.PaymentEvent

		switch {
		case callErr == nil:
			ev = s.event(p, domain.EventProviderRequestOK, domain.SourceAPI, domain.OutcomeNoChange, "", "")
			ev.ProviderReference = res.ProviderReference
			if p.Status == domain.PaymentStatusCreated {
				if err := p.MarkPending(res.ProviderReference, res.Action, res.ExpiresAt, s.now()); err != nil {
					return err
				}
				ev.Outcome = domain.OutcomeApplied
			} else {
				// A provider report already advanced the payment (e.g. an
				// early webhook); keep that state.
				ev.Reason = "payment already " + string(p.Status)
			}
		case errors.Is(callErr, provider.ErrRejected):
			ev = s.event(p, domain.EventProviderRequestFailed, domain.SourceAPI, domain.OutcomeNoChange, "provider rejected request", "")
			if p.Status == domain.PaymentStatusCreated {
				if err := p.MarkFailed(s.now()); err != nil {
					return err
				}
				ev.Outcome = domain.OutcomeApplied
			}
		default:
			s.log.WarnContext(ctx, "provider create outcome unknown",
				"payment_id", p.ID, "provider", p.Provider, "error", callErr)
			ev = s.event(p, domain.EventProviderRequestUnknown, domain.SourceAPI, domain.OutcomeNoChange, "provider call failed or timed out", "")
			resultErr = domain.ErrProviderOutcomeUnknown
		}

		ev.FromStatus, ev.ToStatus = from, p.Status
		if p.Status != from {
			if err := r.UpdatePayment(ctx, p); err != nil {
				return err
			}
		}
		return r.InsertEvent(ctx, ev)
	})
	if err != nil {
		return domain.Payment{}, err
	}
	return p, resultErr
}

// ReportResult describes how a provider report was handled.
type ReportResult struct {
	Payment   domain.Payment // zero when the report matched no payment
	Outcome   domain.EventOutcome
	Duplicate bool // the provider event was already processed earlier
}

// HandleWebhook authenticates a webhook through the provider's adapter and
// applies the reported status. Duplicate deliveries are detected by provider
// event id and have no effect.
func (s *Service) HandleWebhook(ctx context.Context, prov domain.Provider, w provider.Webhook) (ReportResult, error) {
	adapter, err := s.providers.Get(prov)
	if err != nil {
		return ReportResult{}, err
	}
	report, err := adapter.ParseWebhook(ctx, w)
	if err != nil {
		// Unauthenticated or unparseable input is logged but not persisted:
		// it cannot be attributed to the provider, and storing it would let
		// anyone write to the audit trail. Headers and body are never logged.
		s.log.WarnContext(ctx, "webhook not accepted", "provider", prov, "reason", webhookErrorReason(err))
		return ReportResult{}, err
	}
	var invalid error
	switch {
	case report.Provider != prov:
		invalid = fmt.Errorf("%w: adapter returned provider %q", domain.ErrInvalidProviderReport, report.Provider)
	case report.ProviderEventID == "":
		invalid = fmt.Errorf("%w: missing provider event id", domain.ErrInvalidProviderReport)
	}
	if invalid != nil {
		report.Provider = prov // attribute the record to the authenticated route
		report.ProviderEventID = ""
		return ReportResult{Outcome: domain.OutcomeRejected}, s.recordRejection(ctx, domain.SourceWebhook, report, invalid)
	}
	return s.applyReport(ctx, domain.SourceWebhook, report)
}

func webhookErrorReason(err error) string {
	switch {
	case errors.Is(err, provider.ErrInvalidSignature):
		return "invalid signature"
	case errors.Is(err, provider.ErrMalformedWebhook):
		return "malformed payload"
	}
	return "adapter error"
}

// recordRejection stores a rejected provider report in its own transaction
// and returns rejection. A concurrent duplicate of the same provider event is
// reported as a duplicate, never stored twice.
func (s *Service) recordRejection(ctx context.Context, source domain.EventSource, report domain.StatusReport, rejection error) error {
	ev := reportEvent(s.newID(), source, report, s.now())
	ev.Outcome, ev.Reason = domain.OutcomeRejected, rejection.Error()
	if p, err := s.store.GetPaymentByMerchantReference(ctx, report.Provider, report.MerchantReference); err == nil {
		ev.PaymentID, ev.FromStatus, ev.ToStatus = p.ID, p.Status, p.Status
	}
	s.log.WarnContext(ctx, "provider report rejected",
		"payment_id", ev.PaymentID, "provider", report.Provider, "source", source, "reason", rejection.Error())
	if err := s.store.InsertEvent(ctx, ev); err != nil && !errors.Is(err, domain.ErrDuplicateEvent) {
		return fmt.Errorf("recording rejected report: %w", err)
	}
	return rejection
}

// InquirePayment asks the provider for the authoritative status of a payment
// and applies it. It is the resolution path for payments whose provider
// outcome is unknown and the basis for reconciliation jobs.
func (s *Service) InquirePayment(ctx context.Context, paymentID string) (ReportResult, error) {
	p, err := s.store.GetPayment(ctx, paymentID, false)
	if err != nil {
		return ReportResult{}, err
	}
	adapter, err := s.providers.Get(p.Provider)
	if err != nil {
		return ReportResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	defer cancel()
	report, err := adapter.GetPaymentStatus(callCtx, provider.StatusRequest{
		MerchantReference: p.MerchantReference,
		ProviderReference: p.ProviderReference,
	})
	if errors.Is(err, provider.ErrInquiryUnsupported) {
		return ReportResult{}, err
	}
	if err != nil {
		return ReportResult{}, fmt.Errorf("%w: %v", domain.ErrProviderOutcomeUnknown, err)
	}
	report.ProviderEventID = "" // inquiries are not deduplicated; the state machine makes them idempotent
	return s.applyReport(ctx, domain.SourceInquiry, report)
}

// applyReport validates a provider report and applies it atomically: the
// payment status, the order status (on success) and the audit event are
// written in one transaction. Rejected reports are still recorded as events
// and the rejection is returned as an error.
func (s *Service) applyReport(ctx context.Context, source domain.EventSource, report domain.StatusReport) (ReportResult, error) {
	var res ReportResult
	var rejection error

	err := s.store.InTx(ctx, func(r Repository) error {
		res, rejection = ReportResult{}, nil

		if report.ProviderEventID != "" {
			seen, err := r.ProviderEventExists(ctx, report.Provider, report.ProviderEventID)
			if err != nil {
				return err
			}
			if seen {
				res.Duplicate = true
				return nil
			}
		}

		ev := reportEvent(s.newID(), source, report, s.now())

		found, err := r.GetPaymentByMerchantReference(ctx, report.Provider, report.MerchantReference)
		if errors.Is(err, domain.ErrNotFound) {
			ev.Outcome, ev.Reason = domain.OutcomeRejected, "no payment matches merchant reference"
			rejection = fmt.Errorf("%w: unknown merchant reference", domain.ErrConfirmationMismatch)
			res.Outcome = ev.Outcome
			return r.InsertEvent(ctx, ev)
		}
		if err != nil {
			return err
		}

		// Lock order before payment, the same order used when creating
		// payments, so concurrent flows cannot deadlock.
		order, err := r.GetOrder(ctx, found.OrderID, true)
		if err != nil {
			return err
		}
		p, err := r.GetPayment(ctx, found.ID, true)
		if err != nil {
			return err
		}

		ev.PaymentID, ev.FromStatus = p.ID, p.Status
		changed, applyErr := p.ApplyReport(report, s.now())
		ev.ToStatus = p.Status

		switch {
		case applyErr != nil:
			ev.Outcome, ev.Reason = domain.OutcomeRejected, applyErr.Error()
			rejection = applyErr
			s.log.WarnContext(ctx, "provider report rejected",
				"payment_id", p.ID, "provider", report.Provider, "source", source, "reason", applyErr.Error())
		case !changed:
			ev.Outcome = domain.OutcomeNoChange
		default:
			ev.Outcome = domain.OutcomeApplied
			if err := r.UpdatePayment(ctx, p); err != nil {
				return err
			}
			if p.Status == domain.PaymentStatusPaid {
				if err := order.MarkPaid(*p.PaidAt); err != nil {
					// Guarded by the single-active-payment rule; reaching
					// this means the data needs manual reconciliation.
					return fmt.Errorf("confirming payment %s: %w", p.ID, err)
				}
				if err := r.UpdateOrder(ctx, order); err != nil {
					return err
				}
			}
		}

		res.Payment, res.Outcome = p, ev.Outcome
		return r.InsertEvent(ctx, ev)
	})
	if errors.Is(err, domain.ErrDuplicateEvent) {
		// A concurrent delivery of the same event committed first.
		return ReportResult{Duplicate: true}, nil
	}
	if errors.Is(err, domain.ErrConfirmationMismatch) || errors.Is(err, domain.ErrOrderAlreadyPaid) {
		// Rejected by a storage constraint after validation (for example a
		// provider reference already used by another payment). The
		// transaction rolled back; keep an audit record of the rejection.
		return ReportResult{Outcome: domain.OutcomeRejected}, s.recordRejection(ctx, source, report, err)
	}
	if err != nil {
		return ReportResult{}, err
	}
	if res.Duplicate {
		return res, nil
	}
	if res.Outcome == domain.OutcomeApplied {
		s.log.InfoContext(ctx, "payment status updated",
			"payment_id", res.Payment.ID, "status", res.Payment.Status, "source", source)
	}
	return res, rejection
}

func (s *Service) event(p domain.Payment, typ domain.EventType, src domain.EventSource, outcome domain.EventOutcome, reason string, to domain.PaymentStatus) domain.PaymentEvent {
	return domain.PaymentEvent{
		ID:                s.newID(),
		PaymentID:         p.ID,
		Type:              typ,
		Source:            src,
		Outcome:           outcome,
		Reason:            reason,
		Provider:          p.Provider,
		ProviderReference: p.ProviderReference,
		ToStatus:          to,
		CreatedAt:         s.now(),
	}
}

func reportEvent(eventID string, src domain.EventSource, r domain.StatusReport, now time.Time) domain.PaymentEvent {
	var occurredAt *time.Time
	if !r.OccurredAt.IsZero() {
		t := r.OccurredAt.UTC()
		occurredAt = &t
	}
	return domain.PaymentEvent{
		ID:                    eventID,
		Type:                  domain.EventProviderReport,
		Source:                src,
		Provider:              r.Provider,
		ProviderEventID:       r.ProviderEventID,
		ProviderReference:     r.ProviderReference,
		ReportedStatus:        r.Status,
		ReportedAmount:        r.Amount,
		ReportedCurrency:      r.Currency,
		ReportedFundingSource: r.FundingSource,
		Payload:               r.Payload,
		ProviderOccurredAt:    occurredAt,
		CreatedAt:             now,
	}
}

const expiryBatchSize = 100

// OverdueCursor positions a scan of overdue payments. The zero value starts
// at the beginning.
type OverdueCursor struct {
	ExpiresAt time.Time
	ID        string
}

// ExpireOverduePayments handles CREATED/PENDING payments whose
// provider-issued expires_at is more than grace in the past, so they stop
// blocking new payment attempts for their order. The grace period absorbs
// clock skew and late provider notifications.
//
// For each overdue payment the provider is asked first, through the same
// path as POST /payments/{id}/inquiry (no database lock is held during the
// provider call):
//   - the provider answers: its report is applied (PAID, FAILED, ...) and the
//     payment is never expired locally, even if it is still PENDING;
//   - the adapter does not support inquiry: the payment is expired locally;
//   - the inquiry fails or its answer is rejected: nothing changes and the
//     payment is retried on the next sweep. An unknown outcome is never
//     turned into EXPIRED or FAILED.
//
// The scan walks every overdue payment with a cursor, so payments that stay
// unresolved cannot starve newer ones. Payments without expires_at (CREATED
// with an unknown provider outcome) are not touched. It returns the number of
// payments expired locally.
func (s *Service) ExpireOverduePayments(ctx context.Context, grace time.Duration) (int, error) {
	cutoff := s.now().Add(-grace)
	expired := 0
	var cursor OverdueCursor
	for {
		due, err := s.store.ListOverduePayments(ctx, cutoff, cursor, expiryBatchSize)
		if err != nil {
			return expired, err
		}
		for _, d := range due {
			if s.resolveOverdue(ctx, d.ID, cutoff) {
				expired++
			}
		}
		if len(due) < expiryBatchSize {
			return expired, nil
		}
		last := due[len(due)-1]
		cursor = OverdueCursor{ExpiresAt: *last.ExpiresAt, ID: last.ID}
	}
}

// resolveOverdue asks the provider about one overdue payment and expires it
// locally only if the provider cannot be asked. It reports whether the
// payment was expired locally.
func (s *Service) resolveOverdue(ctx context.Context, paymentID string, cutoff time.Time) bool {
	res, err := s.InquirePayment(ctx, paymentID)
	switch {
	case err == nil:
		if res.Outcome == domain.OutcomeApplied {
			s.log.InfoContext(ctx, "overdue payment resolved by inquiry",
				"payment_id", paymentID, "status", res.Payment.Status)
		}
		return false
	case errors.Is(err, provider.ErrInquiryUnsupported):
		ok, err := s.expirePayment(ctx, paymentID, cutoff)
		if err != nil {
			s.log.ErrorContext(ctx, "payment expiry failed", "payment_id", paymentID, "error", err)
			return false
		}
		return ok
	default:
		// Keep going so one unresolved payment cannot block the others.
		s.log.WarnContext(ctx, "overdue payment not resolved; will retry",
			"payment_id", paymentID, "error", err)
		return false
	}
}

func (s *Service) expirePayment(ctx context.Context, paymentID string, cutoff time.Time) (bool, error) {
	expired := false
	err := s.store.InTx(ctx, func(r Repository) error {
		p, err := r.GetPayment(ctx, paymentID, true)
		if err != nil {
			return err
		}
		// Re-check under the row lock: a provider report may have won the race.
		if !p.Status.Active() || p.ExpiresAt == nil || !p.ExpiresAt.Before(cutoff) {
			return nil
		}
		from := p.Status
		if err := p.MarkExpired(s.now()); err != nil {
			return err
		}
		if err := r.UpdatePayment(ctx, p); err != nil {
			return err
		}
		ev := s.event(p, domain.EventPaymentExpired, domain.SourceSystem, domain.OutcomeApplied,
			"provider-issued expires_at passed without confirmation", p.Status)
		ev.FromStatus = from
		expired = true
		return r.InsertEvent(ctx, ev)
	})
	if err == nil && expired {
		s.log.InfoContext(ctx, "payment expired locally", "payment_id", paymentID)
	}
	return expired, err
}
