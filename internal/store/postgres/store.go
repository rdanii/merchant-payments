// Package postgres implements payment.Store on PostgreSQL. Uniqueness and
// idempotency rules are enforced by the schema; this package translates the
// resulting constraint violations into domain errors.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/payment"
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a payment.Store backed by a connection pool.
type Store struct {
	repo
	pool *pgxpool.Pool
}

var _ payment.Store = (*Store)(nil)

// New returns a Store using pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{repo: repo{q: pool}, pool: pool}
}

// InTx runs fn in a READ COMMITTED transaction. Rows read with forUpdate are
// locked until commit, which is what serializes concurrent updates.
func (s *Store) InTx(ctx context.Context, fn func(payment.Repository) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(repo{q: tx})
	})
}

// repo implements payment.Repository on either the pool or a transaction.
type repo struct {
	q querier
}

const orderColumns = `id, merchant_reference, amount, currency, description, status, paid_at, created_at, updated_at`

func (r repo) CreateOrder(ctx context.Context, o domain.Order) error {
	_, err := r.q.Exec(ctx, `INSERT INTO orders (`+orderColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		o.ID, o.MerchantReference, o.Amount, o.Currency, o.Description, o.Status, o.PaidAt, o.CreatedAt, o.UpdatedAt)
	if constraint(err) == "orders_merchant_reference_key" {
		return domain.ErrDuplicateOrder
	}
	return err
}

func (r repo) GetOrder(ctx context.Context, id string, forUpdate bool) (domain.Order, error) {
	sql := `SELECT ` + orderColumns + ` FROM orders WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var o domain.Order
	err := r.q.QueryRow(ctx, sql, id).Scan(&o.ID, &o.MerchantReference, &o.Amount, &o.Currency,
		&o.Description, &o.Status, &o.PaidAt, &o.CreatedAt, &o.UpdatedAt)
	return o, notFound(err)
}

func (r repo) GetOrderByMerchantReference(ctx context.Context, ref string) (domain.Order, error) {
	var o domain.Order
	err := r.q.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE merchant_reference = $1`, ref).Scan(
		&o.ID, &o.MerchantReference, &o.Amount, &o.Currency, &o.Description, &o.Status, &o.PaidAt, &o.CreatedAt, &o.UpdatedAt)
	return o, notFound(err)
}

func (r repo) UpdateOrder(ctx context.Context, o domain.Order) error {
	tag, err := r.q.Exec(ctx, `UPDATE orders SET status = $2, paid_at = $3, updated_at = $4 WHERE id = $1`,
		o.ID, o.Status, o.PaidAt, o.UpdatedAt)
	return affectedOne(tag, err)
}

const paymentColumns = `id, order_id, provider, payment_method, funding_source, status, amount, currency,
	merchant_reference, provider_reference, idempotency_key, action_type, action_value,
	expires_at, paid_at, created_at, updated_at`

func (r repo) CreatePayment(ctx context.Context, p domain.Payment) error {
	_, err := r.q.Exec(ctx, `INSERT INTO payments (`+paymentColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		p.ID, p.OrderID, p.Provider, p.Method, p.FundingSource, p.Status, p.Amount, p.Currency,
		p.MerchantReference, nullable(p.ProviderReference), p.IdempotencyKey, actionType(p.Action), p.Action.Value,
		p.ExpiresAt, p.PaidAt, p.CreatedAt, p.UpdatedAt)
	switch constraint(err) {
	case "payments_one_active_per_order":
		return domain.ErrActivePaymentExists
	case "payments_one_paid_per_order":
		return domain.ErrOrderAlreadyPaid
	case "payments_order_idempotency_key":
		return domain.ErrDuplicateIdempotencyKey
	}
	return err
}

func (r repo) GetPayment(ctx context.Context, id string, forUpdate bool) (domain.Payment, error) {
	sql := `SELECT ` + paymentColumns + ` FROM payments WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	return scanPayment(r.q.QueryRow(ctx, sql, id))
}

func (r repo) GetPaymentByMerchantReference(ctx context.Context, prov domain.Provider, ref string) (domain.Payment, error) {
	return scanPayment(r.q.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE provider = $1 AND merchant_reference = $2`, prov, ref))
}

func (r repo) GetPaymentByIdempotencyKey(ctx context.Context, orderID, key string) (domain.Payment, error) {
	return scanPayment(r.q.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE order_id = $1 AND idempotency_key = $2`, orderID, key))
}

func (r repo) UpdatePayment(ctx context.Context, p domain.Payment) error {
	tag, err := r.q.Exec(ctx, `UPDATE payments SET
			status = $2, funding_source = $3, provider_reference = $4, action_type = $5,
			action_value = $6, expires_at = $7, paid_at = $8, updated_at = $9
		WHERE id = $1`,
		p.ID, p.Status, p.FundingSource, nullable(p.ProviderReference), actionType(p.Action),
		p.Action.Value, p.ExpiresAt, p.PaidAt, p.UpdatedAt)
	switch constraint(err) {
	case "payments_one_paid_per_order":
		return domain.ErrOrderAlreadyPaid
	case "payments_provider_reference_key":
		return fmt.Errorf("%w: provider reference already used by another payment", domain.ErrConfirmationMismatch)
	}
	return affectedOne(tag, err)
}

func (r repo) ListOverduePayments(ctx context.Context, cutoff time.Time, after payment.OverdueCursor, limit int) ([]domain.Payment, error) {
	sql := `SELECT ` + paymentColumns + ` FROM payments
		WHERE status IN ('CREATED', 'PENDING') AND expires_at < $1`
	args := []any{cutoff, limit}
	if after.ID != "" {
		sql += ` AND (expires_at, id) > ($3, $4)`
		args = append(args, after.ExpiresAt, after.ID)
	}
	rows, err := r.q.Query(ctx, sql+` ORDER BY expires_at, id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r repo) InsertEvent(ctx context.Context, e domain.PaymentEvent) error {
	_, err := r.q.Exec(ctx, `INSERT INTO payment_events (
			id, payment_id, event_type, source, outcome, reason, provider, provider_event_id,
			provider_reference, from_status, to_status, reported_status, reported_amount,
			reported_currency, reported_funding_source, payload, provider_occurred_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		e.ID, nullable(e.PaymentID), e.Type, e.Source, e.Outcome, e.Reason, e.Provider,
		nullable(e.ProviderEventID), nullable(e.ProviderReference), nullable(string(e.FromStatus)),
		nullable(string(e.ToStatus)), nullable(string(e.ReportedStatus)), e.ReportedAmount,
		nullable(e.ReportedCurrency), nullable(string(e.ReportedFundingSource)), jsonPayload(e.Payload), e.ProviderOccurredAt, e.CreatedAt)
	if constraint(err) == "payment_events_provider_event_key" {
		return domain.ErrDuplicateEvent
	}
	return err
}

func (r repo) ProviderEventExists(ctx context.Context, prov domain.Provider, eventID string) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM payment_events WHERE provider = $1 AND provider_event_id = $2)`, prov, eventID).Scan(&exists)
	return exists, err
}

func scanPayment(row pgx.Row) (domain.Payment, error) {
	var p domain.Payment
	var providerRef *string
	err := row.Scan(&p.ID, &p.OrderID, &p.Provider, &p.Method, &p.FundingSource, &p.Status, &p.Amount,
		&p.Currency, &p.MerchantReference, &providerRef, &p.IdempotencyKey, &p.Action.Type, &p.Action.Value,
		&p.ExpiresAt, &p.PaidAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return domain.Payment{}, notFound(err)
	}
	if providerRef != nil {
		p.ProviderReference = *providerRef
	}
	return p, nil
}

// constraint returns the name of the violated unique constraint, if any.
func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func affectedOne(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func actionType(a domain.CustomerAction) domain.ActionType {
	if a.Type == "" {
		return domain.ActionNone
	}
	return a.Type
}

// jsonPayload returns p as a jsonb value, wrapping non-JSON payloads in a
// JSON string so nothing is silently dropped.
func jsonPayload(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	if json.Valid(p) {
		return string(p)
	}
	b, _ := json.Marshal(string(p))
	return string(b)
}
