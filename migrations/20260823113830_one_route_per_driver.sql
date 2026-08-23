-- +goose Up
-- +goose StatementBegin

-- One driver runs at most one route per slot.
--
-- THE PROBLEM
--
-- route_slot_drivers is keyed (route_id, slot), so nothing stopped assigning
-- the same driver to Route A morning AND Route B morning. With three or four
-- routes and a couple of drivers, that assignment is likely rather than
-- exotic.
--
-- Both GetMobileManifest and CloseRouteForSlot resolve the driver's route
-- with:
--
--     SELECT route_id FROM route_slot_drivers
--     WHERE driver_id = $1 AND slot = $2 LIMIT 1
--
-- LIMIT 1 with no ORDER BY. Postgres is free to return either row, and free
-- to return a different one on the next call. So a doubly-assigned driver
-- sees one route's stops and never learns the other exists — while ending
-- their shift might close the route they didn't deliver, leaving the one they
-- did deliver open and the one they didn't marked complete.
--
-- WHY A CONSTRAINT RATHER THAN HANDLING MULTIPLE ROUTES
--
-- Supporting two routes per driver per slot means deciding how the stops
-- interleave, what "end shift" means when one route is done and the other
-- isn't, and how the manifest screen presents them. Those are real product
-- questions, and the answer for a pilot is that a driver runs one round.
--
-- This turns silent, non-deterministic wrongness into an error at the moment
-- of assignment, where an admin can see it and pick a different driver.
--
-- Partial index rather than a table constraint because driver_id is nullable:
-- an unassigned slot is a legitimate row, and many of them must be allowed to
-- coexist.

CREATE UNIQUE INDEX IF NOT EXISTS uniq_driver_per_slot
    ON route_slot_drivers (driver_id, slot)
    WHERE driver_id IS NOT NULL;

-- Marks a shift the driver deliberately restarted after it had ended.
--
-- WHY THIS IS NEEDED
--
-- StartShift now reopens an ended shift instead of doing nothing, which fixes
-- a driver being stranded mid-round. But the auto-end sweeper wakes every five
-- minutes and ends any ACTIVE shift past the slot's end time — so a driver who
-- restarts at 11:35 is auto-ended again at 11:40, restarts, is ended again.
-- The fix would have created a loop.
--
-- The sweeper exists to catch drivers who FORGET to end their shift. A driver
-- who has explicitly restarted after an auto-end is telling us they are still
-- out delivering, and the sweeper should not argue with them.

ALTER TABLE shifts
    ADD COLUMN IF NOT EXISTS reopened_at TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS uniq_driver_per_slot;
ALTER TABLE shifts DROP COLUMN IF EXISTS reopened_at;

-- +goose StatementEnd
