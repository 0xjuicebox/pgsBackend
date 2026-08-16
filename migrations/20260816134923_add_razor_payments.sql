-- +goose Up
-- +goose StatementBegin

-- Razorpay payment links on invoices.
--
-- link_amount is stored separately from invoices.total_amount on purpose.
-- A link is created for a fixed rupee value; if the invoice is later
-- recalculated (a delivery log gets corrected), the link still charges the
-- old amount. Keeping both lets the webhook notice the mismatch instead of
-- silently marking a ₹4,200 invoice paid when ₹4,080 arrived.
ALTER TABLE invoices
    ADD COLUMN IF NOT EXISTS payment_link_id      TEXT,
    ADD COLUMN IF NOT EXISTS payment_link_url     TEXT,
    ADD COLUMN IF NOT EXISTS payment_link_amount  NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS payment_link_sent_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS paid_at              TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS payment_reference    TEXT;

-- Looking an invoice up from a webhook payload.
CREATE INDEX IF NOT EXISTS idx_invoices_payment_link
    ON invoices (payment_link_id)
    WHERE payment_link_id IS NOT NULL;

-- Webhook event log.
--
-- Two jobs. First, idempotency: Razorpay documents that the same event may be
-- delivered more than once, identified by the x-razorpay-event-id header, so
-- event_id is the primary key and a duplicate insert simply conflicts.
--
-- Second, an audit trail for money. Every webhook is recorded with its raw
-- payload and what we decided to do about it, including the ones we chose NOT
-- to act on — a payment landing against an already-paid invoice is the case
-- most worth being able to reconstruct later.
CREATE TABLE IF NOT EXISTS payment_events (
    event_id     TEXT PRIMARY KEY,
    event_type   TEXT NOT NULL,
    invoice_id   UUID REFERENCES invoices(id) ON DELETE SET NULL,
    payload      JSONB NOT NULL,
    outcome      TEXT NOT NULL
                 CHECK (outcome IN (
                     'APPLIED',          -- invoice marked paid
                     'ALREADY_PAID',     -- money arrived for a settled invoice — needs a human
                     'AMOUNT_MISMATCH',  -- paid, but not the amount we expected
                     'UNMATCHED',        -- couldn't tie it to an invoice
                     'IGNORED'           -- event type we don't act on
                 )),
    note         TEXT,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The admin review queue: anything that didn't cleanly apply.
CREATE INDEX IF NOT EXISTS idx_payment_events_needs_review
    ON payment_events (received_at DESC)
    WHERE outcome IN ('ALREADY_PAID', 'AMOUNT_MISMATCH', 'UNMATCHED');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS payment_events;
DROP INDEX IF EXISTS idx_invoices_payment_link;
ALTER TABLE invoices
    DROP COLUMN IF EXISTS payment_link_id,
    DROP COLUMN IF EXISTS payment_link_url,
    DROP COLUMN IF EXISTS payment_link_amount,
    DROP COLUMN IF EXISTS payment_link_sent_at,
    DROP COLUMN IF EXISTS paid_at,
    DROP COLUMN IF EXISTS payment_reference;
-- +goose StatementEnd
