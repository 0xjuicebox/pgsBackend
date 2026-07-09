-- +goose Up
CREATE TABLE drivers (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  phone_number TEXT NOT NULL UNIQUE,
  is_active BOOLEAN DEFAULT true NOT NULL,
  created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL
);


ALTER TABLE routes
ADD CONSTRAINT routes_driver_id_fkey FOREIGN KEY (driver_id) REFERENCES drivers(id);

-- +goose Down
ALTER TABLE routes DROP CONSTRAINT IF EXISTS routes_driver_id_fkey;
DROP TABLE IF EXISTS driver;

