-- +goose Up
-- +goose StatementBegin

-- Deferred price changes.
--
-- Business rule: a price change on an established route takes effect from the
-- 1st of the following month, never mid-month. Deliveries already made keep
-- the price snapshotted onto their delivery_log row, and deliveries still to
-- come this month must be charged the price the customer was quoted when the
-- month began.
--
-- Rather than making the billing path aware of effective dates (which would
-- put a date comparison in the hot path for every delivery), the new price
-- parks in pending_price_* until a daily sweeper promotes it into price_*.
-- LogDelivery keeps reading price_* and needs no change at all.
--
-- pending_effective_from NULL means "nothing queued".
ALTER TABLE routes
    ADD COLUMN IF NOT EXISTS pending_price_milk    NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_curd    NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_butter  NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_ghee    NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_lassi   NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_paneer  NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_jaggery NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_khand   NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_oil     NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_atta    NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_price_burfi   NUMERIC(10,2),
    ADD COLUMN IF NOT EXISTS pending_effective_from DATE;

-- Partial index: the sweeper only ever looks for rows with something queued,
-- and that's a small minority of routes.
CREATE INDEX IF NOT EXISTS idx_routes_pending_prices
    ON routes (pending_effective_from)
    WHERE pending_effective_from IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_routes_pending_prices;
ALTER TABLE routes
    DROP COLUMN IF EXISTS pending_price_milk,
    DROP COLUMN IF EXISTS pending_price_curd,
    DROP COLUMN IF EXISTS pending_price_butter,
    DROP COLUMN IF EXISTS pending_price_ghee,
    DROP COLUMN IF EXISTS pending_price_lassi,
    DROP COLUMN IF EXISTS pending_price_paneer,
    DROP COLUMN IF EXISTS pending_price_jaggery,
    DROP COLUMN IF EXISTS pending_price_khand,
    DROP COLUMN IF EXISTS pending_price_oil,
    DROP COLUMN IF EXISTS pending_price_atta,
    DROP COLUMN IF EXISTS pending_price_burfi,
    DROP COLUMN IF EXISTS pending_effective_from;
-- +goose StatementEnd
