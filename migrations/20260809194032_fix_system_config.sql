-- +goose Up
-- +goose StatementBegin

-- system_config: fix column types and guarantee a row exists.
--
-- Two problems, both silent:
--
-- 1. The table was never seeded. It's created in the initial schema and the
--    only INSERT lives in config.Get, so on a fresh database the row doesn't
--    exist until someone happens to open the admin Settings screen. Until
--    then the shift auto-end sweeper logs "no rows in result set" and gives
--    up on every tick.
--
-- 2. morning_cutoff_time and evening_cutoff_time are TEXT holding 'HH:MM',
--    while complaint_cutoff_time is a proper TIME. That mismatch breaks two
--    consumers without either of them erroring visibly:
--
--      • driver/shifts.go reads the column as text ("21:00") and parses it
--        with the layout "15:04:05". Two parts against three fails, the
--        helper returns false, and shifts never auto-end.
--
--      • override.go calls TO_CHAR(morning_cutoff_time, 'HH24:MI'). Postgres
--        has no to_char(text, text), so the query errors, the error is
--        swallowed by a fallback branch, and the hardcoded defaults are used
--        instead of whatever the admin configured.
--
--    Converting to TIME makes ::text produce 'HH:MM:SS' — which is what both
--    the Go parse layout and TO_CHAR expect — and lines all three cutoffs up
--    on one type.
--
-- NOTE on ordering: the existing DEFAULT on evening_cutoff_time is a TEXT
-- literal. Postgres re-evaluates a column's default against the new type
-- during ALTER TYPE and refuses when it can't cast automatically, so the
-- default has to come off first and go back on after. Kept as separate
-- statements rather than one multi-clause ALTER, because subcommand ordering
-- within a single ALTER TABLE isn't something to rely on here.

ALTER TABLE system_config ALTER COLUMN morning_cutoff_time DROP DEFAULT;
ALTER TABLE system_config ALTER COLUMN evening_cutoff_time DROP DEFAULT;

ALTER TABLE system_config
    ALTER COLUMN morning_cutoff_time TYPE TIME USING morning_cutoff_time::time;
ALTER TABLE system_config
    ALTER COLUMN evening_cutoff_time TYPE TIME USING evening_cutoff_time::time;

ALTER TABLE system_config ALTER COLUMN morning_cutoff_time SET DEFAULT '03:00';
ALTER TABLE system_config ALTER COLUMN evening_cutoff_time SET DEFAULT '15:00';

-- Seed the singleton row. Guarded so re-running is harmless, and so an
-- existing deployment that already seeded via the Settings screen isn't
-- given a second row.
INSERT INTO system_config (morning_cutoff_time, evening_cutoff_time, complaint_cutoff_time)
SELECT '03:00'::time, '15:00'::time, '02:00'::time
WHERE NOT EXISTS (SELECT 1 FROM system_config);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Back to TEXT. Values become 'HH:MM:SS' rather than the original 'HH:MM',
-- which every reader tolerates. Same drop-default-first dance in reverse.
ALTER TABLE system_config ALTER COLUMN morning_cutoff_time DROP DEFAULT;
ALTER TABLE system_config ALTER COLUMN evening_cutoff_time DROP DEFAULT;

ALTER TABLE system_config
    ALTER COLUMN morning_cutoff_time TYPE TEXT USING morning_cutoff_time::text;
ALTER TABLE system_config
    ALTER COLUMN evening_cutoff_time TYPE TEXT USING evening_cutoff_time::text;

ALTER TABLE system_config ALTER COLUMN evening_cutoff_time SET DEFAULT '15:00';

-- +goose StatementEnd
