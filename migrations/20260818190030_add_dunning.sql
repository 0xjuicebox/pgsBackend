-- +goose Up
-- +goose StatementBegin

-- Dunning: automated chase for unpaid bills.
--
-- WHY 'suspended' IS DISTINCT FROM 'disabled'
--
-- Both mean "not receiving deliveries", but they must not be lifted the same
-- way. 'disabled' is the customer's own choice — a holiday pause — and typing
-- RESUME should bring them back. 'suspended' is us withholding service over an
-- unpaid bill, and RESUME must NOT lift it, or the suspension has no teeth:
-- the customer would be paused on the 6th, type RESUME on the 7th, and carry
-- on receiving milk without paying.
--
-- Collapsing them into one status was the tempting shortcut. It would have
-- meant either non-payers self-resuming, or holidaying customers being told to
-- pay a bill they don't owe.
--
-- Suspension deliberately does NOT touch subscriptions.route_id or stop_order.
-- FinalInvoice zeroes stop_order because a leaver is gone for good; a
-- suspension is expected to be temporary, so routing is preserved and
-- resuming is a single flag flip rather than a re-routing exercise.

ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_status_check;
ALTER TABLE customers ADD CONSTRAINT customers_status_check
    CHECK (status IN ('pending', 'active', 'disabled', 'rejected', 'suspended'));

-- Per-invoice dunning state.
--
-- last_reminder_on is a DATE, not a counter, and it is what makes the sweeper
-- idempotent. The sweeper may run many times a day — every hour, plus once at
-- every deploy or restart — and without this a customer would get a reminder
-- on each tick. Comparing against CURRENT_DATE means at most one reminder per
-- invoice per day no matter how often the sweeper fires.
ALTER TABLE invoices
    ADD COLUMN IF NOT EXISTS last_reminder_on DATE,
    ADD COLUMN IF NOT EXISTS reminder_count   INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS suspended_at     TIMESTAMPTZ;

-- Which invoice caused a suspension, so payment can resume exactly the right
-- customer and the admin can see why an account is on hold. Nullable: most
-- customers are never suspended.
ALTER TABLE customers
    ADD COLUMN IF NOT EXISTS suspended_for_invoice_id UUID REFERENCES invoices(id) ON DELETE SET NULL,
    -- The status the customer held immediately before suspension.
    --
    -- Without this, resuming on payment sets everyone to 'active' — including
    -- someone who had deliberately paused for a holiday, owed last month's
    -- bill, was suspended on the 6th, and then paid. Restoring them to
    -- 'active' would start delivering milk to an empty house while they're
    -- away, because they settled a bill. Paying a debt is not a request to
    -- resume service.
    ADD COLUMN IF NOT EXISTS status_before_suspension TEXT;

-- The sweeper's hot query is "unpaid invoices for month X". Without this it's
-- a full scan of every invoice ever written, run hourly and growing forever.
CREATE INDEX IF NOT EXISTS idx_invoices_unpaid_by_month
    ON invoices (billing_month, status)
    WHERE status NOT LIKE 'PAID%';

-- Records each automated run so a missed month is visible after the fact.
-- Without this, "did the 1st actually fire?" can only be answered by looking
-- for invoices that may never have existed — the failure and the empty success
-- look identical.
CREATE TABLE IF NOT EXISTS dunning_runs (
    id          UUID PRIMARY KEY,
    run_date    DATE        NOT NULL,
    phase       TEXT        NOT NULL CHECK (phase IN ('GENERATE', 'REMIND', 'SUSPEND')),
    billing_month TEXT      NOT NULL,
    affected    INT         NOT NULL DEFAULT 0,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- One row per phase per day. The sweeper uses this to decide whether
    -- today's generate has already happened, so a restart can't double-bill.
    UNIQUE (run_date, phase)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS dunning_runs;
DROP INDEX IF EXISTS idx_invoices_unpaid_by_month;

ALTER TABLE customers
    DROP COLUMN IF EXISTS suspended_for_invoice_id,
    DROP COLUMN IF EXISTS status_before_suspension;

ALTER TABLE invoices
    DROP COLUMN IF EXISTS last_reminder_on,
    DROP COLUMN IF EXISTS reminder_count,
    DROP COLUMN IF EXISTS suspended_at;

-- Anyone currently suspended has to go somewhere the old constraint allows.
-- 'disabled' is the closest fit: not receiving deliveries, and lift-able by
-- an admin.
UPDATE customers SET status = 'disabled' WHERE status = 'suspended';

ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_status_check;
ALTER TABLE customers ADD CONSTRAINT customers_status_check
    CHECK (status IN ('pending', 'active', 'disabled', 'rejected'));

-- +goose StatementEnd
