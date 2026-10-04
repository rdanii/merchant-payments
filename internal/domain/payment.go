package domain

import (
	"fmt"
	"regexp"
	"time"
)

// Provider identifies a payment provider adapter (for example "gopay").
// It says nothing about how a customer paid; see PaymentMethod and
// FundingSource for that.
type Provider string

var providerPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// Valid reports whether p is a syntactically valid provider code.
func (p Provider) Valid() bool { return providerPattern.MatchString(string(p)) }

// PaymentMethod is the channel the customer was asked to pay through.
// A method is requested by the merchant; it is not evidence of how the
// payment was funded.
type PaymentMethod string

const (
	PaymentMethodQRIS     PaymentMethod = "QRIS"
	PaymentMethodEWallet  PaymentMethod = "EWALLET"
	PaymentMethodPayLater PaymentMethod = "PAYLATER"
)

// Valid reports whether m is a known payment method.
func (m PaymentMethod) Valid() bool {
	switch m {
	case PaymentMethodQRIS, PaymentMethodEWallet, PaymentMethodPayLater:
		return true
	}
	return false
}

// FundingSource is what actually funded a confirmed payment, as reported by
// the provider. It must never be inferred from Provider or PaymentMethod:
// when the provider does not report it verifiably it is UNKNOWN.
type FundingSource string

const (
	FundingSourceUnknown        FundingSource = "UNKNOWN"
	FundingSourceEWalletBalance FundingSource = "EWALLET_BALANCE"
	FundingSourcePayLater       FundingSource = "PAYLATER"
	FundingSourceBankAccount    FundingSource = "BANK_ACCOUNT"
	FundingSourceDebitCard      FundingSource = "DEBIT_CARD"
	FundingSourceCreditCard     FundingSource = "CREDIT_CARD"

	// Provider-specific funding sources: set only by that provider's adapter
	// from explicit provider evidence.
	FundingSourceOVOCash     FundingSource = "OVO_CASH"
	FundingSourceOVOPayLater FundingSource = "OVO_PAYLATER"
)

// NormalizeFundingSource returns fs when it is a known value and UNKNOWN
// otherwise (including when it is empty).
func NormalizeFundingSource(fs FundingSource) FundingSource {
	switch fs {
	case FundingSourceEWalletBalance, FundingSourcePayLater, FundingSourceBankAccount,
		FundingSourceDebitCard, FundingSourceCreditCard, FundingSourceOVOCash, FundingSourceOVOPayLater:
		return fs
	}
	return FundingSourceUnknown
}

// ActionType describes what the customer must do to complete a payment.
type ActionType string

const (
	ActionNone     ActionType = "NONE"
	ActionRedirect ActionType = "REDIRECT"
	ActionDeeplink ActionType = "DEEPLINK"
	ActionQRString ActionType = "QR_STRING"
)

// CustomerAction is the provider-issued instruction for the customer, e.g. a
// QR payload or a redirect URL. It is only ever populated from a real
// provider response.
type CustomerAction struct {
	Type  ActionType
	Value string
}

// Payment is one attempt to settle an order through a provider.
type Payment struct {
	ID                string
	OrderID           string
	Provider          Provider
	Method            PaymentMethod
	FundingSource     FundingSource
	Status            PaymentStatus
	Amount            int64
	Currency          string
	MerchantReference string // reference we send to the provider; unique per payment
	ProviderReference string // provider's transaction id; empty until the provider assigns one
	IdempotencyKey    string
	Action            CustomerAction
	ExpiresAt         *time.Time
	PaidAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NewPaymentInput holds the client-supplied fields for creating a payment.
// Amount and currency always come from the order, never from the client.
type NewPaymentInput struct {
	Provider       Provider
	Method         PaymentMethod
	IdempotencyKey string
}

var idempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x7E]{8,128}$`)

// Validate checks the input against the payment creation rules.
func (in NewPaymentInput) Validate() error {
	errs := fieldErrors{}
	if !in.Provider.Valid() {
		errs.add("provider", "required; lowercase provider code")
	}
	if !in.Method.Valid() {
		errs.add("payment_method", fmt.Sprintf("must be one of %s, %s, %s",
			PaymentMethodQRIS, PaymentMethodEWallet, PaymentMethodPayLater))
	}
	if !idempotencyKeyPattern.MatchString(in.IdempotencyKey) {
		errs.add("Idempotency-Key", "header required; 8-128 printable ASCII characters")
	}
	return errs.err()
}

// NewPayment builds a payment for order in CREATED status. It does not talk
// to any provider and therefore can never produce a PAID payment.
func NewPayment(id string, order Order, in NewPaymentInput, now time.Time) (Payment, error) {
	if err := in.Validate(); err != nil {
		return Payment{}, err
	}
	if order.Status == OrderStatusPaid {
		return Payment{}, ErrOrderAlreadyPaid
	}
	return Payment{
		ID:                id,
		OrderID:           order.ID,
		Provider:          in.Provider,
		Method:            in.Method,
		FundingSource:     FundingSourceUnknown,
		Status:            PaymentStatusCreated,
		Amount:            order.Amount,
		Currency:          order.Currency,
		MerchantReference: id,
		IdempotencyKey:    in.IdempotencyKey,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// StatusReport is a provider's statement about a payment, obtained from a
// verified webhook or a status inquiry. Adapters translate provider payloads
// into this shape; they must leave FundingSource empty/UNKNOWN unless the
// provider explicitly and verifiably reports it.
type StatusReport struct {
	Provider          Provider
	ProviderEventID   string // provider's unique notification id; empty for inquiry results
	ProviderReference string
	MerchantReference string
	Status            PaymentStatus
	Amount            *int64 // nil when the provider did not report an amount
	Currency          string // empty when the provider did not report a currency
	FundingSource     FundingSource
	// OccurredAt is the event time reported by the provider; zero when the
	// provider did not report one. It is never the local receive time.
	OccurredAt time.Time
	// Payload is the provider message for audit. Adapters must strip
	// credentials, signatures and unnecessary personal data before setting it.
	Payload []byte
}

// ApplyReport validates report against the payment and, if valid, applies the
// reported status. It returns whether the payment changed. A report that
// repeats the current status is a no-op, not an error.
func (p *Payment) ApplyReport(r StatusReport, now time.Time) (changed bool, err error) {
	if err := p.verifyReport(r); err != nil {
		return false, err
	}
	if r.Status == p.Status {
		return false, nil
	}
	if !CanTransition(p.Status, r.Status) {
		return false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, p.Status, r.Status)
	}

	p.Status = r.Status
	if p.ProviderReference == "" {
		p.ProviderReference = r.ProviderReference
	}
	if r.Status == PaymentStatusPaid {
		p.FundingSource = NormalizeFundingSource(r.FundingSource)
		p.PaidAt = &now
	}
	p.UpdatedAt = now
	return true, nil
}

func (p *Payment) verifyReport(r StatusReport) error {
	if !r.Status.ReportedByProvider() {
		return fmt.Errorf("%w: status %q", ErrInvalidProviderReport, r.Status)
	}
	if r.Provider != p.Provider {
		return fmt.Errorf("%w: provider", ErrConfirmationMismatch)
	}
	if r.MerchantReference != p.MerchantReference {
		return fmt.Errorf("%w: merchant reference", ErrConfirmationMismatch)
	}
	if r.ProviderReference == "" {
		return fmt.Errorf("%w: missing provider reference", ErrInvalidProviderReport)
	}
	if p.ProviderReference != "" && r.ProviderReference != p.ProviderReference {
		return fmt.Errorf("%w: provider reference", ErrConfirmationMismatch)
	}
	// Amount and currency are mandatory for success. For other statuses they
	// are checked when the provider includes them.
	if r.Status == PaymentStatusPaid && r.Amount == nil {
		return fmt.Errorf("%w: missing amount", ErrConfirmationMismatch)
	}
	if r.Amount != nil && *r.Amount != p.Amount {
		return fmt.Errorf("%w: amount", ErrConfirmationMismatch)
	}
	if r.Status == PaymentStatusPaid || r.Currency != "" {
		if r.Currency != p.Currency {
			return fmt.Errorf("%w: currency", ErrConfirmationMismatch)
		}
	}
	return nil
}
