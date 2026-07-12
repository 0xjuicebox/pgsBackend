-- +goose Up

-- 1. Create the new Subscriptions table (The "Alarm Clock" Engine)
CREATE TABLE subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID REFERENCES customers(id) ON DELETE CASCADE UNIQUE NOT NULL,

    -- Scheduling logic: 0=Sunday, 1=Monday, 2=Tuesday, etc.
    -- Default is 'daily' and everyday is active.
    schedule_type TEXT NOT NULL DEFAULT 'daily',
    active_days INT[] DEFAULT '{0,1,2,3,4,5,6}',

    -- NEW: The starting point for 'alternate' day math (e.g., every 2 days)
    anchor_date DATE DEFAULT CURRENT_DATE NOT NULL,

    -- The product quantities migrated from the customers table
    default_milk_qty INT DEFAULT 0,
    default_curd_qty INT DEFAULT 0,
    default_butter_qty INT DEFAULT 0,
    default_ghee_qty INT DEFAULT 0,
    default_lassi_qty INT DEFAULT 0,
    default_paneer_qty INT DEFAULT 0,
    default_jaggery_qty INT DEFAULT 0,
    default_khand_qty INT DEFAULT 0,
    default_oil_qty INT DEFAULT 0,
    default_atta_qty INT DEFAULT 0,
    default_burfi_qty INT DEFAULT 0,

    created_at TIMESTAMPTZ DEFAULT NOW() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW() NOT NULL
);

-- 2. Add the Upsert constraint to order_overrides to allow multiple edits
ALTER TABLE order_overrides
ADD CONSTRAINT unique_customer_target_date UNIQUE (customer_id, target_date);

-- 3. Trim the logistics data off the identity (customers) table
ALTER TABLE customers
DROP COLUMN IF EXISTS default_milk_qty,
DROP COLUMN IF EXISTS default_curd_qty,
DROP COLUMN IF EXISTS default_butter_qty,
DROP COLUMN IF EXISTS default_ghee_qty,
DROP COLUMN IF EXISTS default_lassi_qty,
DROP COLUMN IF EXISTS default_paneer_qty,
DROP COLUMN IF EXISTS default_jaggery_qty,
DROP COLUMN IF EXISTS default_khand_qty,
DROP COLUMN IF EXISTS default_oil_qty,
DROP COLUMN IF EXISTS default_atta_qty,
DROP COLUMN IF EXISTS default_burfi_qty;


-- +goose Down

-- 1. Re-add columns to customers table if we need to rollback
ALTER TABLE customers
ADD COLUMN default_milk_qty INT DEFAULT 0,
ADD COLUMN default_curd_qty INT DEFAULT 0,
ADD COLUMN default_butter_qty INT DEFAULT 0,
ADD COLUMN default_ghee_qty INT DEFAULT 0,
ADD COLUMN default_lassi_qty INT DEFAULT 0,
ADD COLUMN default_paneer_qty INT DEFAULT 0,
ADD COLUMN default_jaggery_qty INT DEFAULT 0,
ADD COLUMN default_khand_qty INT DEFAULT 0,
ADD COLUMN default_oil_qty INT DEFAULT 0,
ADD COLUMN default_atta_qty INT DEFAULT 0,
ADD COLUMN default_burfi_qty INT DEFAULT 0;

-- 2. Remove the constraint
ALTER TABLE order_overrides
DROP CONSTRAINT IF EXISTS unique_customer_target_date;

-- 3. Drop the subscriptions table
DROP TABLE IF EXISTS subscriptions;
