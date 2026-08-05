-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_status_check;

ALTER TABLE customers ADD CONSTRAINT customers_status_check
CHECK (status IN ('pending', 'active', 'disabled', 'rejected'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Safely revert rejected users to disabled to avoid constraint crash during rollback
UPDATE customers SET status = 'disabled' WHERE status = 'rejected';

ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_status_check;

ALTER TABLE customers ADD CONSTRAINT customers_status_check
CHECK (status IN ('pending', 'active', 'disabled'));
-- +goose StatementEnd
