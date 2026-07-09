-- +goose Up
ALTER TABLE customers ADD COLUMN stop_order INT DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE customers DROP COLUMN IF EXISTS stop_order;
