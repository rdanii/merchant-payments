package httpapi

import (
	"time"

	"merchant-payments/internal/domain"
)

type orderResponse struct {
	ID                string     `json:"id"`
	MerchantReference string     `json:"merchant_reference"`
	Amount            int64      `json:"amount"`
	Currency          string     `json:"currency"`
	Description       string     `json:"description"`
	Status            string     `json:"status"`
	PaidAt            *time.Time `json:"paid_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func toOrderResponse(o domain.Order) orderResponse {
	return orderResponse{
		ID:                o.ID,
		MerchantReference: o.MerchantReference,
		Amount:            o.Amount,
		Currency:          o.Currency,
		Description:       o.Description,
		Status:            string(o.Status),
		PaidAt:            utcPtr(o.PaidAt),
		CreatedAt:         o.CreatedAt.UTC(),
		UpdatedAt:         o.UpdatedAt.UTC(),
	}
}

type actionResponse struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type paymentResponse struct {
	ID                string          `json:"id"`
	OrderID           string          `json:"order_id"`
	Provider          string          `json:"provider"`
	PaymentMethod     string          `json:"payment_method"`
	FundingSource     string          `json:"funding_source"`
	Status            string          `json:"status"`
	Amount            int64           `json:"amount"`
	Currency          string          `json:"currency"`
	MerchantReference string          `json:"merchant_reference"`
	ProviderReference string          `json:"provider_reference,omitempty"`
	Action            *actionResponse `json:"action,omitempty"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	PaidAt            *time.Time      `json:"paid_at"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

func toPaymentResponse(p domain.Payment) paymentResponse {
	resp := paymentResponse{
		ID:                p.ID,
		OrderID:           p.OrderID,
		Provider:          string(p.Provider),
		PaymentMethod:     string(p.Method),
		FundingSource:     string(p.FundingSource),
		Status:            string(p.Status),
		Amount:            p.Amount,
		Currency:          p.Currency,
		MerchantReference: p.MerchantReference,
		ProviderReference: p.ProviderReference,
		ExpiresAt:         utcPtr(p.ExpiresAt),
		PaidAt:            utcPtr(p.PaidAt),
		CreatedAt:         p.CreatedAt.UTC(),
		UpdatedAt:         p.UpdatedAt.UTC(),
	}
	if p.Action.Type != "" && p.Action.Type != domain.ActionNone {
		resp.Action = &actionResponse{Type: string(p.Action.Type), Value: p.Action.Value}
	}
	return resp
}

// utcPtr renders timestamps consistently in UTC regardless of the database
// session time zone.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
