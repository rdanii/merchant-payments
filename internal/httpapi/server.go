// Package httpapi exposes the payment engine over a JSON REST API.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/id"
	"merchant-payments/internal/payment"
	"merchant-payments/internal/provider"
	"merchant-payments/internal/requestid"
)

const (
	maxJSONBody    = 64 << 10 // 64 KiB
	maxWebhookBody = 1 << 20  // 1 MiB
)

// Server routes HTTP requests to the payment service.
type Server struct {
	svc   *payment.Service
	ready func(context.Context) error
	log   *slog.Logger
	mux   *http.ServeMux
}

// New builds the HTTP handler. ready is used by /readyz (e.g. a DB ping).
func New(svc *payment.Service, ready func(context.Context) error, log *slog.Logger) *Server {
	s := &Server{svc: svc, ready: ready, log: log, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)

	s.mux.HandleFunc("POST /api/v1/orders", s.createOrder)
	s.mux.HandleFunc("GET /api/v1/orders/{id}", s.getOrder)
	s.mux.HandleFunc("POST /api/v1/orders/{id}/payments", s.createPayment)
	s.mux.HandleFunc("GET /api/v1/payments/{id}", s.getPayment)
	s.mux.HandleFunc("POST /api/v1/payments/{id}/inquiry", s.inquirePayment)
	s.mux.HandleFunc("POST /api/v1/webhooks/{provider}", s.webhook)
	return s
}

// ServeHTTP applies middleware and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := id.New()
	w.Header().Set("X-Request-ID", reqID)
	// Loggers built on requestid.Handler add request_id to every record
	// logged with this context, including service-level logs.
	r = r.WithContext(requestid.With(r.Context(), reqID))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		if v := recover(); v != nil {
			s.log.ErrorContext(r.Context(), "panic", "panic", v)
			writeJSON(rec, http.StatusInternalServerError, errorBody{apiError{Code: "INTERNAL", Message: "internal server error"}})
		}
		// Only method, path and status are logged: never bodies or headers,
		// which can carry signatures, tokens or customer data.
		s.log.InfoContext(r.Context(), "http request", "method", r.Method,
			"path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	}()
	s.mux.ServeHTTP(rec, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		s.log.WarnContext(ctx, "readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type createOrderRequest struct {
	MerchantReference string `json:"merchant_reference"`
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	Description       string `json:"description"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	o, created, err := s.svc.CreateOrder(r.Context(), domain.NewOrderInput{
		MerchantReference: req.MerchantReference,
		Amount:            req.Amount,
		Currency:          req.Currency,
		Description:       req.Description,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // identical retry of an existing order
	}
	writeJSON(w, status, toOrderResponse(o))
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		s.writeError(w, r, domain.ErrNotFound)
		return
	}
	o, err := s.svc.GetOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOrderResponse(o))
}

type createPaymentRequest struct {
	Provider      string `json:"provider"`
	PaymentMethod string `json:"payment_method"`
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request) {
	orderID, ok := pathID(r)
	if !ok {
		s.writeError(w, r, domain.ErrNotFound)
		return
	}
	var req createPaymentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, created, err := s.svc.CreatePayment(r.Context(), orderID, domain.NewPaymentInput{
		Provider:       domain.Provider(req.Provider),
		Method:         domain.PaymentMethod(req.PaymentMethod),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		writeJSON(w, http.StatusBadGateway, errorBody{apiError{
			Code:      "PROVIDER_OUTCOME_UNKNOWN",
			Message:   "payment recorded as CREATED but the provider outcome is unknown; it changes only on a provider notification or a successful POST /api/v1/payments/{id}/inquiry",
			PaymentID: p.ID,
		}})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // idempotent replay
	}
	writeJSON(w, status, toPaymentResponse(p))
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request) {
	paymentID, ok := pathID(r)
	if !ok {
		s.writeError(w, r, domain.ErrNotFound)
		return
	}
	p, err := s.svc.GetPayment(r.Context(), paymentID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(p))
}

// inquirePayment asks the provider for the payment's status and applies it
// through the same validation as webhooks. It never invents an outcome: if
// the provider cannot answer, the payment is left unchanged.
func (s *Server) inquirePayment(w http.ResponseWriter, r *http.Request) {
	paymentID, ok := pathID(r)
	if !ok {
		s.writeError(w, r, domain.ErrNotFound)
		return
	}
	res, err := s.svc.InquirePayment(r.Context(), paymentID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(res.Payment))
}

type webhookResponse struct {
	Status    string `json:"status"` // "processed" or "duplicate"
	Outcome   string `json:"outcome,omitempty"`
	PaymentID string `json:"payment_id,omitempty"`
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	prov := domain.Provider(r.PathValue("provider"))
	if !prov.Valid() {
		s.writeError(w, r, domain.ErrNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.writeError(w, r, bodyErr(err))
		return
	}
	res, err := s.svc.HandleWebhook(r.Context(), prov, provider.Webhook{Headers: r.Header.Clone(), Body: body})
	if errors.Is(err, domain.ErrUnsupportedProvider) {
		err = domain.ErrNotFound
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if res.Duplicate {
		writeJSON(w, http.StatusOK, webhookResponse{Status: "duplicate"})
		return
	}
	writeJSON(w, http.StatusOK, webhookResponse{Status: "processed", Outcome: string(res.Outcome), PaymentID: res.Payment.ID})
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func pathID(r *http.Request) (string, bool) {
	v := r.PathValue("id")
	return v, uuidPattern.MatchString(v)
}

// decodeJSON strictly decodes a single JSON object, rejecting unknown fields
// and trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		return bodyErr(err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errInvalidJSON
	}
	if dec.More() {
		return errInvalidJSON
	}
	return nil
}

func bodyErr(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errBodyTooLarge
	}
	return errInvalidJSON
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
