-- +goose Up

ALTER TABLE delivery_logs
ADD COLUMN driver_latitude NUMERIC,
ADD COLUMN driver_longitude NUMERIC,
ADD COLUMN captured_at TIMESTAMPTZ,
ADD COLUMN distance_meters FLOAT,
ADD COLUMN proof_image_url TEXT,
ADD COLUMN is_flagged BOOLEAN DEFAULT FALSE;

-- +goose Down
ALTER TABLE delivery_logs
DROP COLUMN IF EXISTS driver_latitude,
DROP COLUMN IF EXISTS driver_longitude,
DROP COLUMN IF EXISTS captured_at,
DROP COLUMN IF EXISTS distance_meters,
DROP COLUMN IF EXISTS proof_image_url,
DROP COLUMN IF EXISTS is_flagged;
