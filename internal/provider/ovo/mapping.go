package ovo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"merchant-payments/internal/domain"
)

// currencyIDR is the only currency in OVO's documented examples. IDR has no
// minor unit (ISO 4217 exponent 0), so internal amounts are whole rupiah.
const currencyIDR = "IDR"

// amountPattern matches OVO amount strings: "10000.00" (documented as
// String 16,2) and "3000" (as in the official Postman example).
var amountPattern = regexp.MustCompile(`^([0-9]{1,16})(?:\.([0-9]{1,2}))?$`)

// formatAmount renders whole rupiah as OVO's "10000.00".
func formatAmount(rupiah int64) (string, error) {
	if rupiah <= 0 {
		return "", fmt.Errorf("ovo: amount must be positive")
	}
	return strconv.FormatInt(rupiah, 10) + ".00", nil
}

// parseAmount converts an OVO IDR amount string to whole rupiah. A non-zero
// fractional part cannot be represented in rupiah and is rejected.
func parseAmount(s string) (int64, error) {
	m := amountPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("ovo: malformed amount %q", s)
	}
	if strings.Trim(m[2], "0") != "" {
		return 0, fmt.Errorf("ovo: amount %q has a fractional rupiah part", s)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ovo: malformed amount %q", s)
	}
	return v, nil
}

// errStatusUnsupported marks OVO statuses this engine does not model.
var errStatusUnsupported = errors.New("ovo: transaction status not supported")

// mapStatus maps OVO latestTransactionStatus to a payment status. Refund
// statuses (01 Refund Pending, 04 Refund Success) and any undocumented code
// are not mapped: refunds are out of scope and nothing is guessed.
func mapStatus(code string) (domain.PaymentStatus, error) {
	switch code {
	case "00":
		return domain.PaymentStatusPaid, nil
	case "03":
		return domain.PaymentStatusPending, nil
	case "06":
		return domain.PaymentStatusFailed, nil
	case "01", "04":
		return "", fmt.Errorf("%w: refund status %q", errStatusUnsupported, code)
	}
	return "", fmt.Errorf("%w: %q", errStatusUnsupported, code)
}

// transactionSummary is OVO's source-of-funds breakdown. Values are kept raw
// so that a malformed value only affects the funding source.
type transactionSummary struct {
	Points   json.RawMessage `json:"points,omitempty"`
	Cash     json.RawMessage `json:"cash,omitempty"`
	PayLater json.RawMessage `json:"payLater,omitempty"`
}

// effectiveSummary picks the transactionSummary to use. OVO's field table
// places it at the top level while its examples place it inside
// additionalInfo; either is accepted, and if both are present they must be
// identical.
func effectiveSummary(top, nested *transactionSummary) *transactionSummary {
	switch {
	case top == nil:
		return nested
	case nested == nil:
		return top
	case bytes.Equal(top.Points, nested.Points) && bytes.Equal(top.Cash, nested.Cash) &&
		bytes.Equal(top.PayLater, nested.PayLater):
		return top
	}
	return nil // conflicting evidence
}

// fundingSource derives the funding source from a transactionSummary only.
// All three components must be present, well formed, and add up to the
// reported amount; then exactly one non-zero cash or payLater component
// identifies OVO_CASH or OVO_PAYLATER. Everything else (points-only, mixed,
// zero, missing, malformed, not reconciling) is UNKNOWN.
func fundingSource(s *transactionSummary, amount *int64) domain.FundingSource {
	if s == nil || amount == nil {
		return domain.FundingSourceUnknown
	}
	cash, ok1 := summaryValue(s.Cash)
	points, ok2 := summaryValue(s.Points)
	payLater, ok3 := summaryValue(s.PayLater)
	if !ok1 || !ok2 || !ok3 || cash+points+payLater != *amount {
		return domain.FundingSourceUnknown
	}
	switch {
	case payLater > 0 && cash == 0 && points == 0:
		return domain.FundingSourceOVOPayLater
	case cash > 0 && payLater == 0 && points == 0:
		return domain.FundingSourceOVOCash
	}
	return domain.FundingSourceUnknown
}

// summaryValue parses one component, documented as a string such as "20000".
func summaryValue(raw json.RawMessage) (int64, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	v, err := parseAmount(s)
	return v, err == nil
}

// paidTimeLayouts are paidTime formats that carry a UTC offset, as in the
// official Postman example "2023-05-02T10:37:53.371+0000".
var paidTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
}

// parsePaidTime returns the provider event time, or the zero time when it
// cannot be placed on the timeline. OVO also documents zone-less forms
// ("yyyyMMddHHmmss", "2023-05-09T18:02:27"); their time zone is not
// documented, so they are not converted rather than guessed. The raw value is
// kept in the audit payload either way.
func parsePaidTime(s string) time.Time {
	for _, layout := range paidTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// auditPayload is the sanitized inquiry response stored for audit and
// reconciliation. Customer data in the response (userName, phoneNumber,
// customerPan, merchantPan, deviceId) is deliberately left out.
type auditPayload struct {
	ResponseCode                     string              `json:"responseCode,omitempty"`
	OriginalReferenceNo              string              `json:"originalReferenceNo,omitempty"`
	OriginalPartnerReferenceNo       string              `json:"originalPartnerReferenceNo,omitempty"`
	LatestTransactionStatus          string              `json:"latestTransactionStatus,omitempty"`
	TransactionStatusDesc            string              `json:"transactionStatusDesc,omitempty"`
	PaidTime                         string              `json:"paidTime,omitempty"`
	Amount                           *amount             `json:"amount,omitempty"`
	TransactionSummary               *transactionSummary `json:"transactionSummary,omitempty"`
	AdditionalInfoTransactionSummary *transactionSummary `json:"additionalInfoTransactionSummary,omitempty"`
	TransactionID                    json.RawMessage     `json:"transactionId,omitempty"`
	BatchNo                          json.RawMessage     `json:"batchNo,omitempty"`
}
