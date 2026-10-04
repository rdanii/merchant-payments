-- Orders, payments and the append-only payment audit trail.
-- Amounts are integers in the ISO 4217 minor unit of the currency.

CREATE TABLE orders (
    id                 uuid        PRIMARY KEY,
    merchant_reference text        NOT NULL,
    amount             bigint      NOT NULL CHECK (amount > 0),
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    description        text        NOT NULL DEFAULT '',
    status             text        NOT NULL CHECK (status IN ('CREATED', 'PAID')),
    paid_at            timestamptz,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    CONSTRAINT orders_merchant_reference_key UNIQUE (merchant_reference),
    CONSTRAINT orders_paid_at_matches_status CHECK ((status = 'PAID') = (paid_at IS NOT NULL))
);

CREATE TABLE payments (
    id                 uuid        PRIMARY KEY,
    order_id           uuid        NOT NULL REFERENCES orders (id),
    provider           text        NOT NULL,
    payment_method     text        NOT NULL CHECK (payment_method IN ('QRIS', 'EWALLET', 'PAYLATER')),
    funding_source     text        NOT NULL DEFAULT 'UNKNOWN' CHECK (funding_source IN
                           ('UNKNOWN', 'EWALLET_BALANCE', 'PAYLATER', 'BANK_ACCOUNT', 'DEBIT_CARD', 'CREDIT_CARD')),
    status             text        NOT NULL CHECK (status IN ('CREATED', 'PENDING', 'PAID', 'FAILED', 'EXPIRED')),
    amount             bigint      NOT NULL CHECK (amount > 0),
    currency           char(3)     NOT NULL,
    merchant_reference text        NOT NULL,
    provider_reference text,
    idempotency_key    text        NOT NULL,
    action_type        text        NOT NULL DEFAULT 'NONE',
    action_value       text        NOT NULL DEFAULT '',
    expires_at         timestamptz,
    paid_at            timestamptz,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    CONSTRAINT payments_merchant_reference_key UNIQUE (merchant_reference),
    CONSTRAINT payments_order_idempotency_key UNIQUE (order_id, idempotency_key),
    CONSTRAINT payments_paid_at_matches_status CHECK ((status = 'PAID') = (paid_at IS NOT NULL))
);

CREATE UNIQUE INDEX payments_provider_reference_key
    ON payments (provider, provider_reference) WHERE provider_reference IS NOT NULL;
-- At most one payment per order may be in flight, and at most one may succeed.
CREATE UNIQUE INDEX payments_one_active_per_order
    ON payments (order_id) WHERE status IN ('CREATED', 'PENDING');
CREATE UNIQUE INDEX payments_one_paid_per_order
    ON payments (order_id) WHERE status = 'PAID';

CREATE TABLE payment_events (
    id                      uuid        PRIMARY KEY,
    payment_id              uuid        REFERENCES payments (id), -- NULL when a report matched no payment
    event_type              text        NOT NULL,
    source                  text        NOT NULL,
    outcome                 text        NOT NULL CHECK (outcome IN ('APPLIED', 'NO_CHANGE', 'REJECTED')),
    reason                  text        NOT NULL DEFAULT '',
    provider                text        NOT NULL,
    provider_event_id       text,
    provider_reference      text,
    from_status             text,
    to_status               text,
    reported_status         text,
    reported_amount         bigint,
    reported_currency       text,
    reported_funding_source text,
    payload                 jsonb,       -- sanitized by the provider adapter
    created_at              timestamptz NOT NULL
);

-- Webhook idempotency: each provider event is processed once.
CREATE UNIQUE INDEX payment_events_provider_event_key
    ON payment_events (provider, provider_event_id) WHERE provider_event_id IS NOT NULL;
CREATE INDEX payment_events_payment_id_idx ON payment_events (payment_id, created_at);

CREATE FUNCTION payment_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment_events is append-only';
END;
$$;

CREATE TRIGGER payment_events_append_only
    BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION payment_events_append_only();
