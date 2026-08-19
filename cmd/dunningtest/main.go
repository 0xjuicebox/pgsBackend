// Command dunningtest drives the full dunning cycle against throwaway
// fixtures and asserts the resulting database state.
//
//	go run ./cmd/dunningtest -no-whatsapp
//	go run ./cmd/dunningtest -phone +919876543210
//	go run ./cmd/dunningtest -phone +919876543210 -force
//	go run ./cmd/dunningtest -keep
//
// # WHY A HARNESS RATHER THAN WAITING FOR THE CALENDAR
//
// Every dunning phase is date-gated: generate on the 1st, remind to the 5th,
// suspend on the 6th. Testing it by waiting means one attempt per month, and
// the resume-on-payment path can't be reached at all without a real payment.
//
// This calls BillingResource.RunPhase directly with an explicit month. The
// phases themselves are unmodified — same idempotency guards, same SQL, same
// notifications. Only the calendar is bypassed.
//
// # WHY SCENARIOS RUN SEQUENTIALLY
//
// Three customers with three different histories, run one after another and
// torn down between, rather than three fixtures existing at once. That means
// a single phone number can play all three parts — so the messages arrive on
// one real handset, in the order a real customer would receive them.
//
// dunning_runs is cleared between scenarios. Its UNIQUE (run_date, phase)
// constraint is what stops GENERATE running twice in a day, which is correct
// in production and would silently skip scenarios two and three here.
//
// THE SCENARIOS
//
//  1. ACTIVE   bill -> reminder -> suspension -> pays -> resumes to active
//  2. PAUSED   paused for a holiday, owes money, suspended, pays, stays paused
//  3. PAID     pays immediately, must never be reminded or suspended
//
// Scenario 2 is the one worth having a harness for. Paying a debt is not a
// request to restart deliveries; restoring that customer to 'active' would
// deliver milk to an empty house.
//
// # SAFETY
//
// Billing month is two years out, so nothing here can be picked up by real
// reporting. If the target phone already belongs to a customer, the run
// aborts and reports what would be destroyed — pass -force to proceed.
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
	// Two years out: past any real billing month, and obviously synthetic to
	// anyone who finds it in the database.
	testMonth      = "2098-11"
	testDatePrefix = "2098-11-"
	fixtureTag     = "DUNNINGTEST FIXTURE — safe to delete"

	// Fallback when no real number is supplied. Ten zeros is not an
	// assignable Indian number, so Twilio rejects it at validation and
	// nothing leaves their platform.
	defaultPhone = "+910000000000"
)

var (
	flagPhone      = flag.String("phone", "", "phone number for fixtures, E.164 (default: unroutable placeholder)")
	flagKeep       = flag.Bool("keep", false, "skip final cleanup so fixture rows can be inspected")
	flagNoWhatsApp = flag.Bool("no-whatsapp", false, "run without Twilio so no real messages are sent")
	flagForce      = flag.Bool("force", false, "delete an existing customer on the target number before running")
	flagPace       = flag.Duration("pace", 3*time.Second, "pause between phases so messages arrive in readable order")
)

type harness struct {
	db      *pgxpool.Pool
	br      billing.BillingResource
	phone   string
	pace    time.Duration
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
		fmt.Printf("  \033[32mPASS\033[0m  %-36s %s\n", name, got)
		return
	}
	h.results = append(h.results, result{name, false, fmt.Sprintf("got %s, want %s", got, want)})
	fmt.Printf("  \033[31mFAIL\033[0m  %-36s got %s, want %s\n", name, got, want)
}

func (h *harness) fail(name, detail string) {
	h.results = append(h.results, result{name, false, detail})
	fmt.Printf("  \033[31mFAIL\033[0m  %-36s %s\n", name, detail)
}

// wait paces the run so a human watching a phone can tell which message
// belongs to which phase. Skipped entirely when Twilio is off, since there is
// then nothing to watch.
func (h *harness) wait() {
	if !*flagNoWhatsApp && h.pace > 0 {
		time.Sleep(h.pace)
	}
}

// -------------------------------------------------------------------------
// Fixtures
// -------------------------------------------------------------------------

type fixture struct {
	customerID uuid.UUID
	routeID    uuid.UUID
	invoiceID  string
}

func (h *harness) newRoute(ctx context.Context) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return id, err
	}
	_, err = h.db.Exec(ctx, `INSERT INTO routes (id, name) VALUES ($1, $2)`, id, fixtureTag)
	return id, err
}

// newCustomer creates a fixture with a route and a stop order, so the harness
// can prove suspension leaves routing intact — unlike FinalInvoice, which
// zeroes stop_order because a leaver is gone for good.
func (h *harness) newCustomer(ctx context.Context, status string, routeID uuid.UUID) (fixture, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return fixture{}, err
	}

	if _, err := h.db.Exec(ctx, `
		INSERT INTO customers (id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status)
		VALUES ($1, $2, $3, 'Fixture address, do not deliver', '0', '0', $4, $5)
	`, id, fixtureTag, h.phone, status == "active", status); err != nil {
		return fixture{}, err
	}

	if _, err := h.db.Exec(ctx, `
		INSERT INTO subscriptions (customer_id, slot, route_id, stop_order, default_milk_qty,
		                           schedule_type, active_days)
		VALUES ($1, 'morning', $2, 7, 2000, 'daily', ARRAY[0,1,2,3,4,5,6])
	`, id, routeID); err != nil {
		return fixture{}, err
	}

	return fixture{customerID: id, routeID: routeID}, nil
}

// addDeliveryLogs writes delivered logs with real price snapshots. Billing
// reads only from these — never from subscriptions — so this is what
// determines the invoice total.
func (h *harness) addDeliveryLogs(ctx context.Context, f fixture, n, milkMl int, pricePerMl float64) (float64, error) {
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

// resetScenario removes everything a scenario created. Called before and
// after each, so a scenario always starts from nothing and a failure part way
// through can't poison the next one.
//
// dunning_runs must go too: its UNIQUE (run_date, phase) is what stops
// GENERATE running twice in one day, which would otherwise make scenarios two
// and three silently no-op.
func (h *harness) resetScenario(ctx context.Context) error {
	if _, err := h.db.Exec(ctx, `DELETE FROM dunning_runs WHERE run_date = CURRENT_DATE`); err != nil {
		return err
	}

	var id uuid.UUID
	err := h.db.QueryRow(ctx,
		`SELECT id FROM customers WHERE phone_number = $1 AND name = $2`, h.phone, fixtureTag).Scan(&id)
	if err == nil {
		if derr := deleteCustomerDeep(ctx, h.db, id); derr != nil {
			return derr
		}
	}

	_, err = h.db.Exec(ctx, `DELETE FROM routes WHERE name = $1`, fixtureTag)
	return err
}

// deleteCustomerDeep removes a customer and everything referencing them.
//
// Not every foreign key into customers cascades — order_overrides and
// pending_subscription_changes both use the default NO ACTION, so a plain
// DELETE FROM customers fails with a 23503 for anyone who has ever changed a
// single day's order or submitted an order change. Dependents therefore have
// to go first, in reference order.
//
// This mirrors customer.Delete in internal/customer/routes.go, which does the
// same thing for the admin path — except that one omits
// pending_subscription_changes and so fails on any customer with change
// history.
func deleteCustomerDeep(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID) error {
	// payment_events references invoices, not customers, and is ON DELETE
	// SET NULL — so it survives deliberately. A payment record outliving the
	// customer is correct: the money still moved.
	stmts := []string{
		`DELETE FROM order_overrides WHERE customer_id = $1`,
		`DELETE FROM pending_subscription_changes WHERE customer_id = $1`,
		`DELETE FROM delivery_logs WHERE customer_id = $1`,
		`DELETE FROM invoices WHERE customer_id = $1`,
		`DELETE FROM subscriptions WHERE customer_id = $1`,
		`DELETE FROM customers WHERE id = $1`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(ctx, q, customerID); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(q, " WHERE", 2)[0], err)
		}
	}
	return nil
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
}

func (h *harness) invoiceFor(ctx context.Context, f fixture) (invoiceState, error) {
	var st invoiceState
	err := h.db.QueryRow(ctx, `
		SELECT id::text, total_amount, status, COALESCE(reminder_count,0),
		       TO_CHAR(last_reminder_on,'YYYY-MM-DD')
		FROM invoices WHERE customer_id = $1 AND billing_month = $2
	`, f.customerID, testMonth).Scan(&st.id, &st.total, &st.status, &st.reminderCount, &st.lastReminder)
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
		SELECT c.status, c.is_active, c.status_before_suspension, s.stop_order, s.route_id::text
		FROM customers c
		JOIN subscriptions s ON s.customer_id = c.id AND s.slot = 'morning'
		WHERE c.id = $1
	`, f.customerID).Scan(&st.status, &st.isActive, &st.beforeSusp, &st.stopOrder, &st.routeID)
	return st, err
}

// -------------------------------------------------------------------------
// Scenario 1 — active customer, full cycle through to resume
// -------------------------------------------------------------------------

func (h *harness) scenarioActive(ctx context.Context) {
	const s = "active"
	fmt.Println("\n\033[1mScenario 1 — active customer: bill, reminder, suspension, payment\033[0m")

	if err := h.resetScenario(ctx); err != nil {
		h.fail(s+"/reset", err.Error())
		return
	}
	routeID, err := h.newRoute(ctx)
	if err != nil {
		h.fail(s+"/route", err.Error())
		return
	}
	f, err := h.newCustomer(ctx, "active", routeID)
	if err != nil {
		h.fail(s+"/customer", err.Error())
		return
	}
	// 20 deliveries of 2 L at ₹0.07/ml = ₹140/day = ₹2,800
	expected, err := h.addDeliveryLogs(ctx, f, 20, 2000, 0.07)
	if err != nil {
		h.fail(s+"/logs", err.Error())
		return
	}

	// --- Generate -------------------------------------------------------
	fmt.Println("  → generating bill")
	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail(s+"/generate", err.Error())
		return
	}
	inv, err := h.invoiceFor(ctx, f)
	if err != nil {
		h.fail(s+"/generate/invoice", "no invoice created: "+err.Error())
		return
	}
	h.expect(s+"/generate/total",
		fmt.Sprintf("%.2f", inv.total) == fmt.Sprintf("%.2f", expected),
		fmt.Sprintf("₹%.2f", inv.total), fmt.Sprintf("₹%.2f", expected))
	h.expect(s+"/generate/status", inv.status == "PENDING", inv.status, "PENDING")
	h.wait()

	// Re-run: dunning_runs already holds today's GENERATE. Without that
	// guard, a restart on the 1st would re-message every customer.
	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail(s+"/generate/rerun", err.Error())
	}
	inv2, _ := h.invoiceFor(ctx, f)
	h.expect(s+"/generate/idempotent", inv2.id == inv.id, "same invoice", "same invoice")

	// --- Remind ---------------------------------------------------------
	fmt.Println("  → sending reminder")
	if err := h.br.RunPhase(ctx, "REMIND", testMonth); err != nil {
		h.fail(s+"/remind", err.Error())
	}
	inv, _ = h.invoiceFor(ctx, f)
	h.expect(s+"/remind/marked", inv.lastReminder != nil,
		fmt.Sprintf("last_reminder_on=%s", derefOr(inv.lastReminder, "<nil>")), "today")
	h.expect(s+"/remind/counted", inv.reminderCount == 1,
		fmt.Sprintf("count=%d", inv.reminderCount), "1")
	h.wait()

	// Same day again: last_reminder_on excludes it. This is what stops the
	// hourly ticker sending 24 reminders a day.
	if err := h.br.RunPhase(ctx, "REMIND", testMonth); err != nil {
		h.fail(s+"/remind/rerun", err.Error())
	}
	inv, _ = h.invoiceFor(ctx, f)
	h.expect(s+"/remind/idempotent", inv.reminderCount == 1,
		fmt.Sprintf("count=%d", inv.reminderCount), "1 (unchanged)")

	// --- Suspend --------------------------------------------------------
	fmt.Println("  → suspending")
	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail(s+"/suspend", err.Error())
	}
	cs, err := h.customerFor(ctx, f)
	if err != nil {
		h.fail(s+"/suspend/read", err.Error())
		return
	}
	h.expect(s+"/suspend/status", cs.status == "suspended", cs.status, "suspended")
	h.expect(s+"/suspend/inactive", !cs.isActive, fmt.Sprintf("is_active=%v", cs.isActive), "false")
	// Routing survives so resuming is one flag flip, not an admin re-routing
	// them from scratch.
	h.expect(s+"/suspend/keeps_stop_order", cs.stopOrder == 7,
		fmt.Sprintf("stop_order=%d", cs.stopOrder), "7")
	h.expect(s+"/suspend/keeps_route", cs.routeID != nil,
		fmt.Sprintf("route_id=%s", derefOr(cs.routeID, "<nil>")), "unchanged")
	h.expect(s+"/suspend/records_prior", derefOr(cs.beforeSusp, "") == "active",
		fmt.Sprintf("before=%s", derefOr(cs.beforeSusp, "<nil>")), "active")
	h.wait()

	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail(s+"/suspend/rerun", err.Error())
	}
	cs, _ = h.customerFor(ctx, f)
	h.expect(s+"/suspend/idempotent", cs.status == "suspended", cs.status, "suspended")

	// --- Pay and resume -------------------------------------------------
	fmt.Println("  → paying")
	inv, _ = h.invoiceFor(ctx, f)
	if _, err := h.db.Exec(ctx,
		`UPDATE invoices SET status = 'PAID_ONLINE', paid_at = NOW() WHERE id = $1::uuid`, inv.id); err != nil {
		h.fail(s+"/resume/mark_paid", err.Error())
		return
	}
	resumed, err := h.br.ResumeIfSuspended(ctx, inv.id)
	if err != nil {
		h.fail(s+"/resume/call", err.Error())
		return
	}
	h.expect(s+"/resume/reported", resumed, fmt.Sprintf("resumed=%v", resumed), "true")
	cs, _ = h.customerFor(ctx, f)
	h.expect(s+"/resume/status", cs.status == "active", cs.status, "active")
	h.expect(s+"/resume/active", cs.isActive, fmt.Sprintf("is_active=%v", cs.isActive), "true")
	h.expect(s+"/resume/clears_prior", cs.beforeSusp == nil,
		fmt.Sprintf("before=%s", derefOr(cs.beforeSusp, "<nil>")), "<nil>")

	_ = h.resetScenario(ctx)
}

// -------------------------------------------------------------------------
// Scenario 2 — paused customer who owes money
// -------------------------------------------------------------------------

// The case this harness exists for. Someone pauses for a holiday, still owes
// last month's bill, gets suspended on the 6th, then pays. They must go back
// to 'disabled' — they are still on holiday. Restoring them to 'active' would
// deliver milk to an empty house because they settled a debt.
func (h *harness) scenarioPaused(ctx context.Context) {
	const s = "paused"
	fmt.Println("\n\033[1mScenario 2 — paused customer who owes money: must stay paused after paying\033[0m")

	if err := h.resetScenario(ctx); err != nil {
		h.fail(s+"/reset", err.Error())
		return
	}
	routeID, err := h.newRoute(ctx)
	if err != nil {
		h.fail(s+"/route", err.Error())
		return
	}
	f, err := h.newCustomer(ctx, "disabled", routeID)
	if err != nil {
		h.fail(s+"/customer", err.Error())
		return
	}
	if _, err := h.addDeliveryLogs(ctx, f, 10, 1000, 0.07); err != nil {
		h.fail(s+"/logs", err.Error())
		return
	}

	fmt.Println("  → generating bill")
	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail(s+"/generate", err.Error())
		return
	}
	inv, err := h.invoiceFor(ctx, f)
	if err != nil {
		h.fail(s+"/generate/invoice", "no invoice for a paused customer: "+err.Error())
		return
	}
	// A paused customer is still billed for what was delivered before they
	// paused — billing reads delivery_logs, not account state.
	h.expect(s+"/generate/billed_anyway", inv.total > 0,
		fmt.Sprintf("₹%.2f", inv.total), "> 0")
	h.wait()

	fmt.Println("  → suspending")
	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail(s+"/suspend", err.Error())
	}
	cs, err := h.customerFor(ctx, f)
	if err != nil {
		h.fail(s+"/suspend/read", err.Error())
		return
	}
	h.expect(s+"/suspend/status", cs.status == "suspended", cs.status, "suspended")
	h.expect(s+"/suspend/records_prior", derefOr(cs.beforeSusp, "") == "disabled",
		fmt.Sprintf("before=%s", derefOr(cs.beforeSusp, "<nil>")), "disabled")
	h.wait()

	fmt.Println("  → paying")
	if _, err := h.db.Exec(ctx,
		`UPDATE invoices SET status = 'PAID_ONLINE', paid_at = NOW() WHERE id = $1::uuid`, inv.id); err != nil {
		h.fail(s+"/resume/mark_paid", err.Error())
		return
	}
	resumed, err := h.br.ResumeIfSuspended(ctx, inv.id)
	if err != nil {
		h.fail(s+"/resume/call", err.Error())
		return
	}
	// Suspension lifted, but they are NOT back in service — so the caller
	// must not send a "deliveries resumed" message.
	h.expect(s+"/resume/not_resumed", !resumed, fmt.Sprintf("resumed=%v", resumed), "false")
	cs, _ = h.customerFor(ctx, f)
	h.expect(s+"/resume/back_to_disabled", cs.status == "disabled", cs.status, "disabled")
	h.expect(s+"/resume/stays_inactive", !cs.isActive,
		fmt.Sprintf("is_active=%v", cs.isActive), "false")

	_ = h.resetScenario(ctx)
}

// -------------------------------------------------------------------------
// Scenario 3 — customer who pays immediately
// -------------------------------------------------------------------------

func (h *harness) scenarioPaid(ctx context.Context) {
	const s = "paid"
	fmt.Println("\n\033[1mScenario 3 — customer who pays on time: must never be chased\033[0m")

	if err := h.resetScenario(ctx); err != nil {
		h.fail(s+"/reset", err.Error())
		return
	}
	routeID, err := h.newRoute(ctx)
	if err != nil {
		h.fail(s+"/route", err.Error())
		return
	}
	f, err := h.newCustomer(ctx, "active", routeID)
	if err != nil {
		h.fail(s+"/customer", err.Error())
		return
	}
	if _, err := h.addDeliveryLogs(ctx, f, 5, 1000, 0.07); err != nil {
		h.fail(s+"/logs", err.Error())
		return
	}

	fmt.Println("  → generating bill")
	if err := h.br.RunPhase(ctx, "GENERATE", testMonth); err != nil {
		h.fail(s+"/generate", err.Error())
		return
	}
	inv, err := h.invoiceFor(ctx, f)
	if err != nil {
		h.fail(s+"/generate/invoice", err.Error())
		return
	}
	h.wait()

	fmt.Println("  → paying immediately")
	if _, err := h.db.Exec(ctx,
		`UPDATE invoices SET status = 'PAID_CASH', paid_at = NOW() WHERE id = $1::uuid`, inv.id); err != nil {
		h.fail(s+"/mark_paid", err.Error())
		return
	}

	// From here on the customer must hear nothing. If either of these sends
	// a message, a paying customer gets chased — the fastest way to lose one.
	fmt.Println("  → running remind and suspend (expect silence)")
	if err := h.br.RunPhase(ctx, "REMIND", testMonth); err != nil {
		h.fail(s+"/remind", err.Error())
	}
	inv, _ = h.invoiceFor(ctx, f)
	h.expect(s+"/remind/skipped", inv.reminderCount == 0,
		fmt.Sprintf("count=%d", inv.reminderCount), "0")

	if err := h.br.RunPhase(ctx, "SUSPEND", testMonth); err != nil {
		h.fail(s+"/suspend", err.Error())
	}
	cs, err := h.customerFor(ctx, f)
	if err != nil {
		h.fail(s+"/suspend/read", err.Error())
		return
	}
	h.expect(s+"/suspend/skipped", cs.status == "active", cs.status, "active")
	h.expect(s+"/suspend/still_active", cs.isActive,
		fmt.Sprintf("is_active=%v", cs.isActive), "true")

	if !*flagKeep {
		_ = h.resetScenario(ctx)
	}
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

	phone := strings.TrimSpace(*flagPhone)
	if phone == "" {
		phone = os.Getenv("WHATSAPP_TEST_PHONE")
	}
	if phone == "" {
		phone = defaultPhone
	}
	if !strings.HasPrefix(phone, "+") {
		exit("phone must be in E.164 form, e.g. +919876543210")
	}

	ctx := context.Background()
	connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		exit("bad DATABASE_URL: " + err.Error())
	}
	// Same IST pinning the server uses. Without it CURRENT_DATE here is the
	// UTC date, and last_reminder_on comparisons drift for 5.5 hours a day.
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"

	db, err := pgxpool.NewWithConfig(connCtx, cfg)
	if err != nil {
		exit("could not connect: " + err.Error())
	}
	defer db.Close()
	if err := db.Ping(connCtx); err != nil {
		exit("database ping failed: " + err.Error())
	}

	// Guard an existing customer on this number. Deleting one cascades to
	// its subscriptions, delivery logs and invoices — a real record with
	// history is not something to remove as a side effect of running a test.
	var existingName, existingStatus string
	var logs, invs int
	err = db.QueryRow(ctx, `
		SELECT c.name, c.status,
		       (SELECT COUNT(*) FROM delivery_logs d WHERE d.customer_id = c.id),
		       (SELECT COUNT(*) FROM invoices i WHERE i.customer_id = c.id)
		FROM customers c WHERE c.phone_number = $1
	`, phone).Scan(&existingName, &existingStatus, &logs, &invs)

	if err == nil && existingName != fixtureTag {
		if !*flagForce {
			exit(fmt.Sprintf(
				"A customer already exists on %s:\n\n  name:     %s\n  status:   %s\n  logs:     %d\n  invoices: %d\n\n"+
					"Running would delete this record and everything attached to it.\n"+
					"Pass -force to proceed, or use a different -phone.",
				phone, existingName, existingStatus, logs, invs))
		}
		fmt.Printf("⚠️  -force: deleting existing customer %q (%d logs, %d invoices)\n\n", existingName, logs, invs)
		var existingID uuid.UUID
		if derr := db.QueryRow(ctx,
			`SELECT id FROM customers WHERE phone_number = $1`, phone).Scan(&existingID); derr != nil {
			exit("could not find existing customer to delete: " + derr.Error())
		}
		if derr := deleteCustomerDeep(ctx, db, existingID); derr != nil {
			exit("could not delete existing customer: " + derr.Error())
		}
	}

	var whatsApp *notification.WhatsAppService
	if !*flagNoWhatsApp {
		whatsApp = notification.NewWhatsAppService()
	}

	pay := payment.New(db,
		os.Getenv("RAZORPAY_KEY_ID"),
		os.Getenv("RAZORPAY_KEY_SECRET"),
		os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
	)

	templates := notification.LoadTemplateSIDs()

	h := &harness{
		db:    db,
		phone: phone,
		pace:  *flagPace,
		br: billing.BillingResource{
			DB:        db,
			WhatsApp:  whatsApp,
			Payment:   pay,
			Templates: templates,
		},
	}

	fmt.Printf("\nDunning cycle test — billing month %s\n", testMonth)
	fmt.Printf("Fixtures use %s", phone)
	if phone == defaultPhone {
		fmt.Print(" (unroutable placeholder)")
	}
	fmt.Println()
	if *flagNoWhatsApp {
		fmt.Println("WhatsApp suppressed (-no-whatsapp)")
	} else {
		if missing := templates.TemplateHealth(); len(missing) > 0 {
			fmt.Printf("⚠️  Templates not configured: %s — those sends fall back to free-form.\n",
				strings.Join(missing, ", "))
		}
		fmt.Printf("Pacing %s between phases so messages arrive in order.\n", h.pace)
	}

	h.scenarioActive(ctx)
	h.scenarioPaused(ctx)
	h.scenarioPaid(ctx)

	var failed int
	for _, r := range h.results {
		if !r.passed {
			failed++
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("─", 72))
	if failed == 0 {
		fmt.Printf("\033[32m%d/%d assertions passed.\033[0m\n", len(h.results), len(h.results))
	} else {
		fmt.Printf("\033[31m%d of %d assertions FAILED:\033[0m\n", failed, len(h.results))
		for _, r := range h.results {
			if !r.passed {
				fmt.Printf("  • %-36s %s\n", r.name, r.detail)
			}
		}
	}

	if *flagKeep {
		fmt.Printf("\n-keep: scenario 3 fixtures left in place on %s, month %s.\n", phone, testMonth)
	}
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
