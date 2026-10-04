-- Provider-reported event time, kept separate from local processing time
-- (created_at). NULL when the provider did not report one.
ALTER TABLE payment_events ADD COLUMN provider_occurred_at timestamptz;

-- Same rule as the application and orders.currency.
ALTER TABLE payments ADD CONSTRAINT payments_currency_format CHECK (currency ~ '^[A-Z]{3}$');

-- Supports the local expiry sweep over in-flight payments.
CREATE INDEX payments_active_expires_at_idx
    ON payments (expires_at) WHERE status IN ('CREATED', 'PENDING') AND expires_at IS NOT NULL;
