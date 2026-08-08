-- +goose Up
-- +goose StatementBegin

-- Complaint cutoff: how long after a delivery a customer may still flag it.
-- Stored as a "wall clock" IST time (e.g. '02:00' = 2 AM), matching the
-- pattern already used for morning/evening driver cutoffs. Interpretation is
-- "this hour on the day AFTER the delivery" — so a 2 AM value gives evening-
-- slot customers about 8 hours and morning-slot customers about 22 hours.
--
-- If NULL, no cutoff is enforced (admin-editable escape hatch for launch).
ALTER TABLE system_config
    ADD COLUMN IF NOT EXISTS complaint_cutoff_time TIME DEFAULT '02:00';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE system_config DROP COLUMN IF EXISTS complaint_cutoff_time;
-- +goose StatementEnd
