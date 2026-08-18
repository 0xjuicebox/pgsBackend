-- +goose Up
-- +goose StatementBegin

-- Separate "when can a customer still change today's order" from "when does a
-- driver's shift stop counting as in-progress".
--
-- Before this, evening_cutoff_time did both jobs. sweepAutoEnd read it and
-- closed EVERY active shift — morning and evening alike — once that time
-- passed. With the seeded default of 15:00 that meant an evening driver who
-- started their run at 16:00 had their shift flipped to AUTO_ENDED within five
-- minutes, while they were still delivering.
--
-- The damage was cosmetic rather than operational: nothing in delivery.go
-- reads the shifts table, so logging a delivery against an auto-ended shift
-- still works. But the admin dashboard's "runs in progress" count went to zero
-- while vans were on the road, which is exactly the number an operations
-- person trusts during the evening run.
--
-- Four independent times now:
--
--   morning_cutoff_time     03:00  customer's deadline to change today's morning order
--   evening_cutoff_time     14:00  customer's deadline to change today's evening order
--   morning_shift_end_time  12:00  morning shifts still ACTIVE past this are auto-ended
--   evening_shift_end_time  22:00  evening shifts still ACTIVE past this are auto-ended
--
-- All are IST wall-clock times, matching the existing TIME columns.

ALTER TABLE system_config
    ADD COLUMN IF NOT EXISTS morning_shift_end_time TIME DEFAULT '12:00',
    ADD COLUMN IF NOT EXISTS evening_shift_end_time TIME DEFAULT '22:00';

-- Backfill the singleton row. ADD COLUMN ... DEFAULT populates existing rows
-- in modern Postgres, but being explicit costs nothing and makes the intent
-- readable if this is ever run against a partially-migrated database.
UPDATE system_config
SET morning_shift_end_time = COALESCE(morning_shift_end_time, '12:00'::time),
    evening_shift_end_time = COALESCE(evening_shift_end_time, '22:00'::time);

-- The evening ORDER cutoff moves 15:00 -> 14:00. This is a business decision,
-- not a consequence of the split: two o'clock gives the depot a clear two
-- hours to pack before the evening run.
--
-- Applied to the live row as well as the default, because the seeded row
-- already holds 15:00 and a default change alone would not touch it.
ALTER TABLE system_config ALTER COLUMN evening_cutoff_time SET DEFAULT '14:00';
UPDATE system_config SET evening_cutoff_time = '14:00'::time
WHERE evening_cutoff_time = '15:00'::time;

-- Guard the ordering that makes the whole scheme coherent: a slot's order
-- cutoff must come before that slot's shift end, or a customer could change
-- an order for a van that has already finished its round.
--
-- NOT VALID so an existing row that somehow violates this doesn't block the
-- migration; new writes are still checked.
ALTER TABLE system_config
    ADD CONSTRAINT system_config_morning_order_before_shift_end
    CHECK (morning_cutoff_time < morning_shift_end_time) NOT VALID;

ALTER TABLE system_config
    ADD CONSTRAINT system_config_evening_order_before_shift_end
    CHECK (evening_cutoff_time < evening_shift_end_time) NOT VALID;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE system_config DROP CONSTRAINT IF EXISTS system_config_morning_order_before_shift_end;
ALTER TABLE system_config DROP CONSTRAINT IF EXISTS system_config_evening_order_before_shift_end;

ALTER TABLE system_config ALTER COLUMN evening_cutoff_time SET DEFAULT '15:00';
UPDATE system_config SET evening_cutoff_time = '15:00'::time
WHERE evening_cutoff_time = '14:00'::time;

ALTER TABLE system_config DROP COLUMN IF EXISTS morning_shift_end_time;
ALTER TABLE system_config DROP COLUMN IF EXISTS evening_shift_end_time;

-- +goose StatementEnd
