// Command dunningtest drives the full dunning cycle against throwaway
// fixtures and asserts the resulting database state.
//
//	go run ./cmd/dunningtest
//	go run ./cmd/dunningtest -keep        # leave fixtures for inspection
//	go run ./cmd/dunningtest -no-whatsapp # skip Twilio sends
//
// # WHY A HARNESS RATHER THAN WAITING FOR THE CALENDAR
//
// Every dunning phase is date-gated: generate on the 1st, remind to the 5th,
// suspend on the 6th. Testing it by waiting means one attempt per month, and
// the resume-on-payment path can't be reached at all without a real payment.
//
// The alternative — editing rows and restarting the server until each phase
// fires — proves less than it looks like it does, because it exercises the
// date arithmetic rather than the billing logic, and it leaves the database
// in a half-modified state that's easy to mistake for a passing test.
//
// This calls BillingResource.RunPhase directly with an explicit month. The
// phases themselves are unmodified: the same idempotency guards, the same
// SQL, the same notifications. Only the calendar is bypassed.
//
// WHAT IT COVERS
//
//  1. GENERATE creates an invoice with the correct total from delivery logs
//  2. GENERATE is idempotent — a second run creates nothing
//  3. REMIND marks last_reminder_on and increments reminder_count
//  4. REMIND is idempotent within a day
//  5. SUSPEND sets status='suspended', is_active=false
//  6. SUSPEND preserves route_id and stop_order
//  7. SUSPEND records status_before_suspension
//  8. Payment resumes an active customer back to active
//  9. Payment does NOT resume a customer who was paused before suspension
//  10. A paid invoice is never reminded or suspended
//
// Case 9 is the one worth having a harness for. A customer who paused for a
// holiday, owed last month's bill, was suspended, then paid, must go back to
// 'disabled' — not 'active'. Settling a debt is not a request to restart
// deliveries, and getting this wrong delivers milk to an empty house.
//
// # SAFETY
//
// Fixtures use phone numbers +910000000001 / +910000000002 and a billing
// month two years in the future, so they cannot collide with real data or be
// picked up by real reporting. Cleanup runs on exit unless -keep is passed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/billing"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/payment"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	// Two years out: past any real billing month, and clearly synthetic to
	// anyone who stumbles across it in the database.
	testMonth = "2098-11"
	// Dates inside testMonth for the delivery logs.
	testDatePrefix = "2098-11-"

	activePhone = "+910000000001" // suspended from 'active', should resume
	pausedPhone = "+910000000002" // suspended from 'disabled', should NOT resume
	paidPhone   = "+910000000003" // pays immediately, should never be chased

	fixtureTag = "DUNNINGTEST FIXTURE — safe to delete"
)

var (
	flagKeep       = flag.Bool("keep", false, "skip cleanup so fixture rows can be inspected")
	flagNoWhatsApp = flag.Bool("no-whatsapp", false, "run without Twilio so no real messages are sent")
)

type harness struct {
	db      *pgxpool.Pool
	br      billing.BillingResource
	results []result
}

type result struct {
	name   string
	passed bool
	detail string
}

func (h *harness) expect(name string, cond bool, got, want string) {
	if cond {
		h.results = append(h.results, result{name, true, got})
		fmt.Printf("  \033[32mPASS\033[0m  %-34s %s\n", name, got)
		return
	}
	h.results = append(h.results, result{name, false, fmt.Sprintf("got %s, want %s", got, want)})
	fmt.Printf("  \033[31mFAIL\033[0m  %-34s got %s, want %s\n", name, got, want)
}

func (h *harness) fail(name, detail string) {
	h.results = append(h.results, result{name, false, detail})
	fmt.Printf("  \033[31mFAIL\033[0m  %-34s %s\n", name, detail)
}

// -------------------------------------------------------------------------
// Fixtures
// -------------------------------------------------------------------------

type fixture struct {
	customerID uuid.UUID
	phone      string
	status     string
	routeID    *uuid.UUID
}

// newCustomer creates a fixture customer with a route and stop order, so the
// harness can prove suspension leaves routing intact.
func (h *harness) newCustomer(ctx context.Context, phone, status string, routeID *uuid.UUID) (fixture, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return fixture{}, err
	}

	isActive := status == "active"
	if _, err := h.db.Exec(ctx, `
		INSERT INTO customers (id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status)
		VALUES ($1, $2, $3, 'Fixture address, do not deliver', '0', '0', $4, $5)
	`, id, fixtureTag, phone, isActive, status); err != nil {
		return fixture{}, err
	}

	// A subscription with a route and stop_order. Suspension must not touch
	// either — unlike FinalInvoice, which zeroes stop_order because a leaver
	// is gone for good.
	if _, err := h.db.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, slot, route_id, stop_order, default_milk_qty,
		                           schedule_type, active_days)
		VALUES ($1, 'morning', $2, 7, 2000, 'daily', ARRAY[0,1,2,3,4,5,6])
	`, id, routeID); err != nil {
		return fixture{}, err
	}

	return fixture{customerID: id, phone: phone, status: status, routeID: routeID}, nil
}

// addDeliveryLogs writes n delivered logs inside testMonth with real price
// snapshots. Billing reads only from these — never from subscriptions — so
// this is what determines the invoice total.
func (h *harness) addDeliveryLogs(ctx context.Context, f fixture, n int, milkMl int, pricePerMl float64) (float64, error) {
	var expected float64
	for i := 1; i <= n; i++ {
		date := fmt.Sprintf("%s%02d", testDatePrefix, i)
		if _, err := h.db.Exec(ctx, `
			INSERT INTO delivery_logs (customer_id, route_id, delivery_date, slot, status,
			                           delivered_milk_qty, unit_price_milk)
			VALUES ($1, $2, $3::date, 'morning', 'DELIVERED', $4, $5)
			ON CONFLICT (customer_id, delivery_date, slot) DO NOTHING
		`, f.customerID, f.routeID, date, milkMl, pricePerMl); err != nil {
			return 0, err
		}
		expected += float64(milkMl) * pricePerMl
	}
	return expected, nil
}

// newRoute creates a throwaway route so subscriptions have something real to
// point at. Named distinctly so it's obvious in the admin UI if cleanup fails.
func (h *harness) newRoute(ctx context.Context) (*uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	if _, err := h.db.Exec(ctx,
		`INSERT INTO routes (id, name) VALUES ($1, $2)`, id, fixtureTag); err != nil {
		return nil, err
	}
	return &id, nil
}

func (h *harness) cleanup(ctx context.Context) error {
	// dunning_runs rows are keyed by (run_date, phase) and would otherwise
	// make a second run today think GENERATE had already happened.
	if _, err := h.db.Exec(ctx,
		`DELETE FROM dunning_runs WHERE billing_month = $1 OR run_date = CURRENT_DATE`, testMonth); err != nil {
		return err
	}
	// invoices, subscriptions and delivery_logs all cascade from customers.
	if _, err := h.db.Exec(ctx,
		`DELETE FROM customers WHERE phone_number IN ($1,$2,$3)`,
		activePhone, pausedPhone, paidPhone); err != nil {
		return err
	}
	_, err := h.db.Exec(ctx, `DELETE FROM routes WHERE name = $1`, fixtureTag)
	return err
}

// -------------------------------------------------------------------------
// Reads
// -------------------------------------------------------------------------

type invoiceState struct {
	id            string
	total         float64
	status        string
	reminderCount int
	lastReminder  *string
	suspendedAt   *string
}

func (h *harness) invoiceFor(ctx context.Context, f fixture) (invoiceState, error) {
	var st invoiceState
	err := h.db.QueryRow(ctx, `
		SELECT id::text, total_amount, status, COALESCE(reminder_count,0),
		       TO_CHAR(last_reminder_on,'YYYY-MM-DD'), TO_CHAR(suspended_at,'YYYY-MM-DD')
		FROM invoices WHERE customer_id = $1 AND billing_month = $2
	`, f.customerID, testMonth).Scan(&st.id, &st.total, &st.status,
		&st.reminderCount, &st.lastReminder, &st.suspendedAt)
	return st, err
}

type customerState struct {
	status     string
	isActive   bool
	beforeSusp *string
	stopOrder  int
	routeID    *string
}

func (h *harness) customerFor(ctx context.Context, f fixture) (customerState, error) {
	var st customerState
	err := h.db.QueryRow(ctx, `
		SELECT c.status, c.is_active, c.status_before_suspension,
		       s.stop_order, s.route_id::text
		FROM customers c
		JOIN subscriptions s ON s.customer_id = c.id AND s.slot = 'morning'
		WHERE c.id = $1
	`, f.customerID).Scan(&st.status, &st.isActive, &st.beforeSusp, &st.stopOrder, &st.routeID)
	return st, err
}

func (h *harness) invoiceCount(ctx context.Context) int {
	var n int
	_ = h.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM invoices WHERE billing_month = $1`, testMonth).Scan(&n)
	return n
}

// -------------------------------------------------------------------------
// Entry point
// -------------------------------------------------------------------------

func main() {
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		fmt.Println("Notice: no .env found, reading from environment")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		exit("DATABASE_URL is not set.")
	}

	ctx := context.Background()
	connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		exit("bad DATABASE_URL: " + err.Error())
	}
	// Same IST pinning the server uses. Without it CURRENT_DATE here is the
	// UTC date and last_reminder_on comparisons drift for 5.5 hours a day.
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"

	db, err := pgxpool.NewWithConfig(connCtx, cfg)
	if err != nil {
		exit("could not connect: " + err.Error())
	}
	defer db.Close()
	if err := db.Ping(connCtx); err != nil {
		exit("database ping failed: " + err.Error())
	}

	// WhatsApp is real unless suppressed. The phases notify, so a run with
	// live Twilio credentials sends actual messages to the fixture numbers —
	// which are unassignable, so Twilio rejects them at validation.
	var whatsApp *notification.WhatsAppService
	if !*flagNoWhatsApp {
		whatsApp = notification.NewWhatsAppService()
	}

	pay := payment.New(db,
		os.Getenv("RAZORPAY_KEY_ID"),
		os.Getenv("RAZORPAY_KEY_SECRET"),
		os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
	)

	h := &harness{
		db: db,
		br: billing.BillingResource{
			DB:        db,
			WhatsApp:  whatsApp,
			Payment:   pay,
			Templates: notification.LoadTemplateSIDs(),
		},
	}

	fmt.Printf("\nDunning cycle test — billing month %s\n", testMonth)
	if *flagNoWhatsApp {
		fmt.Println("WhatsApp suppressed (-no-whatsapp)")
	}
	fmt.Println()

	// Always start clean: a previous -keep run would otherwise make GENERATE
	// look idempotent when it simply had nothing to do.
	if err := h.cleanup(ctx); err != nil {
		exit("pre-clean failed: " + err.Error())
	}

	teardown := func() {
		if *flagKeep {
			fmt.Printf("\n-keep: fixtures left in place. Phones %s / %s / %s, month %s.\n",
				activePhone, pausedPhone, paidPhone, testMonth)
			return
		}
		if err := h.cleanup(ctx); err != nil {
			fmt.Printf("\n⚠️  cleanup failed: %v\n", err)
			fmt.Printf("    By hand: DELETE FROM customers WHERE phone_number IN ('%s','%s','%s');\n",
				activePhone, pausedPhone, paidPhone)
			fmt.Printf("             DELETE FROM routes WHERE name = '%s';\n", fixtureTag)
			fmt.Printf("             DELETE FROM dunning_runs WHERE run_date = CURRENT_DATE;\n")
		}
	}

	// ---------------------------------------------------------------------
	// Setup
	// ---------------------------------------------------------------------

	routeID, err := h.newRoute(ctx)
	if err != nil {
		exit("route fixture failed: " + err.Error())
	}

	activeCust, err := h.newCustomer(ctx, activePhone, "active", routeID)
	if err != nil {
		exit("active customer fixture failed: " + err.Error())
	}
	pausedCust, err := h.newCustomer(ctx, pausedPhone, "disabled", routeID)
	if err != nil {
		exit("paused customer fixture failed: " + err.Error())
	}
	paidCust, err := h.newCustomer(ctx, paidPhone, "active", routeID)
	if err != nil {
		exit("paid customer fixture failed: " + err.Error())
	}

	// 20 deliveries of 2 L at ₹0.07/ml = ₹140/day = ₹2,800
	expectActive, err := h.addDeliveryLogs(ctx, activeCust, 20, 2000, 0.07)
	if err != nil {
		exit("delivery logs failed: " + err.Error())
	}
	if _, err := h.addDeliveryLogs(ctx, pausedCust, 10, 1000, 0.07); err != nil {
		exit("delivery logs failed: " + err.Error())
	}
	if _, err := h.addDeliveryLogs(ctx, paidCust, 5, 1000, 0.07); err != nil {
		exit("delivery logs failed: " + err.Error())
	}

	// ---------------------------------------------------------------------
	// Phase 1 — GENERATE
	// ---------------------------------------------------------------------

	fmt.Println("Phase 1 — generate")

	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail("generate/run", err.Error())
	}

	inv, err := h.invoiceFor(ctx, activeCust)
	if err != nil {
		h.fail("generate/invoice", "no invoice created: "+err.Error())
	} else {
		h.expect("generate/invoice_total",
			fmt.Sprintf("%.2f", inv.total) == fmt.Sprintf("%.2f", expectActive),
			fmt.Sprintf("₹%.2f", inv.total), fmt.Sprintf("₹%.2f", expectActive))
		h.expect("generate/invoice_status", inv.status == "PENDING", inv.status, "PENDING")
	}

	countAfterFirst := h.invoiceCount(ctx)
	h.expect("generate/all_three", countAfterFirst == 3,
		fmt.Sprintf("%d invoices", countAfterFirst), "3 invoices")

	// Second run: dunning_runs already holds today's GENERATE row, so this
	// must be a no-op. Without that guard a restart on the 1st would
	// re-message every customer.
	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail("generate/rerun", err.Error())
	}
	h.expect("generate/idempotent", h.invoiceCount(ctx) == countAfterFirst,
		fmt.Sprintf("%d invoices", h.invoiceCount(ctx)),
		fmt.Sprintf("%d (unchanged)", countAfterFirst))

	// One customer pays straight away — they must not be chased below.
	paidInv, err := h.invoiceFor(ctx, paidCust)
	if err != nil {
		h.fail("setup/paid_invoice", err.Error())
	} else if _, err := db.Exec(ctx,
		`UPDATE invoices SET status = 'PAID_CASH', paid_at = NOW() WHERE id = $1::uuid`, paidInv.id); err != nil {
		h.fail("setup/mark_paid", err.Error())
	}

	// ---------------------------------------------------------------------
	// Phase 2 — REMIND
	// ---------------------------------------------------------------------

	fmt.Println("\nPhase 2 — remind")

	if err := h.br.RunPhase(ctx, "REMIND", testMonth); err != nil {
		h.fail("remind/run", err.Error())
	}

	inv, err = h.invoiceFor(ctx, activeCust)
	if err != nil {
		h.fail("remind/read", err.Error())
	} else {
		h.expect("remind/marked", inv.lastReminder != nil,
			fmt.Sprintf("last_reminder_on=%s", derefOr(inv.lastReminder, "<nil>")), "today's date")
		h.expect("remind/counted", inv.reminderCount == 1,
			fmt.Sprintf("count=%d", inv.reminderCount), "1")
	}

	// Same day again: last_reminder_on excludes it. This is what stops the
	// hourly ticker sending 24 reminders a day.
	if err := h.br.RunPhase(ctx, "REMIND", testMonth); err != nil {
		h.fail("remind/rerun", err.Error())
	}
	inv, _ = h.invoiceFor(ctx, activeCust)
	h.expect("remind/idempotent", inv.reminderCount == 1,
		fmt.Sprintf("count=%d", inv.reminderCount), "1 (unchanged)")

	paidInv, _ = h.invoiceFor(ctx, paidCust)
	h.expect("remind/skips_paid", paidInv.reminderCount == 0,
		fmt.Sprintf("count=%d", paidInv.reminderCount), "0")

	// ---------------------------------------------------------------------
	// Phase 3 — SUSPEND
	// ---------------------------------------------------------------------

	fmt.Println("\nPhase 3 — suspend")

	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail("suspend/run", err.Error())
	}

	cs, err := h.customerFor(ctx, activeCust)
	if err != nil {
		h.fail("suspend/read", err.Error())
	} else {
		h.expect("suspend/status", cs.status == "suspended", cs.status, "suspended")
		h.expect("suspend/inactive", !cs.isActive,
			fmt.Sprintf("is_active=%v", cs.isActive), "false")
		// The point of not clearing routing: resuming should be one flag
		// flip, not an admin re-routing them from scratch.
		h.expect("suspend/keeps_stop_order", cs.stopOrder == 7,
			fmt.Sprintf("stop_order=%d", cs.stopOrder), "7")
		h.expect("suspend/keeps_route", cs.routeID != nil,
			fmt.Sprintf("route_id=%s", derefOr(cs.routeID, "<nil>")), "unchanged")
		h.expect("suspend/records_prior", derefOr(cs.beforeSusp, "") == "active",
			fmt.Sprintf("before=%s", derefOr(cs.beforeSusp, "<nil>")), "active")
	}

	ps, err := h.customerFor(ctx, pausedCust)
	if err != nil {
		h.fail("suspend/paused_read", err.Error())
	} else {
		h.expect("suspend/paused_suspended", ps.status == "suspended", ps.status, "suspended")
		h.expect("suspend/paused_records_prior", derefOr(ps.beforeSusp, "") == "disabled",
			fmt.Sprintf("before=%s", derefOr(ps.beforeSusp, "<nil>")), "disabled")
	}

	pc, err := h.customerFor(ctx, paidCust)
	if err != nil {
		h.fail("suspend/paid_read", err.Error())
	} else {
		h.expect("suspend/skips_paid", pc.status == "active", pc.status, "active")
	}

	// Running again must not re-notify; status is already 'suspended'.
	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail("suspend/rerun", err.Error())
	}
	cs, _ = h.customerFor(ctx, activeCust)
	h.expect("suspend/idempotent", cs.status == "suspended", cs.status, "suspended")

	// ---------------------------------------------------------------------
	// Phase 4 — resume on payment
	// ---------------------------------------------------------------------

	fmt.Println("\nPhase 4 — resume on payment")

	activeInv, err := h.invoiceFor(ctx, activeCust)
	if err != nil {
		h.fail("resume/read_invoice", err.Error())
	} else {
		if _, err := db.Exec(ctx,
			`UPDATE invoices SET status = 'PAID_ONLINE', paid_at = NOW() WHERE id = $1::uuid`, activeInv.id); err != nil {
			h.fail("resume/mark_paid", err.Error())
		}
		resumed, err := h.br.ResumeIfSuspended(ctx, activeInv.id)
		if err != nil {
			h.fail("resume/call", err.Error())
		} else {
			h.expect("resume/reported", resumed, fmt.Sprintf("resumed=%v", resumed), "true")
		}
		cs, _ = h.customerFor(ctx, activeCust)
		h.expect("resume/status", cs.status == "active", cs.status, "active")
		h.expect("resume/active", cs.isActive, fmt.Sprintf("is_active=%v", cs.isActive), "true")
		h.expect("resume/clears_prior", cs.beforeSusp == nil,
			fmt.Sprintf("before=%s", derefOr(cs.beforeSusp, "<nil>")), "<nil>")
	}

	// The case this harness exists for. A customer who paused for a holiday,
	// was suspended over an unpaid bill, and then paid, must go back to
	// 'disabled'. Returning them to 'active' would deliver milk to an empty
	// house because they settled a debt.
	pausedInv, err := h.invoiceFor(ctx, pausedCust)
	if err != nil {
		h.fail("resume/paused_read", err.Error())
	} else {
		if _, err := db.Exec(ctx,
			`UPDATE invoices SET status = 'PAID_ONLINE', paid_at = NOW() WHERE id = $1::uuid`, pausedInv.id); err != nil {
			h.fail("resume/paused_mark_paid", err.Error())
		}
		resumed, err := h.br.ResumeIfSuspended(ctx, pausedInv.id)
		if err != nil {
			h.fail("resume/paused_call", err.Error())
		} else {
			// Suspension lifted, but they are NOT back in service — so the
			// caller must not send a "deliveries resumed" message.
			h.expect("resume/paused_not_resumed", !resumed,
				fmt.Sprintf("resumed=%v", resumed), "false")
		}
		ps, _ = h.customerFor(ctx, pausedCust)
		h.expect("resume/paused_back_to_disabled", ps.status == "disabled", ps.status, "disabled")
		h.expect("resume/paused_stays_inactive", !ps.isActive,
			fmt.Sprintf("is_active=%v", ps.isActive), "false")
	}

	// ---------------------------------------------------------------------
	// Summary
	// ---------------------------------------------------------------------

	var failed int
	for _, r := range h.results {
		if !r.passed {
			failed++
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("─", 68))
	if failed == 0 {
		fmt.Printf("\033[32m%d/%d assertions passed.\033[0m\n", len(h.results), len(h.results))
	} else {
		fmt.Printf("\033[31m%d of %d assertions FAILED:\033[0m\n", failed, len(h.results))
		for _, r := range h.results {
			if !r.passed {
				fmt.Printf("  • %-34s %s\n", r.name, r.detail)
			}
		}
	}

	teardown()
	fmt.Println()

	if failed > 0 {
		db.Close()
		os.Exit(1)
	}
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func exit(msg string) {
	fmt.Fprintf(os.Stderr, "\n%s\n\n", msg)
	os.Exit(1)
}
