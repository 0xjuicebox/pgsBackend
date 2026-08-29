-- +goose Up
-- +goose StatementBegin

-- Per-item delivery frequency, stage 1: schema and predicate only.
--
-- Deploying this changes no behaviour. item_schedules starts NULL for every
-- subscription, NULL means "inherit the slot schedule", and item_due_on()
-- returns exactly what the five hand-written predicates returned. That is the
-- point: the refactor lands on its own so that if something moves, it is the
-- refactor and not the feature.
--
-- WHY A JSONB MAP RATHER THAN COLUMNS
--
-- The alternative was eleven column triples — milk_schedule_type,
-- milk_active_days, milk_anchor_date, and the same for ten more products.
-- Thirty-three columns, every query grown by eleven near-identical branches,
-- and a new product costing three migrations.
--
-- The map is read as a whole, always alongside its subscription, always to
-- answer one question about one date. It is never queried independently, which
-- is the case that would have justified a separate table.
--
-- Shape:
--
--   {
--     "milk":   { "type": "daily" },
--     "butter": { "type": "alternate", "anchor": "2026-08-24" },
--     "atta":   { "type": "custom",    "days": [2, 5] }
--   }
--
-- A product absent from the map falls through to the slot-level schedule.

ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS item_schedules JSONB;

-- +goose StatementEnd

-- +goose StatementBegin

-- item_due_on answers "is this product due on this date?".
--
-- WHY THIS LIVES IN SQL
--
-- The predicate existed in five places: route.GenerateManifest,
-- route.LockManifest, driver.CloseRouteForSlot, stats.dueTodayCTE and
-- schedule.NextDelivery. They agreed by coincidence rather than by
-- construction, and a divergence between them has already caused one bug in
-- this system.
--
-- Three of those five are inside queries that also filter rows. Moving the
-- logic to Go would mean fetching every subscription and filtering in
-- application code — an indexed query becomes a full scan plus a loop, on the
-- hot path of every delivery round. Keeping it in SQL keeps it where the
-- filtering happens, and makes it one implementation instead of five.
--
-- IMMUTABLE so the planner can inline it rather than calling it per row.
--
-- Arguments are the item's own schedule first, then the slot-level fallbacks.
-- Passing NULL for item_schedule reproduces the old behaviour exactly, which
-- is how stage 1 proves itself.
CREATE OR REPLACE FUNCTION item_due_on(
    item_schedule JSONB,
    slot_type     TEXT,
    slot_days     INT[],
    slot_anchor   DATE,
    target        DATE
) RETURNS BOOLEAN AS $$
DECLARE
    s_type   TEXT;
    s_days   INT[];
    s_anchor DATE;
BEGIN
    -- Resolve the effective schedule: the item's own, or the slot's.
    --
    -- Per field rather than per object, so an item schedule that specifies a
    -- type but no anchor still inherits the slot's anchor instead of falling
    -- back to a NULL date and silently never being due.
    IF item_schedule IS NULL OR item_schedule->>'type' IS NULL THEN
        s_type   := slot_type;
        s_days   := slot_days;
        s_anchor := slot_anchor;
    ELSE
        s_type := item_schedule->>'type';

        s_days := CASE
            WHEN item_schedule ? 'days'
             AND jsonb_typeof(item_schedule->'days') = 'array'
            THEN ARRAY(SELECT jsonb_array_elements_text(item_schedule->'days')::int)
            ELSE slot_days
        END;

        s_anchor := COALESCE((item_schedule->>'anchor')::date, slot_anchor);
    END IF;

    IF s_type = 'custom' THEN
        -- EXTRACT(DOW) is 0=Sunday..6=Saturday, matching active_days.
        RETURN EXTRACT(DOW FROM target)::int = ANY(COALESCE(s_days, ARRAY[0,1,2,3,4,5,6]));

    ELSIF s_type = 'alternate' THEN
        -- Whole days between two dates; Postgres date subtraction is already
        -- an integer count, so no time-of-day can shift the parity.
        --
        -- A NULL anchor would make this NULL rather than false, which in a
        -- WHERE clause silently drops the row. Defaulting to the target date
        -- means "due today" — the safer direction, since an extra delivery is
        -- visible and a missing one is not.
        RETURN (target - COALESCE(s_anchor, target)) % 2 = 0;

    ELSE
        -- 'daily', and anything unrecognised.
        --
        -- Deliberately permissive: a typo in schedule_type should over-deliver
        -- rather than quietly stop a customer's milk. The former gets noticed
        -- the same morning.
        RETURN TRUE;
    END IF;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION IF EXISTS item_due_on(JSONB, TEXT, INT[], DATE, DATE);
ALTER TABLE subscriptions DROP COLUMN IF EXISTS item_schedules;

-- +goose StatementEnd
