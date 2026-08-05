-- +goose Up
-- +goose StatementBegin
ALTER TABLE delivery_logs
ADD COLUMN driver_id uuid REFERENCES drivers(id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE delivery_logs
DROP COLUMN driver_id;
-- +goose StatementEnd
