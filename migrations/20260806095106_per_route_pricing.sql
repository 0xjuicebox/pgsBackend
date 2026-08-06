-- +goose Up

-- Per-route pricing, with price snapshotting on delivery.
--
-- Two halves that work together:
--
--   routes.price_*          — the CURRENT price list for that route. What a
--                             delivery on this route will cost from now on.
--
--   delivery_logs.unit_price_* — what was ACTUALLY charged, frozen at the
--                             moment of delivery.
--
-- Billing reads only from delivery_logs and never joins to routes or
-- system_config. That is what makes a mid-month price change correct (earlier
-- days keep the old price) and what makes moving a customer between routes
-- correct (their delivery history doesn't reprice itself).
--
-- Order matters below: both backfills read system_config prices, so the
-- system_config price columns are only dropped at the very end.


-- 1. CURRENT PRICE LIST, PER ROUTE -----------------------------------------

ALTER TABLE routes
ADD COLUMN price_milk    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_curd    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_butter  NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_ghee    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_lassi   NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_paneer  NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_jaggery NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_khand   NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_oil     NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_atta    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN price_burfi   NUMERIC(10,2) NOT NULL DEFAULT 0;

-- Seed every route with the old global price list, so nothing silently drops
-- to zero. Differentiating routes is then an admin edit, not a data migration.
UPDATE routes r
SET price_milk    = cfg.price_milk,
    price_curd    = cfg.price_curd,
    price_butter  = cfg.price_butter,
    price_ghee    = cfg.price_ghee,
    price_lassi   = cfg.price_lassi,
    price_paneer  = cfg.price_paneer,
    price_jaggery = cfg.price_jaggery,
    price_khand   = cfg.price_khand,
    price_oil     = cfg.price_oil,
    price_atta    = cfg.price_atta,
    price_burfi   = cfg.price_burfi
FROM (SELECT * FROM system_config LIMIT 1) cfg;


-- 2. PRICE SNAPSHOT, PER DELIVERY ------------------------------------------

ALTER TABLE delivery_logs
ADD COLUMN unit_price_milk    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_curd    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_butter  NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_ghee    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_lassi   NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_paneer  NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_jaggery NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_khand   NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_oil     NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_atta    NUMERIC(10,2) NOT NULL DEFAULT 0,
ADD COLUMN unit_price_burfi   NUMERIC(10,2) NOT NULL DEFAULT 0;

-- Backfill history at the old global prices. Without this every past delivery
-- would price at zero and all existing revenue would vanish from billing.
UPDATE delivery_logs dl
SET unit_price_milk    = cfg.price_milk,
    unit_price_curd    = cfg.price_curd,
    unit_price_butter  = cfg.price_butter,
    unit_price_ghee    = cfg.price_ghee,
    unit_price_lassi   = cfg.price_lassi,
    unit_price_paneer  = cfg.price_paneer,
    unit_price_jaggery = cfg.price_jaggery,
    unit_price_khand   = cfg.price_khand,
    unit_price_oil     = cfg.price_oil,
    unit_price_atta    = cfg.price_atta,
    unit_price_burfi   = cfg.price_burfi
FROM (SELECT * FROM system_config LIMIT 1) cfg;

-- Billing aggregates by month across all customers; this supports that scan.
CREATE INDEX IF NOT EXISTS idx_delivery_logs_status_date
ON delivery_logs (status, delivery_date);


-- 3. SYSTEM CONFIG BECOMES CUTOFFS ONLY ------------------------------------

-- Prices no longer live here. What remains is scheduling: the deadline after
-- which a customer can no longer change that slot's order.
ALTER TABLE system_config RENAME COLUMN nightly_cutoff_time TO morning_cutoff_time;

ALTER TABLE system_config
ADD COLUMN evening_cutoff_time TEXT NOT NULL DEFAULT '15:00';

ALTER TABLE system_config
DROP COLUMN IF EXISTS price_milk,
DROP COLUMN IF EXISTS price_curd,
DROP COLUMN IF EXISTS price_butter,
DROP COLUMN IF EXISTS price_ghee,
DROP COLUMN IF EXISTS price_lassi,
DROP COLUMN IF EXISTS price_paneer,
DROP COLUMN IF EXISTS price_jaggery,
DROP COLUMN IF EXISTS price_khand,
DROP COLUMN IF EXISTS price_oil,
DROP COLUMN IF EXISTS price_atta,
DROP COLUMN IF EXISTS price_burfi;


-- +goose Down

ALTER TABLE system_config
ADD COLUMN price_milk    NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_curd    NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_butter  NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_ghee    NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_lassi   NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_paneer  NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_jaggery NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_khand   NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_oil     NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_atta    NUMERIC(10,2) DEFAULT 0,
ADD COLUMN price_burfi   NUMERIC(10,2) DEFAULT 0;

-- Collapse back to a single global price list by taking the oldest route's
-- prices. Any per-route differentiation is lost on the way down.
UPDATE system_config
SET price_milk    = r.price_milk,
    price_curd    = r.price_curd,
    price_butter  = r.price_butter,
    price_ghee    = r.price_ghee,
    price_lassi   = r.price_lassi,
    price_paneer  = r.price_paneer,
    price_jaggery = r.price_jaggery,
    price_khand   = r.price_khand,
    price_oil     = r.price_oil,
    price_atta    = r.price_atta,
    price_burfi   = r.price_burfi
FROM (SELECT * FROM routes ORDER BY created_at ASC LIMIT 1) r;

ALTER TABLE system_config DROP COLUMN IF EXISTS evening_cutoff_time;
ALTER TABLE system_config RENAME COLUMN morning_cutoff_time TO nightly_cutoff_time;

DROP INDEX IF EXISTS idx_delivery_logs_status_date;

ALTER TABLE delivery_logs
DROP COLUMN IF EXISTS unit_price_milk,
DROP COLUMN IF EXISTS unit_price_curd,
DROP COLUMN IF EXISTS unit_price_butter,
DROP COLUMN IF EXISTS unit_price_ghee,
DROP COLUMN IF EXISTS unit_price_lassi,
DROP COLUMN IF EXISTS unit_price_paneer,
DROP COLUMN IF EXISTS unit_price_jaggery,
DROP COLUMN IF EXISTS unit_price_khand,
DROP COLUMN IF EXISTS unit_price_oil,
DROP COLUMN IF EXISTS unit_price_atta,
DROP COLUMN IF EXISTS unit_price_burfi;

ALTER TABLE routes
DROP COLUMN IF EXISTS price_milk,
DROP COLUMN IF EXISTS price_curd,
DROP COLUMN IF EXISTS price_butter,
DROP COLUMN IF EXISTS price_ghee,
DROP COLUMN IF EXISTS price_lassi,
DROP COLUMN IF EXISTS price_paneer,
DROP COLUMN IF EXISTS price_jaggery,
DROP COLUMN IF EXISTS price_khand,
DROP COLUMN IF EXISTS price_oil,
DROP COLUMN IF EXISTS price_atta,
DROP COLUMN IF EXISTS price_burfi;
