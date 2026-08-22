-- +goose Up
-- +goose StatementBegin

-- Manifest locking.
--
-- THE PROBLEM
--
-- The manifest was never stored. GenerateManifest ran a live query every time
-- a driver's app or the admin screen asked for one, computing the list from
-- subscriptions, overrides and today's delivery logs on the spot.
--
-- Three consequences, all bad:
--
--  1. No record a driver was ever given a manifest. A driver who never opened
--     the app produced nothing at all — no rows, no evidence, and a dashboard
--     that showed the run as neither started nor missed.
--
--  2. Past manifests were fiction. Paging back to last Tuesday reconstructed
--     the plan from TODAY's subscriptions, so a customer who changed their
--     order since would show this week's quantities against last week's date.
--
--  3. The list could change under a driver mid-round. An admin editing an
--     order at 06:00 changed what the driver saw at 06:05, after the van was
--     already loaded to the old plan.
--
-- THE FIX
--
-- At each slot's existing order cutoff — the moment changes already stop
-- being accepted — a sweeper writes one delivery_logs row per due stop with
-- the planned quantities frozen into planned_order.
--
-- WHY planned_order IS A SEPARATE COLUMN
--
-- delivery_logs already has delivered_*_qty, and reusing those would have
-- been simpler. But the driver overwrites them when they deliver, so the plan
-- would be destroyed by the first delivery — leaving a record that a manifest
-- existed without any record of what it said. Planned and actual have to be
-- two things or "delivered short" is not a question the data can answer.

ALTER TABLE delivery_logs
    -- The frozen plan: {"milk": 2000, "curd": 500}, base units, same keys as
    -- the billing breakdown. Written once at lock time.
    ADD COLUMN IF NOT EXISTS planned_order JSONB,
    -- When the plan was frozen. NULL means this row predates locking, or was
    -- created by a driver marking a stop that wasn't on the locked manifest.
    ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ,
    -- Set when an admin changes the plan after the lock. Drives the "edited
    -- after lock" marker, so a driver whose list changed mid-round can see
    -- that it was deliberate rather than assume the app is wrong.
    ADD COLUMN IF NOT EXISTS plan_edited_at TIMESTAMPTZ;

-- One lock per route, slot and date.
--
-- Separate from delivery_logs because the useful question is "was this round
-- locked?", which has no answer in a table of stops when the round legitimately
-- has zero due customers. Without this, an empty round and an unlocked round
-- look identical.
CREATE TABLE IF NOT EXISTS manifest_locks (
    id            UUID PRIMARY KEY,
    route_id      UUID NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    slot          TEXT NOT NULL CHECK (slot IN ('morning', 'evening')),
    manifest_date DATE NOT NULL,
    stop_count    INT  NOT NULL DEFAULT 0,
    locked_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (route_id, slot, manifest_date)
);

-- The sweeper runs every ten minutes and asks "which rounds still need
-- locking today". Without this it scans the whole table each time, forever.
CREATE INDEX IF NOT EXISTS idx_manifest_locks_date
    ON manifest_locks (manifest_date, slot);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_manifest_locks_date;
DROP TABLE IF EXISTS manifest_locks;

ALTER TABLE delivery_logs
    DROP COLUMN IF EXISTS planned_order,
    DROP COLUMN IF EXISTS locked_at,
    DROP COLUMN IF EXISTS plan_edited_at;

-- +goose StatementEnd
