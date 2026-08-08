-- +goose Up
-- +goose StatementBegin

-- Shifts: one row per (driver, date, slot). The uniqueness constraint is what
-- makes "start shift" idempotent — the mobile client can safely POST twice
-- and only one row exists.
--
-- Status transitions: ACTIVE → ENDED (driver taps End) or ACTIVE → AUTO_ENDED
-- (background sweeper past evening cutoff). ENDED and AUTO_ENDED are terminal;
-- there is no reopen. If a driver needs to log something after AUTO_ENDED, the
-- delivery still writes fine — the shift row is only a housekeeping marker for
-- the admin dashboard's "runs in progress" count.
CREATE TABLE IF NOT EXISTS shifts (
    id           UUID PRIMARY KEY,
    driver_id    UUID NOT NULL REFERENCES drivers(id) ON DELETE CASCADE,
    shift_date   DATE NOT NULL,
    slot         TEXT NOT NULL CHECK (slot IN ('morning', 'evening')),
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at     TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ENDED', 'AUTO_ENDED')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (driver_id, shift_date, slot)
);

CREATE INDEX IF NOT EXISTS idx_shifts_active ON shifts (status) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_shifts_driver_date ON shifts (driver_id, shift_date DESC);

-- Sync failures: the "admin bucket" for delivery payloads the driver's client
-- gave up on. Rows land here in two cases:
--   1. Backend permanently rejected the payload (400/409) — reason REJECTED_400
--   2. Retries failed for 48h — reason TIMED_OUT_48H
-- The admin log-correction screen (not yet built) will read from here.
--
-- payload is JSONB so we can display whatever the driver's client attempted
-- without a schema migration every time the DeliveryPayload shape changes.
CREATE TABLE IF NOT EXISTS delivery_sync_failures (
    id            UUID PRIMARY KEY,
    driver_id     UUID REFERENCES drivers(id) ON DELETE SET NULL,
    payload       JSONB NOT NULL,
    error_message TEXT NOT NULL,
    reason        TEXT NOT NULL CHECK (reason IN ('REJECTED_400', 'TIMED_OUT_48H', 'OTHER')),
    reported_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_sync_failures_open
    ON delivery_sync_failures (reported_at DESC)
    WHERE resolved_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS delivery_sync_failures;
DROP TABLE IF EXISTS shifts;
-- +goose StatementEnd
