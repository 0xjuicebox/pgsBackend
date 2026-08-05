-- +goose Up

-- The Invoices Table
CREATE TABLE invoices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID REFERENCES customers(id) ON DELETE CASCADE NOT NULL,
    billing_month TEXT NOT NULL, -- Format: 'YYYY-MM' (e.g., '2026-07')
    total_amount NUMERIC NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING', -- 'PENDING', 'PAID_ONLINE', 'PAID_CASH'

    -- The immutable snapshot of the quantities and prices at the time of generation
    breakdown JSONB NOT NULL,

    created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,

    -- A customer can only have one official invoice per month
    CONSTRAINT unique_monthly_invoice UNIQUE(customer_id, billing_month)
);

-- +goose Down
DROP TABLE IF EXISTS invoices;
