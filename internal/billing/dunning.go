package billing

// Dunning — the automated chase for unpaid bills.
//
// THE CYCLE, for the month that just ended:
//
//	 1st       generate invoices, send bills
//	 2nd–5th   one reminder per unpaid invoice per day
//	 6th       suspend: is_active = false, status = 'suspended', notice sent
//	 any time  payment resumes the customer instantly, no admin step
//
// WHY THIS RUNS HOURLY RATHER THAN ONCE A DAY
//
// A daily timer only fires if the process is alive at that exact moment. A
// deploy, a crash, or a DigitalOcean restart at 00:59 on the 1st would skip
// the entire billing run for the month, and nothing would notice — the same
// silent-failure shape as the ₹0 invoices and the unseeded system_config.
//
// Running hourly and making every phase idempotent means a missed hour is
// caught by the next one. The guards are:
//
//	GENERATE  a row in dunning_runs for (today, 'GENERATE')
//	REMIND    invoices.last_reminder_on = CURRENT_DATE
//	SUSPEND   customers.status already 'suspended'
//
// So the sweeper can run sixty times a day and each customer still gets
// exactly one bill and one reminder.
//
// WHY CUSTOMERS KEEP RECEIVING DELIVERIES UNTIL THE 6TH
//
// A deliberate business decision: cutting someone off on the 1st over a bill
// they may not have seen yet turns a billing delay into a lost customer.
// Those 1st-to-5th deliveries bill into the following month as normal.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
)

const (
	// Day of month on which invoices are generated and bills go out.
	generateDay = 1
	// Last day on which a reminder is sent. On the following day, unpaid
	// accounts are suspended.
	lastReminderDay = 5
	// Day of month on which unpaid accounts are suspended.
	suspendDay = 6
)

// StartDunningSweeper runs the billing chase on an hourly ticker.
//
// Start from main.go after the pool is ready:
//
//	go br.StartDunningSweeper()
//
// Runs once immediately so a deploy during the billing window catches up
// rather than waiting up to an hour.
func (br BillingResource) StartDunningSweeper() {
	// A short delay before the first run: at boot the pool is warm but
	// outbound Twilio and Razorpay calls compete with whatever else is
	// starting, and nothing here is urgent to the second.
	time.Sleep(30 * time.Second)

	br.runDunning()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		br.runDunning()
	}
}

func (br BillingResource) runDunning() {
	// Generous: a generate phase makes one Razorpay call and one Twilio call
	// per customer, serially.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		fmt.Printf("⚠️ dunning: cannot load Asia/Kolkata: %v\n", err)
		return
	}
	now := time.Now().In(loc)
	day := now.Day()

	// The month being chased is always the one that just ended. On 5 Sept
	// we are chasing August. Computed by stepping back to the previous
	// month from the first of this one, which avoids the classic bug of
	// subtracting a month from the 31st and landing two months back.
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	targetMonth := firstOfThisMonth.AddDate(0, 0, -1).Format("2006-01")

	switch {
	case day == generateDay:
		br.dunningGenerate(ctx, now, targetMonth)
	case day > generateDay && day <= lastReminderDay:
		br.dunningRemind(ctx, now, targetMonth)
	case day >= suspendDay:
		// Suspension runs on the 6th and every day after, so a customer who
		// somehow escaped the 6th (server down all day) is still caught.
		br.dunningSuspend(ctx, now, targetMonth)
	}
}

// -------------------------------------------------------------------------
// Phase 1 — generate and bill
// -------------------------------------------------------------------------

func (br BillingResource) dunningGenerate(ctx context.Context, now time.Time, month string) {
	today := now.Format("2006-01-02")

	// Claim the day before doing any work. The unique constraint on
	// (run_date, phase) means a second sweeper — or the same one after a
	// restart — finds the row already present and stops. Without this,
	// restarting the process on the 1st would re-run the whole billing
	// cycle; ON CONFLICT DO NOTHING protects the invoices themselves, but
	// notifyFreshInvoices filters on created_at within the last minute and
	// would happily message everyone a second time.
	runID, _ := uuid.NewV7()
	tag, err := br.DB.Exec(ctx, `
		INSERT INTO dunning_runs (id, run_date, phase, billing_month)
		VALUES ($1, $2::date, 'GENERATE', $3)
		ON CONFLICT (run_date, phase) DO NOTHING
	`, runID, today, month)
	if err != nil {
		fmt.Printf("⚠️ dunning generate: could not claim run: %v\n", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // already generated today
	}

	tallies, err := br.aggregate(ctx, month)
	if err != nil {
		br.noteRun(ctx, today, "GENERATE", 0, "aggregate failed: "+err.Error())
		fmt.Printf("⚠️ dunning generate: aggregate failed for %s: %v\n", month, err)
		return
	}

	tx, err := br.DB.Begin(ctx)
	if err != nil {
		br.noteRun(ctx, today, "GENERATE", 0, "begin failed: "+err.Error())
		return
	}
	defer tx.Rollback(ctx)

	generated := 0
	for _, t := range tallies {
		if t.TotalAmount <= 0 {
			continue
		}
		invID, _ := uuid.NewV7()
		breakdown, _ := json.Marshal(t.Breakdown)
		tag, err := tx.Exec(ctx, `
			INSERT INTO invoices (id, customer_id, billing_month, total_amount, breakdown)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (customer_id, billing_month) DO NOTHING
		`, invID, t.CustomerId, month, t.TotalAmount, breakdown)
		if err != nil {
			fmt.Printf("⚠️ dunning generate: insert failed for %s: %v\n", t.CustomerId, err)
			br.noteRun(ctx, today, "GENERATE", 0, "insert failed: "+err.Error())
			return
		}
		if tag.RowsAffected() > 0 {
			generated++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		br.noteRun(ctx, today, "GENERATE", 0, "commit failed: "+err.Error())
		fmt.Printf("⚠️ dunning generate: commit failed: %v\n", err)
		return
	}

	fmt.Printf("🧾 dunning: generated %d invoice(s) for %s\n", generated, month)
	br.noteRun(ctx, today, "GENERATE", generated, "")

	if br.WhatsApp != nil && generated > 0 {
		// Reuses the same notifier as the manual admin Generate, so the
		// automated and manual paths cannot drift in what a customer receives.
		br.notifyFreshInvoices(month, tallies)
	}
}

// -------------------------------------------------------------------------
// Phase 2 — daily reminders
// -------------------------------------------------------------------------

type dunningTarget struct {
	InvoiceID string
	Phone     string
	Name      string
	Total     float64
}

func (br BillingResource) dunningRemind(ctx context.Context, now time.Time, month string) {
	today := now.Format("2006-01-02")

	// last_reminder_on is the idempotency guard: an invoice already reminded
	// today is excluded, so the hourly ticker sends at most one per day.
	rows, err := br.DB.Query(ctx, `
		SELECT i.id::text, c.phone_number, c.name, i.total_amount
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.billing_month = $1
		  AND i.status NOT LIKE 'PAID%'
		  AND (i.last_reminder_on IS NULL OR i.last_reminder_on < CURRENT_DATE)
	`, month)
	if err != nil {
		fmt.Printf("⚠️ dunning remind: query failed: %v\n", err)
		return
	}

	var targets []dunningTarget
	for rows.Next() {
		var t dunningTarget
		if err := rows.Scan(&t.InvoiceID, &t.Phone, &t.Name, &t.Total); err != nil {
			fmt.Printf("⚠️ dunning remind: scan failed: %v\n", err)
			continue
		}
		targets = append(targets, t)
	}
	rows.Close()

	if len(targets) == 0 {
		return
	}

	sent := 0
	for _, t := range targets {
		// Mark BEFORE sending. A send that fails is logged and retried
		// tomorrow; marking after would risk a Twilio timeout on a message
		// that actually went out, and the customer getting the same reminder
		// every hour for the rest of the day. Under-messaging beats
		// over-messaging when the alternative is harassment.
		if _, err := br.DB.Exec(ctx, `
			UPDATE invoices
			SET last_reminder_on = CURRENT_DATE, reminder_count = reminder_count + 1
			WHERE id = $1::uuid
		`, t.InvoiceID); err != nil {
			fmt.Printf("⚠️ dunning remind: could not mark invoice %s: %v\n", t.InvoiceID, err)
			continue
		}

		if br.WhatsApp == nil || br.Templates.PaymentReminder == "" {
			continue
		}
		if err := br.WhatsApp.SendPaymentReminder(
			t.Phone, br.Templates.PaymentReminder,
			t.Name, labelForMonth(month), formatAmount(t.Total), t.InvoiceID,
		); err != nil {
			fmt.Printf("⚠️ dunning remind: send failed for %s: %v\n", t.Phone, err)
			continue
		}
		sent++
	}

	fmt.Printf("🔔 dunning: sent %d payment reminder(s) for %s\n", sent, month)
	br.noteRun(ctx, today, "REMIND", sent, "")
}

// -------------------------------------------------------------------------
// Phase 3 — suspension
// -------------------------------------------------------------------------

func (br BillingResource) dunningSuspend(ctx context.Context, now time.Time, month string) {
	today := now.Format("2006-01-02")

	// status <> 'suspended' is the idempotency guard here. Anyone already
	// suspended is skipped, so this can run every hour from the 6th onward
	// without re-notifying.
	//
	// Customers who are 'disabled' (their own holiday pause) are still
	// suspended if they owe money — otherwise pausing would be a way to
	// dodge the chase. The status change is what matters; they're already
	// not receiving deliveries.
	rows, err := br.DB.Query(ctx, `
		SELECT i.id::text, c.phone_number, c.name, i.total_amount
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.billing_month = $1
		  AND i.status NOT LIKE 'PAID%'
		  AND c.status <> 'suspended'
	`, month)
	if err != nil {
		fmt.Printf("⚠️ dunning suspend: query failed: %v\n", err)
		return
	}

	var targets []dunningTarget
	for rows.Next() {
		var t dunningTarget
		if err := rows.Scan(&t.InvoiceID, &t.Phone, &t.Name, &t.Total); err != nil {
			continue
		}
		targets = append(targets, t)
	}
	rows.Close()

	if len(targets) == 0 {
		return
	}

	suspended := 0
	for _, t := range targets {
		// Routing is deliberately untouched — no stop_order reset, no
		// route_id clear. A suspension is expected to end, and preserving
		// the customer's place on the round makes resuming a single flag
		// flip rather than an admin re-routing them from scratch.
		//
		// status_before_suspension captures where to put them back. A
		// customer who was 'disabled' (their own holiday pause) and owed
		// money must return to 'disabled' when they pay, not to 'active' —
		// settling a bill is not a request to restart deliveries.
		if _, err := br.DB.Exec(ctx, `
			UPDATE customers c
			SET is_active = false,
			    status_before_suspension = c.status,
			    status = 'suspended',
			    suspended_for_invoice_id = $1::uuid,
			    updated_at = NOW()
			FROM invoices i
			WHERE i.id = $1::uuid AND c.id = i.customer_id
		`, t.InvoiceID); err != nil {
			fmt.Printf("⚠️ dunning suspend: could not suspend for invoice %s: %v\n", t.InvoiceID, err)
			continue
		}
		if _, err := br.DB.Exec(ctx,
			`UPDATE invoices SET suspended_at = NOW() WHERE id = $1::uuid`, t.InvoiceID); err != nil {
			fmt.Printf("⚠️ dunning suspend: could not stamp invoice %s: %v\n", t.InvoiceID, err)
		}
		suspended++

		if br.WhatsApp == nil || br.Templates.Suspension == "" {
			continue
		}
		if err := br.WhatsApp.SendSuspensionNotice(
			t.Phone, br.Templates.Suspension,
			t.Name, labelForMonth(month), formatAmount(t.Total), t.InvoiceID,
		); err != nil {
			fmt.Printf("⚠️ dunning suspend: notice failed for %s: %v\n", t.Phone, err)
		}
	}

	fmt.Printf("⏸️  dunning: suspended %d account(s) over unpaid %s bills\n", suspended, month)
	br.noteRun(ctx, today, "SUSPEND", suspended, "")
}

// -------------------------------------------------------------------------
// Resume on payment
// -------------------------------------------------------------------------

// RunPhase executes one dunning phase directly, for an explicit month,
// bypassing the day-of-month routing in runDunning.
//
// This exists so the cycle can be tested. The phases are date-gated by design
// — generate on the 1st, remind to the 5th, suspend on the 6th — which means
// the only way to exercise them otherwise is to wait for the calendar or
// falsify rows in the database and restart the server. Neither proves the
// resume path at all.
//
// The idempotency guards still apply: a second GENERATE on the same day is
// still refused by dunning_runs, a REMIND still respects last_reminder_on.
// So this is a way to run the real code early, not a way to run different
// code.
//
// phase must be "GENERATE", "REMIND" or "SUSPEND". month is "2006-01".
func (br BillingResource) RunPhase(ctx context.Context, phase, month string) error {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return fmt.Errorf("load Asia/Kolkata: %w", err)
	}
	now := time.Now().In(loc)

	switch phase {
	case "GENERATE":
		br.dunningGenerate(ctx, now, month)
	case "REMIND":
		br.dunningRemind(ctx, now, month)
	case "SUSPEND":
		br.dunningSuspend(ctx, now, month)
	default:
		return fmt.Errorf("unknown phase %q", phase)
	}
	return nil
}

// ResumeIfSuspended lifts a suspension when its invoice is paid.
//
// Wired into payment.Service.OnPaid from main.go, so it fires for both the
// Razorpay webhook and an admin marking a bill PAID_CASH. No admin approval:
// the customer was suspended by a machine for a specific reason, and that
// reason no longer holds.
//
// Returns resumed=true only when the customer was actually put back into
// service, so the caller can send a welcome-back message. A customer restored
// to 'disabled' is not resumed — their pause is still in force — and gets an
// ordinary receipt instead.
func (br BillingResource) ResumeIfSuspended(ctx context.Context, invoiceID string) (resumed bool, err error) {
	// Only lifts a suspension caused by THIS invoice. A customer suspended
	// over August who pays a stray July bill stays suspended — paying the
	// wrong month shouldn't restore service.
	//
	// The status they return to is the one they held before suspension, not
	// 'active'. Someone who paused for a holiday, was then suspended for an
	// unpaid bill, and pays it, must go back to 'disabled': they are still on
	// holiday. Forcing them to 'active' would deliver milk to an empty house
	// because they settled a debt.
	//
	// COALESCE covers rows suspended before this column existed, and any
	// future path that forgets to set it — 'active' is the safer default
	// there, since the alternative is a customer stuck unable to receive
	// deliveries they've paid for.
	var newStatus string
	err = br.DB.QueryRow(ctx, `
		UPDATE customers
		SET status = COALESCE(status_before_suspension, 'active'),
		    is_active = (COALESCE(status_before_suspension, 'active') = 'active'),
		    status_before_suspension = NULL,
		    suspended_for_invoice_id = NULL,
		    updated_at = NOW()
		WHERE status = 'suspended'
		  AND suspended_for_invoice_id = $1::uuid
		RETURNING status
	`, invoiceID).Scan(&newStatus)

	if err == pgx.ErrNoRows {
		// Nobody was suspended for this invoice — the common case.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return newStatus == "active", nil
}

// noteRun records the outcome of a phase. Best-effort: a failure to write the
// audit row must never abort the work it describes.
func (br BillingResource) noteRun(ctx context.Context, day, phase string, affected int, note string) {
	runID, _ := uuid.NewV7()
	_, _ = br.DB.Exec(ctx, `
		INSERT INTO dunning_runs (id, run_date, phase, billing_month, affected, note)
		VALUES ($1, $2::date, $3, '', $4, NULLIF($5,''))
		ON CONFLICT (run_date, phase) DO UPDATE
		SET affected = EXCLUDED.affected, note = EXCLUDED.note
	`, runID, day, phase, affected, note)
}
