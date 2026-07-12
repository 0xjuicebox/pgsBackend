-- +goose Up

CREATE TABLE delivery_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID REFERENCES customers(id) ON DELETE CASCADE NOT NULL,
    route_id UUID REFERENCES routes(id) ON DELETE SET NULL,
    delivery_date DATE NOT NULL,
    status TEXT NOT NULL, -- e.g., 'DELIVERED', 'SKIPPED', 'FAILED'

    -- The absolute source of truth for billing
    delivered_milk_qty INT DEFAULT 0,
    delivered_curd_qty INT DEFAULT 0,
    delivered_butter_qty INT DEFAULT 0,
    delivered_ghee_qty INT DEFAULT 0,
    delivered_lassi_qty INT DEFAULT 0,
    delivered_paneer_qty INT DEFAULT 0,
    delivered_jaggery_qty INT DEFAULT 0,
    delivered_khand_qty INT DEFAULT 0,
    delivered_oil_qty INT DEFAULT 0,
    delivered_atta_qty INT DEFAULT 0,
    delivered_burfi_qty INT DEFAULT 0,

    created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,

    -- A customer can only have one final delivery state per day
    CONSTRAINT unique_daily_delivery UNIQUE(customer_id, delivery_date)
);

-- +goose Down
DROP TABLE IF EXISTS delivery_logs;
