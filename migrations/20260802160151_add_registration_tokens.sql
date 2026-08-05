-- +goose Up
-- +goose StatementBegin
CREATE TABLE registration_tokens (
    token TEXT PRIMARY KEY,
    phone_number TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);
CREATE INDEX idx_registration_tokens_phone ON registration_tokens (phone_number);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE registration_tokens;
-- +goose StatementEnd
