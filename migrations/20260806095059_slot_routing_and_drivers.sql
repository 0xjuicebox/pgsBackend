-- +goose Up

-- Per-slot routing and per-slot drivers.
--
-- Two structural moves here:
--
--   1. Route assignment moves from customers -> subscriptions. A customer is no
--      longer "on a route"; their per-slot subscription is. This is what lets
--      someone take a morning delivery on Route A and an evening one on Route B.
--
--   2. Driver assignment moves from routes -> route_slot_drivers, so the same
--      route can be run by different drivers in the morning and the evening.
--
-- WARNING: this drops customers.route_id, customers.stop_order and
-- routes.driver_id. Any code still reading those columns will break the moment
-- this runs — customer.Approve, route.GetRoster, route.UpdateSequence and
-- GenerateManifest all need updating alongside it.


-- 1. ROUTE ASSIGNMENT MOVES ONTO THE SUBSCRIPTION --------------------------

ALTER TABLE subscriptions
ADD COLUMN route_id UUID REFERENCES routes(id) ON DELETE SET NULL,
ADD COLUMN stop_order INT NOT NULL DEFAULT 0;

-- Backfill from the customer record. Every subscription row is currently
-- slot='morning' (migration 001 backfilled it that way), so this is an exact
-- one-to-one carry-over with no ambiguity.
UPDATE subscriptions s
SET route_id = c.route_id,
    stop_order = COALESCE(c.stop_order, 0)
FROM customers c
WHERE c.id = s.customer_id;

-- Manifest and roster queries both filter on (route_id, slot) and order by
-- stop_order. Without this they degrade to a full scan of subscriptions.
CREATE INDEX IF NOT EXISTS idx_subscriptions_route_slot
ON subscriptions (route_id, slot, stop_order);

-- route_id is deliberately left nullable: a customer registers via WhatsApp and
-- gets a subscription row immediately, but has no route until an admin
-- approves them. Manifest generation ignores rows where route_id IS NULL.
ALTER TABLE customers DROP COLUMN IF EXISTS route_id;
ALTER TABLE customers DROP COLUMN IF EXISTS stop_order;


-- 2. DRIVER ASSIGNMENT MOVES ONTO (ROUTE, SLOT) ----------------------------

CREATE TABLE route_slot_drivers (
    route_id  UUID NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    slot      TEXT NOT NULL,
    driver_id UUID REFERENCES drivers(id) ON DELETE SET NULL,

    created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,

    PRIMARY KEY (route_id, slot),
    CONSTRAINT route_slot_drivers_slot_check CHECK (slot IN ('morning', 'evening')),

    -- A driver can run Route A in the morning and Route B in the evening, but
    -- cannot be on two routes in the same slot at once.
    CONSTRAINT unique_driver_per_slot UNIQUE (driver_id, slot)
);

CREATE INDEX IF NOT EXISTS idx_route_slot_drivers_driver
ON route_slot_drivers (driver_id, slot);

-- Carry existing assignments over as the morning run.
INSERT INTO route_slot_drivers (route_id, slot, driver_id)
SELECT id, 'morning', driver_id
FROM routes
WHERE driver_id IS NOT NULL;

ALTER TABLE routes DROP COLUMN IF EXISTS driver_id;


-- +goose Down

-- Restore the single-driver-per-route model, taking the morning assignment as
-- the winner. Any evening-only driver assignment is lost.
ALTER TABLE routes ADD COLUMN driver_id UUID UNIQUE;

UPDATE routes r
SET driver_id = rsd.driver_id
FROM route_slot_drivers rsd
WHERE rsd.route_id = r.id AND rsd.slot = 'morning';

DROP INDEX IF EXISTS idx_route_slot_drivers_driver;
DROP TABLE IF EXISTS route_slot_drivers;

-- Restore route assignment on the customer, again taking morning as the
-- winner. A customer whose evening route differed will lose that distinction.
ALTER TABLE customers
ADD COLUMN route_id UUID REFERENCES routes(id) ON DELETE SET NULL,
ADD COLUMN stop_order INT NOT NULL DEFAULT 0;

UPDATE customers c
SET route_id = s.route_id,
    stop_order = s.stop_order
FROM subscriptions s
WHERE s.customer_id = c.id AND s.slot = 'morning';

DROP INDEX IF EXISTS idx_subscriptions_route_slot;

ALTER TABLE subscriptions DROP COLUMN IF EXISTS route_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS stop_order;
