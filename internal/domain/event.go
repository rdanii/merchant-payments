package domain

import "time"

// EventType classifies an audit record.
type EventType string

const (
	EventPaymentCreated         EventType = "PAYMENT_CREATED"
	EventProviderRequestOK      EventType = "PROVIDER_REQUEST_ACCEPTED"
	EventProviderRequestFailed  EventType = "PROVIDER_REQUEST_REJECTED"
	EventProviderRequestUnknown EventType = "PROVIDER_REQUEST_OUTCOME_UNKNOWN"
	EventProviderReport         EventType = "PROVIDER_STATUS_REPORT"
	EventPaymentExpired         EventType = "PAYMENT_EXPIRED_LOCALLY"
)

// EventSource is where an audit record originated.
type EventSource string

const (
	SourceAPI     EventSource = "API"
	SourceWebhook EventSource = "WEBHOOK"
	SourceInquiry EventSource = "INQUIRY"
	SourceSystem  EventSource = "SYSTEM"
)

// EventOutcome is what the system did with the event.
type EventOutcome string

const (
	OutcomeApplied  EventOutcome = "APPLIED"   // state changed
	OutcomeNoChange EventOutcome = "NO_CHANGE" // valid but already reflected
	OutcomeRejected EventOutcome = "REJECTED"  // failed validation; kept for reconciliation
)

// PaymentEvent is an append-only audit and reconciliation record. Fields
// prefixed Reported hold what the provider claimed, which may differ from
// the payment when the event was rejected.
type PaymentEvent struct {
	ID                    string
	PaymentID             string // empty when a report matched no payment
	Type                  EventType
	Source                EventSource
	Outcome               EventOutcome
	Reason                string
	Provider              Provider
	ProviderEventID       string
	ProviderReference     string
	FromStatus            PaymentStatus
	ToStatus              PaymentStatus
	ReportedStatus        PaymentStatus
	ReportedAmount        *int64 // nil when the provider did not report an amount
	ReportedCurrency      string
	ReportedFundingSource FundingSource
	Payload               []byte     // sanitized provider payload (JSON) or nil
	ProviderOccurredAt    *time.Time // provider-reported event time, if any
	CreatedAt             time.Time  // local processing time
}
