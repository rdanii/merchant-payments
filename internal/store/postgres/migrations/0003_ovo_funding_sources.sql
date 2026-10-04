-- Allow the OVO funding sources. Existing values are kept unchanged.
ALTER TABLE payments DROP CONSTRAINT payments_funding_source_check;
ALTER TABLE payments ADD CONSTRAINT payments_funding_source_check CHECK (funding_source IN
    ('UNKNOWN', 'EWALLET_BALANCE', 'PAYLATER', 'BANK_ACCOUNT', 'DEBIT_CARD', 'CREDIT_CARD',
     'OVO_CASH', 'OVO_PAYLATER'));
