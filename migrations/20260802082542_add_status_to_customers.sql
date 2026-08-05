-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE customers ADD CONSTRAINT customers_status_check CHECK (status IN ('pending', 'active', 'disabled'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE customers DROP CONSTRAINT customers_status_check;
ALTER TABLE customers DROP COLUMN status;
-- +goose StatementEnd
