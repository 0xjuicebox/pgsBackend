-- +goose Up
-- +goose StatementBegin
ALTER TABLE delivery_logs ADD COLUMN customer_feedback TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE delivery_logs DROP COLUMN customer_feedback;
-- +goose StatementEnd
