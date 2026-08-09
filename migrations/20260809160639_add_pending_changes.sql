-- +goose Up
-- +goose StatementBegin

-- Staged customer changes.
--
-- Before this, update.Submit wrote straight into subscriptions and flipped the
-- customer to is_active=false. Three things went wrong at once:
--
--   1. Today's delivery was cancelled rather than modified — the manifest
--      filters on c.is_active, so the customer vanished from it entirely.
--   2. It bypassed the override cutoff. A customer who couldn't legitimately
--      change today's order at 10 AM could silently cancel it instead.
--   3. Rejecting the change killed the customer permanently, and since the
--      new values had already overwritten the old ones there was nothing to
--      revert to.
--
-- Now a change parks here until an admin acts on it. The customer keeps
-- receiving deliveries on their existing order throughout, and a rejection
-- simply discards the row.
--
-- Lifecycle: PENDING -> APPROVED -> APPLIED, or PENDING -> REJECTED.
-- APPROVED sits with effective_from = tomorrow; a sweeper promotes it to
-- APPLIED once that date arrives. Same pattern as pending route prices, and
-- for the same reason: production is planned a day ahead, so nothing a
-- customer submits today can change what's already been milked for today.
CREATE TABLE IF NOT EXISTS pending_subscription_changes (
    id             UUID PRIMARY KEY,
    customer_id    UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,

    -- Proposed identity fields.
    name           TEXT NOT NULL,
    house_address  TEXT NOT NULL,
    geo_latitude   TEXT,
    geo_longitude  TEXT,

    -- Proposed slot rows: [{slot, scheduleType, activeDays, startDate, items}].
    -- JSONB rather than a child table because this is a short-lived proposal,
    -- never queried by shape, and always read whole.
    subscriptions  JSONB NOT NULL,

    -- Set at approval time. Lets the admin re-route in the same action when
    -- an address change means the old stop sequence no longer makes sense.
    assignments    JSONB,

    -- True when the address or coordinates differ from what's on file, so the
    -- admin review screen can flag "this needs re-routing" without diffing.
    address_changed BOOLEAN NOT NULL DEFAULT false,

    review_status  TEXT NOT NULL DEFAULT 'PENDING'
                   CHECK (review_status IN ('PENDING', 'APPROVED', 'APPLIED', 'REJECTED')),
    review_reason  TEXT,
    effective_from DATE,

    submitted_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at    TIMESTAMPTZ,
    applied_at     TIMESTAMPTZ
);

-- One open request per customer, enforced in the database rather than by a
-- read-then-write in the handler. A customer who submits twice gets a clean
-- conflict instead of two competing proposals.
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_change_one_per_customer
    ON pending_subscription_changes (customer_id)
    WHERE review_status = 'PENDING';

-- The sweeper's working set.
CREATE INDEX IF NOT EXISTS idx_pending_change_due
    ON pending_subscription_changes (effective_from)
    WHERE review_status = 'APPROVED';

-- Admin review queue.
CREATE INDEX IF NOT EXISTS idx_pending_change_open
    ON pending_subscription_changes (submitted_at DESC)
    WHERE review_status = 'PENDING';

-- Rejection reasons were previously passed to WhatsApp and thrown away, so
-- nobody could later see why an account was turned down.
ALTER TABLE customers
    ADD COLUMN IF NOT EXISTS rejection_reason TEXT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE customers DROP COLUMN IF EXISTS rejection_reason;
DROP TABLE IF EXISTS pending_subscription_changes;
-- +goose StatementEnd
