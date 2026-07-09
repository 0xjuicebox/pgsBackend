-- +goose Up
CREATE TABLE routes (
id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
name TEXT NOT NULL UNIQUE,
description TEXT,
driver_id UUID UNIQUE,
created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL
);

ALTER TABLE customers
ADD COLUMN route_id UUID REFERENCES routes(id) on DELETE SET NULL;

-- +goose Down
ALTER TABLE customers DROP COLUMN IF EXISTS route_id;
DROP TABLE IF EXISTS routes;
