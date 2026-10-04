// Package paymenttest provides an in-memory payment.Store for tests. It
// emulates transaction rollback and the schema's uniqueness rules; it is not
// used by the server. The PostgreSQL store has its own integration tests.
package paymenttest

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/payment"
)

// MemStore is an in-memory payment.Store. Transactions are serialized.
type MemStore struct {
	mu sync.Mutex
	st state

	// FailUpdateOrder, when set, is returned by UpdateOrder to simulate a
	// storage failure in the middle of a transaction.
	FailUpdateOrder error
	// FailUpdatePayment, when set, is returned by UpdatePayment.
	FailUpdatePayment error
}

var _ payment.Store = (*MemStore)(nil)

type state struct {
	orders   map[string]domain.Order
	payments map[string]domain.Payment
	events   []domain.PaymentEvent
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{st: state{orders: map[string]domain.Order{}, payments: map[string]domain.Payment{}}}
}

func (s state) clone() state {
	return state{orders: maps.Clone(s.orders), payments: maps.Clone(s.payments), events: slices.Clone(s.events)}
}

// Events returns a copy of all recorded events.
func (m *MemStore) Events() []domain.PaymentEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.st.events)
}

// InTx runs fn on a copy of the state and keeps the copy only if fn succeeds.
func (m *MemStore) InTx(_ context.Context, fn func(payment.Repository) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	work := m.st.clone()
	if err := fn(&repo{st: &work, m: m}); err != nil {
		return err
	}
	m.st = work
	return nil
}

func (m *MemStore) do(fn func(*repo) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(&repo{st: &m.st, m: m})
}

func (m *MemStore) CreateOrder(ctx context.Context, o domain.Order) error {
	return m.do(func(r *repo) error { return r.CreateOrder(ctx, o) })
}

func (m *MemStore) GetOrder(ctx context.Context, id string, forUpdate bool) (o domain.Order, err error) {
	err = m.do(func(r *repo) error { o, err = r.GetOrder(ctx, id, forUpdate); return err })
	return o, err
}

func (m *MemStore) GetOrderByMerchantReference(ctx context.Context, ref string) (o domain.Order, err error) {
	err = m.do(func(r *repo) error { o, err = r.GetOrderByMerchantReference(ctx, ref); return err })
	return o, err
}

func (m *MemStore) ListOverduePayments(ctx context.Context, cutoff time.Time, after payment.OverdueCursor, limit int) (ps []domain.Payment, err error) {
	err = m.do(func(r *repo) error { ps, err = r.ListOverduePayments(ctx, cutoff, after, limit); return err })
	return ps, err
}

func (m *MemStore) UpdateOrder(ctx context.Context, o domain.Order) error {
	return m.do(func(r *repo) error { return r.UpdateOrder(ctx, o) })
}

func (m *MemStore) CreatePayment(ctx context.Context, p domain.Payment) error {
	return m.do(func(r *repo) error { return r.CreatePayment(ctx, p) })
}

func (m *MemStore) GetPayment(ctx context.Context, id string, forUpdate bool) (p domain.Payment, err error) {
	err = m.do(func(r *repo) error { p, err = r.GetPayment(ctx, id, forUpdate); return err })
	return p, err
}

func (m *MemStore) GetPaymentByMerchantReference(ctx context.Context, prov domain.Provider, ref string) (p domain.Payment, err error) {
	err = m.do(func(r *repo) error { p, err = r.GetPaymentByMerchantReference(ctx, prov, ref); return err })
	return p, err
}

func (m *MemStore) GetPaymentByIdempotencyKey(ctx context.Context, orderID, key string) (p domain.Payment, err error) {
	err = m.do(func(r *repo) error { p, err = r.GetPaymentByIdempotencyKey(ctx, orderID, key); return err })
	return p, err
}

func (m *MemStore) UpdatePayment(ctx context.Context, p domain.Payment) error {
	return m.do(func(r *repo) error { return r.UpdatePayment(ctx, p) })
}

func (m *MemStore) InsertEvent(ctx context.Context, e domain.PaymentEvent) error {
	return m.do(func(r *repo) error { return r.InsertEvent(ctx, e) })
}

func (m *MemStore) ProviderEventExists(ctx context.Context, prov domain.Provider, eventID string) (ok bool, err error) {
	err = m.do(func(r *repo) error { ok, err = r.ProviderEventExists(ctx, prov, eventID); return err })
	return ok, err
}

type repo struct {
	st *state
	m  *MemStore
}

func (r *repo) CreateOrder(_ context.Context, o domain.Order) error {
	for _, e := range r.st.orders {
		if e.MerchantReference == o.MerchantReference {
			return domain.ErrDuplicateOrder
		}
	}
	r.st.orders[o.ID] = o
	return nil
}

func (r *repo) GetOrder(_ context.Context, id string, _ bool) (domain.Order, error) {
	o, ok := r.st.orders[id]
	if !ok {
		return domain.Order{}, domain.ErrNotFound
	}
	return o, nil
}

func (r *repo) GetOrderByMerchantReference(_ context.Context, ref string) (domain.Order, error) {
	for _, o := range r.st.orders {
		if o.MerchantReference == ref {
			return o, nil
		}
	}
	return domain.Order{}, domain.ErrNotFound
}

func (r *repo) ListOverduePayments(_ context.Context, cutoff time.Time, after payment.OverdueCursor, limit int) ([]domain.Payment, error) {
	var due []domain.Payment
	for _, p := range r.st.payments {
		if !p.Status.Active() || p.ExpiresAt == nil || !p.ExpiresAt.Before(cutoff) {
			continue
		}
		if after.ID != "" && compareOverdue(*p.ExpiresAt, p.ID, after.ExpiresAt, after.ID) <= 0 {
			continue
		}
		due = append(due, p)
	}
	slices.SortFunc(due, func(a, b domain.Payment) int { return compareOverdue(*a.ExpiresAt, a.ID, *b.ExpiresAt, b.ID) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (r *repo) UpdateOrder(_ context.Context, o domain.Order) error {
	if r.m.FailUpdateOrder != nil {
		return r.m.FailUpdateOrder
	}
	if _, ok := r.st.orders[o.ID]; !ok {
		return domain.ErrNotFound
	}
	r.st.orders[o.ID] = o
	return nil
}

func (r *repo) CreatePayment(_ context.Context, p domain.Payment) error {
	for _, e := range r.st.payments {
		switch {
		case e.OrderID == p.OrderID && e.IdempotencyKey == p.IdempotencyKey:
			return domain.ErrDuplicateIdempotencyKey
		case e.OrderID == p.OrderID && e.Status.Active() && p.Status.Active():
			return domain.ErrActivePaymentExists
		}
	}
	r.st.payments[p.ID] = p
	return nil
}

func (r *repo) GetPayment(_ context.Context, id string, _ bool) (domain.Payment, error) {
	p, ok := r.st.payments[id]
	if !ok {
		return domain.Payment{}, domain.ErrNotFound
	}
	return p, nil
}

func (r *repo) GetPaymentByMerchantReference(_ context.Context, prov domain.Provider, ref string) (domain.Payment, error) {
	for _, p := range r.st.payments {
		if p.Provider == prov && p.MerchantReference == ref {
			return p, nil
		}
	}
	return domain.Payment{}, domain.ErrNotFound
}

func (r *repo) GetPaymentByIdempotencyKey(_ context.Context, orderID, key string) (domain.Payment, error) {
	for _, p := range r.st.payments {
		if p.OrderID == orderID && p.IdempotencyKey == key {
			return p, nil
		}
	}
	return domain.Payment{}, domain.ErrNotFound
}

func (r *repo) UpdatePayment(_ context.Context, p domain.Payment) error {
	if r.m.FailUpdatePayment != nil {
		return r.m.FailUpdatePayment
	}
	if _, ok := r.st.payments[p.ID]; !ok {
		return domain.ErrNotFound
	}
	if p.Status == domain.PaymentStatusPaid {
		for _, e := range r.st.payments {
			if e.ID != p.ID && e.OrderID == p.OrderID && e.Status == domain.PaymentStatusPaid {
				return domain.ErrOrderAlreadyPaid
			}
		}
	}
	r.st.payments[p.ID] = p
	return nil
}

func (r *repo) InsertEvent(_ context.Context, e domain.PaymentEvent) error {
	if e.ProviderEventID != "" {
		if ok, _ := r.ProviderEventExists(context.Background(), e.Provider, e.ProviderEventID); ok {
			return domain.ErrDuplicateEvent
		}
	}
	r.st.events = append(r.st.events, e)
	return nil
}

func (r *repo) ProviderEventExists(_ context.Context, prov domain.Provider, eventID string) (bool, error) {
	for _, e := range r.st.events {
		if e.Provider == prov && e.ProviderEventID == eventID {
			return true, nil
		}
	}
	return false, nil
}

// compareOverdue orders payments by (expires_at, id), like the SQL store.
func compareOverdue(at time.Time, id string, bt time.Time, bid string) int {
	if c := at.Compare(bt); c != 0 {
		return c
	}
	return strings.Compare(id, bid)
}
