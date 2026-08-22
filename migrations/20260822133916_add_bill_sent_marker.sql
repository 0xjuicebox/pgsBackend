-- +goose Up
-- +goose StatementBegin

-- Durable record of whether a customer's bill actually reached them.
--
-- THE PROBLEM THIS SOLVES
--
-- notifyFreshInvoices decided what to send by selecting invoices created in
-- the last 60 seconds, then looping serially with a 10-minute budget. Each
-- iteration makes a Razorpay call and a Twilio call — roughly 1.5 seconds.
--
-- At 150 customers that finishes in four minutes and the design holds. At 700
-- it needs seventeen, so the context expires around customer 400. The rest get
-- an invoice row and nothing else: no WhatsApp, no payment link. They then
-- enter the dunning cycle as unpaid, get chased daily for a bill they never
-- received, and are suspended on the 6th.
--
-- Nothing anywhere reported that it had stopped.
--
-- The 60-second window was the deeper flaw: it made "has this been sent?" a
-- question about wall-clock timing rather than a fact about the invoice. A
-- restart, a retry, or a slow month all produced the wrong answer. This column
-- makes it a fact, which also makes the send resumable — a second run picks up
-- exactly the invoices the first one missed.

ALTER TABLE invoices
    ADD COLUMN IF NOT EXISTS bill_sent_at TIMESTAMPTZ;

-- Backfill: every invoice that already exists has, as far as anyone knows,
-- been sent. Leaving these NULL would make the first run after this migration
-- re-send every historical bill.
UPDATE invoices SET bill_sent_at = created_at WHERE bill_sent_at IS NULL;

-- The sender's hot query is "invoices for month X with no bill_sent_at".
-- Partial index because the interesting rows are a vanishing fraction of the
-- table — everything sent successfully is excluded forever.
CREATE INDEX IF NOT EXISTS idx_invoices_unsent
    ON invoices (billing_month)
    WHERE bill_sent_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_invoices_unsent;
ALTER TABLE invoices DROP COLUMN IF EXISTS bill_sent_at;

-- +goose StatementEnd
