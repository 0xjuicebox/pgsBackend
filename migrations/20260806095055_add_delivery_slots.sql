-- +goose Up

-- Introduces the delivery slot dimension (morning / evening).
--
-- Slot is modelled as a ROW dimension, not a column dimension: a customer who
-- takes both a morning and an evening delivery has two subscription rows, two
-- override rows for a given date, and two delivery logs per day. This keeps the
-- per-product column count flat and makes evening delivery opt-in by default
-- (no row = no evening delivery).
--
-- Every existing row is backfilled to 'morning', which matches current
-- behaviour exactly — nothing changes for customers already on the books.

-- 1. SUBSCRIPTIONS ---------------------------------------------------------

ALTER TABLE subscriptions
ADD COLUMN slot TEXT NOT NULL DEFAULT 'morning';

ALTER TABLE subscriptions
ADD CONSTRAINT subscriptions_slot_check CHECK (slot IN ('morning', 'evening'));

-- The old UNIQUE(customer_id) was created inline on the column, so Postgres
-- named it subscriptions_customer_id_key. It has to go: one customer now owns
-- up to one subscription PER SLOT.
ALTER TABLE subscriptions
DROP CONSTRAINT IF EXISTS subscriptions_customer_id_key;

ALTER TABLE subscriptions
ADD CONSTRAINT unique_customer_slot UNIQUE (customer_id, slot);

-- 2. ORDER OVERRIDES -------------------------------------------------------

ALTER TABLE order_overrides
ADD COLUMN slot TEXT NOT NULL DEFAULT 'morning';

ALTER TABLE order_overrides
ADD CONSTRAINT order_overrides_slot_check CHECK (slot IN ('morning', 'evening'));

ALTER TABLE order_overrides
DROP CONSTRAINT IF EXISTS unique_customer_target_date;

ALTER TABLE order_overrides
ADD CONSTRAINT unique_customer_target_date_slot UNIQUE (customer_id, target_date, slot);

-- 3. DELIVERY LOGS ---------------------------------------------------------

ALTER TABLE delivery_logs
ADD COLUMN slot TEXT NOT NULL DEFAULT 'morning';

ALTER TABLE delivery_logs
ADD CONSTRAINT delivery_logs_slot_check CHECK (slot IN ('morning', 'evening'));

ALTER TABLE delivery_logs
DROP CONSTRAINT IF EXISTS unique_daily_delivery;

ALTER TABLE delivery_logs
ADD CONSTRAINT unique_daily_delivery UNIQUE (customer_id, delivery_date, slot);

-- Manifest generation filters hard on (route, date, slot); without this index
-- that query degrades into a sequential scan as the log table grows.
CREATE INDEX IF NOT EXISTS idx_delivery_logs_route_date_slot
ON delivery_logs (route_id, delivery_date, slot);


-- +goose Down

DROP INDEX IF EXISTS idx_delivery_logs_route_date_slot;

-- Collapsing back to one delivery per customer per day would violate the old
-- unique constraint wherever an evening row exists, so those rows are dropped
-- on the way down. This is destructive by necessity.
DELETE FROM delivery_logs WHERE slot = 'evening';
DELETE FROM order_overrides WHERE slot = 'evening';
DELETE FROM subscriptions WHERE slot = 'evening';

ALTER TABLE delivery_logs DROP CONSTRAINT IF EXISTS unique_daily_delivery;
ALTER TABLE delivery_logs DROP CONSTRAINT IF EXISTS delivery_logs_slot_check;
ALTER TABLE delivery_logs DROP COLUMN IF EXISTS slot;
ALTER TABLE delivery_logs ADD CONSTRAINT unique_daily_delivery UNIQUE (customer_id, delivery_date);

ALTER TABLE order_overrides DROP CONSTRAINT IF EXISTS unique_customer_target_date_slot;
ALTER TABLE order_overrides DROP CONSTRAINT IF EXISTS order_overrides_slot_check;
ALTER TABLE order_overrides DROP COLUMN IF EXISTS slot;
ALTER TABLE order_overrides ADD CONSTRAINT unique_customer_target_date UNIQUE (customer_id, target_date);

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS unique_customer_slot;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_slot_check;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS slot;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_customer_id_key UNIQUE (customer_id);
