package httpapi

import (
	"errors"
	"net/http"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/provider"
)

// errorBody is the JSON error envelope: {"error": {...}}.
type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	PaymentID string            `json:"payment_id,omitempty"`
}

var (
	errInvalidJSON  = errors.New("request body must be a single valid JSON object with known fields")
	errBodyTooLarge = errors.New("request body too large")
)

// mapping from domain/provider errors to HTTP responses. Messages come from
// the sentinel errors, never from wrapped provider output.
var errorMap = []struct {
	target error
	status int
	code   string
}{
	{domain.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
	{domain.ErrDuplicateOrder, http.StatusConflict, "DUPLICATE_ORDER"},
	{domain.ErrOrderAlreadyPaid, http.StatusConflict, "ORDER_ALREADY_PAID"},
	{domain.ErrActivePaymentExists, http.StatusConflict, "ACTIVE_PAYMENT_EXISTS"},
	{domain.ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
	{domain.ErrDuplicateIdempotencyKey, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
	{domain.ErrUnsupportedProvider, http.StatusUnprocessableEntity, "UNSUPPORTED_PROVIDER"},
	{domain.ErrUnsupportedMethod, http.StatusUnprocessableEntity, "UNSUPPORTED_PAYMENT_METHOD"},
	{domain.ErrProviderOutcomeUnknown, http.StatusBadGateway, "PROVIDER_OUTCOME_UNKNOWN"},
	{domain.ErrConfirmationMismatch, http.StatusUnprocessableEntity, "PROVIDER_REPORT_REJECTED"},
	{domain.ErrInvalidProviderReport, http.StatusUnprocessableEntity, "PROVIDER_REPORT_REJECTED"},
	{domain.ErrInvalidTransition, http.StatusUnprocessableEntity, "PROVIDER_REPORT_REJECTED"},
	{provider.ErrInquiryUnsupported, http.StatusNotImplemented, "INQUIRY_UNSUPPORTED"},
	{provider.ErrInvalidSignature, http.StatusUnauthorized, "INVALID_SIGNATURE"},
	{provider.ErrMalformedWebhook, http.StatusBadRequest, "MALFORMED_WEBHOOK"},
	{errInvalidJSON, http.StatusBadRequest, "INVALID_JSON"},
	{errBodyTooLarge, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE"},
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, errorBody{apiError{
			Code: "VALIDATION_FAILED", Message: "request validation failed", Fields: ve.Fields,
		}})
		return
	}
	for _, m := range errorMap {
		if errors.Is(err, m.target) {
			if m.status >= 500 || m.status == http.StatusUnprocessableEntity {
				s.log.WarnContext(r.Context(), "request failed", "path", r.URL.Path, "code", m.code, "error", err)
			}
			writeJSON(w, m.status, errorBody{apiError{Code: m.code, Message: m.target.Error()}})
			return
		}
	}
	s.log.ErrorContext(r.Context(), "internal error", "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody{apiError{
		Code: "INTERNAL", Message: "internal server error",
	}})
}
