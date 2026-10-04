package domain

import (
	"regexp"
	"time"
)

// OrderStatus is the lifecycle state of a merchant order.
type OrderStatus string

const (
	OrderStatusCreated OrderStatus = "CREATED"
	OrderStatusPaid    OrderStatus = "PAID"
)

// Order is a merchant order that one payment eventually settles.
//
// Amount is expressed in the minor unit of Currency as defined by ISO 4217
// (for IDR, whose exponent is 0, that is whole rupiah).
type Order struct {
	ID                string
	MerchantReference string
	Amount            int64
	Currency          string
	Description       string
	Status            OrderStatus
	PaidAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NewOrderInput holds the client-supplied fields for creating an order.
type NewOrderInput struct {
	MerchantReference string
	Amount            int64
	Currency          string
	Description       string
}

var (
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
)

const maxDescriptionLen = 255

// Validate checks the input against the order creation rules.
func (in NewOrderInput) Validate() error {
	errs := fieldErrors{}
	if !referencePattern.MatchString(in.MerchantReference) {
		errs.add("merchant_reference", "required; 1-64 characters of A-Z a-z 0-9 . _ : -")
	}
	if in.Amount <= 0 {
		errs.add("amount", "must be a positive integer in the currency's minor unit")
	}
	if !currencyPattern.MatchString(in.Currency) {
		errs.add("currency", "must be a 3-letter uppercase ISO 4217 code")
	}
	if len(in.Description) > maxDescriptionLen {
		errs.add("description", "must be at most 255 bytes")
	}
	return errs.err()
}

// NewOrder validates the input and builds an order in CREATED status.
func NewOrder(id string, in NewOrderInput, now time.Time) (Order, error) {
	if err := in.Validate(); err != nil {
		return Order{}, err
	}
	return Order{
		ID:                id,
		MerchantReference: in.MerchantReference,
		Amount:            in.Amount,
		Currency:          in.Currency,
		Description:       in.Description,
		Status:            OrderStatusCreated,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// MarkPaid moves the order to PAID. It is only valid from CREATED.
func (o *Order) MarkPaid(at time.Time) error {
	if o.Status == OrderStatusPaid {
		return ErrOrderAlreadyPaid
	}
	o.Status = OrderStatusPaid
	o.PaidAt = &at
	o.UpdatedAt = at
	return nil
}
