-- +goose Up
CREATE TABLE IF NOT EXISTS customers (
  id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
  name TEXT NOT NULL,
  phone_number TEXT UNIQUE NOT NULL,
  house_address TEXT,
  geo_latitude TEXT,
  geo_longitude TEXT,
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
  is_active BOOLEAN DEFAULT TRUE
);

CREATE TABLE IF NOT EXISTS order_overrides (
  id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
  customer_id UUID REFERENCES customers(id),
  target_date DATE NOT NULL,
  new_milk_qty INT DEFAULT 0,
  new_paneer_qty INT DEFAULT 0,
  new_butter_qty INT DEFAULT 0,
  new_ghee_qty INT DEFAULT 0,
  new_lassi_qty INT DEFAULT 0,
  new_curd_qty INT DEFAULT 0,
  new_jaggery_qty INT DEFAULT 0,
  new_khand_qty INT DEFAULT 0,
  new_oil_qty INT DEFAULT 0,
  new_atta_qty INT DEFAULT 0,
  new_burfi_qty INT DEFAULT 0,
  created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS system_config (
  id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
  nightly_cutoff_time TEXT NOT NULL,

  -- Flattened Price List (Using NUMERIC for currency precision)
  price_milk NUMERIC(10,2) DEFAULT 0,
  price_curd NUMERIC(10,2) DEFAULT 0,
  price_butter NUMERIC(10,2) DEFAULT 0,
  price_ghee NUMERIC(10,2) DEFAULT 0,
  price_lassi NUMERIC(10,2) DEFAULT 0,
  price_paneer NUMERIC(10,2) DEFAULT 0,
  price_jaggery NUMERIC(10,2) DEFAULT 0,
  price_khand NUMERIC(10,2) DEFAULT 0,
  price_oil NUMERIC(10,2) DEFAULT 0,
  price_atta NUMERIC(10,2) DEFAULT 0,
  price_burfi NUMERIC(10,2) DEFAULT 0,

  updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS system_config;
DROP TABLE IF EXISTS order_overrides;
DROP TABLE IF EXISTS customers;
