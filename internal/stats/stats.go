package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/billing"
	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UnroutedSlot is an active customer with a subscription that has no route
// (or no stop position), so no manifest will ever include it.
//
// This is possible by design: Approve requires at least one slot to be
// assigned, not all of them, so you can onboard someone's morning delivery
// while evening capacity is still being sorted out. The risk is that it's
// completely silent — the customer shows as active, nothing errors, and the
// missing delivery only surfaces when they phone up asking where it is.
type UnroutedSlot struct {
	CustomerId   string `json:"customerId"`
	CustomerName string `json:"customerName"`
	Slot         string `json:"slot"`
}

// AdminStats is the single payload behind the admin dashboard.
//
// Everything here is derived, never stored. The dashboard is a read-only view
// of state that already lives in customers / subscriptions / delivery_logs.
type AdminStats struct {
	Date string `json:"date"`

	// Fleet
	ActiveDrivers int `json:"activeDrivers"`
	TotalDrivers  int `json:"totalDrivers"`

	// Today's runs. NOTE: the unit here is a (route, slot) pair, not a route.
	// Since the slots migration, "Route A morning" and "Route A evening" are
	// two independent runs with potentially different drivers, so counting
	// bare routes would understate the day's work.
	RunsTotal      int `json:"runsTotal"`
	RunsInProgress int `json:"runsInProgress"`
	RunsCompleted  int `json:"runsCompleted"`

	// Today's stops
	StopsExpected  int `json:"stopsExpected"`
	StopsDelivered int `json:"stopsDelivered"`
	CompletionPct  int `json:"completionPct"`

	// Queues needing a human
	PendingApprovals int `json:"pendingApprovals"`
	FlaggedCount     int `json:"flaggedCount"`

	// Slots that will never be delivered until someone assigns a route.
	// Returned as a list rather than a bare count so the dashboard can name
	// the customers — a number alone tells you there's a problem without
	// telling you whose.
	UnroutedSlots []UnroutedSlot `json:"unroutedSlots"`

	// Commercials
	ActiveCustomers int     `json:"activeCustomers"`
	MonthRevenue    float64 `json:"monthRevenue"`
	MonthLabel      string  `json:"monthLabel"`

	// Things that are quietly wrong. Always present, never null, so the
	// client can render the section unconditionally.
	Attention Attention `json:"attention"`
}

// PaymentAnomaly is a webhook outcome that needs a human.
//
// Every one of these means money moved in a way the system couldn't fully
// reconcile, and none of them were visible anywhere before this: they lived
// only in payment_events, readable by hand-written SQL.
//
//	ALREADY_PAID    the customer paid twice — cash then online, usually.
//	                They are owed a refund, and nobody will chase us for it.
//	AMOUNT_MISMATCH they paid a different amount than billed, typically
//	                because the invoice was corrected after the link went out.
//	UNMATCHED       money arrived that we can't tie to any invoice. This one
//	                becomes a customer insisting they paid while the system
//	                says otherwise.
type PaymentAnomaly struct {
	EventId      string   `json:"eventId"`
	Outcome      string   `json:"outcome"`
	Note         string   `json:"note"`
	ReceivedAt   string   `json:"receivedAt"`
	InvoiceId    *string  `json:"invoiceId"`
	CustomerId   *string  `json:"customerId"`
	CustomerName *string  `json:"customerName"`
	Amount       *float64 `json:"amount"`
}

// StuckChange is an approved order change whose effective date has passed
// without the sweeper applying it.
//
// Should always be empty. When it isn't, a customer was told their new order
// starts tomorrow and it never did — and because ListPendingChanges filters
// on review_status = 'PENDING', the row has also vanished from the admin
// review queue. Invisible in both directions, which is the exact failure
// shape this codebase keeps producing.
type StuckChange struct {
	ChangeId      string `json:"changeId"`
	CustomerId    string `json:"customerId"`
	CustomerName  string `json:"customerName"`
	EffectiveFrom string `json:"effectiveFrom"`
	DaysLate      int    `json:"daysLate"`
}

// SuspendedCustomer is an account cut off for non-payment.
type SuspendedCustomer struct {
	CustomerId   string  `json:"customerId"`
	CustomerName string  `json:"customerName"`
	Amount       float64 `json:"amount"`
	BillingMonth string  `json:"billingMonth"`
	SuspendedOn  *string `json:"suspendedOn"`
}

// DunningStatus reports whether the automated billing cycle actually ran.
//
// The cycle is the only thing that produces invoices. If the 1st passes
// without a GENERATE row, no bills went out at all — and the first signal
// would be nobody paying, weeks later. A dashboard that shows revenue but
// not whether it was ever billed is telling half the story.
type DunningStatus struct {
	Phase        string `json:"phase"`
	LastRunOn    string `json:"lastRunOn"`
	Affected     int    `json:"affected"`
	Note         string `json:"note"`
	BillingMonth string `json:"billingMonth"`
}

// Attention groups everything a human needs to act on that isn't part of
// today's delivery run. Kept separate from the operational counts above
// because they answer different questions: "is today going out?" versus
// "is anything quietly broken?".
type Attention struct {
	PaymentAnomalies []PaymentAnomaly    `json:"paymentAnomalies"`
	StuckChanges     []StuckChange       `json:"stuckChanges"`
	Suspended        []SuspendedCustomer `json:"suspended"`
	OverdueCount     int                 `json:"overdueCount"`
	OverdueAmount    float64             `json:"overdueAmount"`
	LastDunningRuns  []DunningStatus     `json:"lastDunningRuns"`
	BillingRunMissed bool                `json:"billingRunMissed"`
}

type StatsResource struct{ DB *pgxpool.Pool }

func (sr StatsResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", sr.GetStats)
	return r
}

// dueTodayCTE resolves which (customer, slot) pairs are actually owed a
// delivery on the target date.
//
// This is the same schedule predicate used by GenerateManifest and
// CloseRoute. It is duplicated here deliberately rather than shared: the
// dashboard must keep reporting even if manifest logic is mid-refactor, and a
// silent divergence is easier to spot than a silent coupling. If you change
// the schedule rules, change them in all three places.
//
// The trailing quantity check matters as much as the schedule rules. A
// customer who paused today has an override row, which satisfies
// `o.id IS NOT NULL`, so without it they'd count as a stop that was expected
// and never delivered — permanently capping the completion rate below 100%
// on any day someone skips. GenerateManifest drops those stops too, so the
// driver was never asked to make them.
// Built at init rather than declared const, because the schedule predicate is
// now generated — one call to item_due_on() per product, produced by
// schedule.AnyItemDueExpr so that this site and the four query sites cannot
// drift apart. They previously agreed only by coincidence.
var dueTodayCTE = `
	WITH due AS (
		SELECT s.route_id, s.slot, s.customer_id
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		LEFT JOIN order_overrides o
			   ON o.customer_id = s.customer_id
			  AND o.target_date  = $1::date
			  AND o.slot         = s.slot
		WHERE s.route_id IS NOT NULL
		  AND c.status = 'active'
		  AND (
			  -- Any item due, or an explicit override for this date.
			  ` + schedule.AnyItemDueExpr("s", "$1") + `
			  OR o.id IS NOT NULL
		  )
		  AND (
			  COALESCE(o.new_milk_qty,    s.default_milk_qty,    0)
			+ COALESCE(o.new_curd_qty,    s.default_curd_qty,    0)
			+ COALESCE(o.new_butter_qty,  s.default_butter_qty,  0)
			+ COALESCE(o.new_ghee_qty,    s.default_ghee_qty,    0)
			+ COALESCE(o.new_lassi_qty,   s.default_lassi_qty,   0)
			+ COALESCE(o.new_paneer_qty,  s.default_paneer_qty,  0)
			+ COALESCE(o.new_jaggery_qty, s.default_jaggery_qty, 0)
			+ COALESCE(o.new_khand_qty,   s.default_khand_qty,   0)
			+ COALESCE(o.new_oil_qty,     s.default_oil_qty,     0)
			+ COALESCE(o.new_atta_qty,    s.default_atta_qty,    0)
			+ COALESCE(o.new_burfi_qty,   s.default_burfi_qty,   0)
		  ) > 0
	)
`

func (sr StatsResource) GetStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	target := r.URL.Query().Get("date")
	if target == "" {
		target = time.Now().Format("2006-01-02")
	}
	month := target[:7]

	out := AdminStats{Date: target, MonthLabel: month, UnroutedSlots: []UnroutedSlot{}}

	// --- 1. Today's runs and stops -----------------------------------------
	//
	// A run is counted as:
	//   completed   — every stop owed on it has a log row (delivered, skipped
	//                 or unattempted); the driver is done with it either way
	//   in progress — at least one log row, but not all of them
	//
	// Note the FILTER clauses count *rows in agg*, i.e. runs, while the SUMs
	// count stops. Mixing those up is the easy mistake here.
	runQuery := dueTodayCTE + `
		, agg AS (
			SELECT due.route_id,
			       due.slot,
			       COUNT(*)                                            AS expected,
			       COUNT(dl.id)                                        AS logged,
			       COUNT(dl.id) FILTER (WHERE dl.status = 'DELIVERED') AS delivered
			FROM due
			LEFT JOIN delivery_logs dl
				   ON dl.customer_id   = due.customer_id
				  AND dl.delivery_date = $1::date
				  AND dl.slot          = due.slot
			GROUP BY due.route_id, due.slot
		)
		SELECT
			COALESCE(SUM(expected), 0),
			COALESCE(SUM(delivered), 0),
			COUNT(*),
			COUNT(*) FILTER (WHERE logged > 0 AND logged < expected),
			COUNT(*) FILTER (WHERE logged >= expected)
		FROM agg
	`
	err := sr.DB.QueryRow(ctx, runQuery, target).Scan(
		&out.StopsExpected,
		&out.StopsDelivered,
		&out.RunsTotal,
		&out.RunsInProgress,
		&out.RunsCompleted,
	)
	if err != nil {
		http.Error(w, "Failed computing run stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if out.StopsExpected > 0 {
		out.CompletionPct = int(float64(out.StopsDelivered) / float64(out.StopsExpected) * 100)
	}

	// --- 2. Fleet, queues, customer counts ---------------------------------
	//
	// Four independent scalars in one round trip. Each subquery is a cheap
	// indexed count; splitting them into four QueryRow calls would just cost
	// three extra network hops.
	countQuery := `
		SELECT
			(SELECT COUNT(*) FROM drivers WHERE is_active = true),
			(SELECT COUNT(*) FROM drivers),
			(SELECT COUNT(*) FROM customers WHERE status = 'pending'),
			(SELECT COUNT(*) FROM delivery_logs WHERE is_flagged = true),
			(SELECT COUNT(*) FROM customers WHERE status = 'active')
	`
	err = sr.DB.QueryRow(ctx, countQuery).Scan(
		&out.ActiveDrivers,
		&out.TotalDrivers,
		&out.PendingApprovals,
		&out.FlaggedCount,
		&out.ActiveCustomers,
	)
	if err != nil {
		http.Error(w, "Failed computing counts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// --- 3. Unrouted slots -------------------------------------------------
	//
	// stop_order = 0 counts as unrouted too: it's the default, and the
	// manifest requires stop_order > 0. It's also what FinalInvoice sets to
	// pull a departing customer off the run — but those are marked inactive
	// shortly after, and status = 'active' filters them out here.
	//
	// Capped at 500 — bounded by customer count, not by time.
	//
	// An unrouted slot means a customer who believes they are subscribed and
	// appears on no manifest. Hiding some behind a cap is the same failure as
	// not listing them at all, and a bulk import or a batch of approvals could
	// easily produce more than fifty at once.
	unroutedRows, err := sr.DB.Query(ctx, `
		SELECT s.customer_id::text, c.name, s.slot
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		WHERE c.status = 'active'
		  AND (s.route_id IS NULL OR s.stop_order = 0)
		ORDER BY c.name ASC, s.slot ASC
		LIMIT 500
	`)
	if err != nil {
		http.Error(w, "Failed checking unrouted slots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer unroutedRows.Close()

	for unroutedRows.Next() {
		var u UnroutedSlot
		if err := unroutedRows.Scan(&u.CustomerId, &u.CustomerName, &u.Slot); err != nil {
			continue
		}
		out.UnroutedSlots = append(out.UnroutedSlots, u)
	}

	// --- 4. Month-to-date revenue ------------------------------------------
	//
	// Reads unit_price_* off the log, never routes.price_*. That is the whole
	// point of the price snapshot: a mid-month price change must not
	// retroactively reprice deliveries that already happened.
	terms := make([]string, 0, len(billing.Products))
	for _, p := range billing.Products {
		terms = append(terms,
			fmt.Sprintf("COALESCE(SUM(delivered_%s_qty * unit_price_%s), 0)", p, p))
	}
	revQuery := fmt.Sprintf(`
		SELECT %s
		FROM delivery_logs
		WHERE status = 'DELIVERED'
		  AND TO_CHAR(delivery_date, 'YYYY-MM') = $1
	`, strings.Join(terms, " + "))

	if err := sr.DB.QueryRow(ctx, revQuery, month).Scan(&out.MonthRevenue); err != nil {
		http.Error(w, "Failed computing revenue: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// --- Attention ---------------------------------------------------------
	//
	// Deliberately best-effort. Each loader logs and leaves its slice empty on
	// failure rather than aborting the response: the operational half of this
	// dashboard is what someone checks at 6am to see whether the vans went
	// out, and it must not go blank because a payment query timed out.
	out.Attention = sr.loadAttention(ctx)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// loadAttention gathers everything that is quietly wrong.
func (sr StatsResource) loadAttention(ctx context.Context) Attention {
	a := Attention{
		PaymentAnomalies: []PaymentAnomaly{},
		StuckChanges:     []StuckChange{},
		Suspended:        []SuspendedCustomer{},
		LastDunningRuns:  []DunningStatus{},
	}

	// --- Payment anomalies -------------------------------------------------
	//
	// Capped at 200. Each row is money that moved in a way we couldn't
	// reconcile — an ALREADY_PAID is a refund somebody is owed and will not
	// chase us for. A cap of 50 was a worklist's number, but these do not get
	// worked off by being looked at; they persist until someone acts.
	//
	// Still bounded, because payment_events grows with every webhook forever
	// and an unbounded query would eventually make the dashboard slow. If 200
	// is ever reached, the problem is not the limit.
	//
	// LEFT JOINs throughout because an UNMATCHED event by definition has no
	// invoice, and therefore no customer.
	rows, err := sr.DB.Query(ctx, `
		SELECT pe.event_id, pe.outcome, COALESCE(pe.note, ''),
		       TO_CHAR(pe.received_at, 'YYYY-MM-DD HH24:MI'),
		       i.id::text, c.id::text, c.name, i.total_amount
		FROM payment_events pe
		LEFT JOIN invoices  i ON i.id = pe.invoice_id
		LEFT JOIN customers c ON c.id = i.customer_id
		WHERE pe.outcome IN ('ALREADY_PAID', 'AMOUNT_MISMATCH', 'UNMATCHED')
		ORDER BY pe.received_at DESC
		LIMIT 200
	`)
	if err != nil {
		fmt.Printf("⚠️ stats: payment anomalies failed: %v\n", err)
	} else {
		for rows.Next() {
			var p PaymentAnomaly
			if err := rows.Scan(&p.EventId, &p.Outcome, &p.Note, &p.ReceivedAt,
				&p.InvoiceId, &p.CustomerId, &p.CustomerName, &p.Amount); err != nil {
				continue
			}
			a.PaymentAnomalies = append(a.PaymentAnomalies, p)
		}
		rows.Close()
	}

	// --- Stuck approved changes -------------------------------------------
	//
	// Should always return nothing. A row here means applyOneChange has been
	// failing silently — it logs a warning and retries hourly forever, while
	// the change is invisible in the admin queue because that filters on
	// PENDING.
	rows, err = sr.DB.Query(ctx, `
		SELECT p.id::text, p.customer_id::text, c.name,
		       TO_CHAR(p.effective_from, 'YYYY-MM-DD'),
		       (CURRENT_DATE - p.effective_from)::int
		FROM pending_subscription_changes p
		JOIN customers c ON c.id = p.customer_id
		WHERE p.review_status = 'APPROVED'
		  AND p.effective_from < CURRENT_DATE
		ORDER BY p.effective_from
		-- Should always return nothing. If it ever returns 200, the sweeper
		-- has been failing for weeks and the count matters more than the list.
		LIMIT 200
	`)
	if err != nil {
		fmt.Printf("⚠️ stats: stuck changes failed: %v\n", err)
	} else {
		for rows.Next() {
			var sc StuckChange
			if err := rows.Scan(&sc.ChangeId, &sc.CustomerId, &sc.CustomerName,
				&sc.EffectiveFrom, &sc.DaysLate); err != nil {
				continue
			}
			a.StuckChanges = append(a.StuckChanges, sc)
		}
		rows.Close()
	}

	// --- Suspended customers ----------------------------------------------
	//
	// Joined to the invoice that caused the suspension, so the list can show
	// what's owed rather than just who's cut off. Amount is what gets them
	// switched back on, so it's the actionable number.
	rows, err = sr.DB.Query(ctx, `
		SELECT c.id::text, c.name,
		       COALESCE(i.total_amount, 0), COALESCE(i.billing_month, ''),
		       TO_CHAR(i.suspended_at, 'YYYY-MM-DD')
		FROM customers c
		LEFT JOIN invoices i ON i.id = c.suspended_for_invoice_id
		WHERE c.status = 'suspended'
		ORDER BY i.suspended_at NULLS LAST
		-- Bounded by the customer count, not by time. Every suspended account
		-- is a customer receiving no deliveries who thinks they should be.
		LIMIT 500
	`)
	if err != nil {
		fmt.Printf("⚠️ stats: suspended customers failed: %v\n", err)
	} else {
		for rows.Next() {
			var sc SuspendedCustomer
			if err := rows.Scan(&sc.CustomerId, &sc.CustomerName,
				&sc.Amount, &sc.BillingMonth, &sc.SuspendedOn); err != nil {
				continue
			}
			a.Suspended = append(a.Suspended, sc)
		}
		rows.Close()
	}

	// --- Money outstanding -------------------------------------------------
	//
	// Every unpaid invoice regardless of month, not just last month's. An old
	// unpaid bill is still money owed, and the dunning cycle only ever chases
	// the most recent month — so anything older would otherwise be chased
	// once and then forgotten.
	if err := sr.DB.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_amount), 0)
		FROM invoices WHERE status NOT LIKE 'PAID%'
	`).Scan(&a.OverdueCount, &a.OverdueAmount); err != nil {
		fmt.Printf("⚠️ stats: overdue totals failed: %v\n", err)
	}

	// --- Did the billing cycle run? ---------------------------------------
	rows, err = sr.DB.Query(ctx, `
		SELECT DISTINCT ON (phase)
		       phase, TO_CHAR(run_date, 'YYYY-MM-DD'), affected,
		       COALESCE(note, ''), billing_month
		FROM dunning_runs
		ORDER BY phase, run_date DESC
	`)
	if err != nil {
		fmt.Printf("⚠️ stats: dunning runs failed: %v\n", err)
	} else {
		for rows.Next() {
			var d DunningStatus
			if err := rows.Scan(&d.Phase, &d.LastRunOn, &d.Affected, &d.Note, &d.BillingMonth); err != nil {
				continue
			}
			a.LastDunningRuns = append(a.LastDunningRuns, d)
		}
		rows.Close()
	}

	// BillingRunMissed: it's past the 1st and no GENERATE ran this month.
	//
	// This is the single most valuable flag on the dashboard. If invoice
	// generation fails on the 1st, nothing else surfaces it — no bill goes
	// out, no reminder, no suspension, and the first signal is customers not
	// paying for a month they were never billed for.
	loc, lerr := time.LoadLocation("Asia/Kolkata")
	if lerr == nil {
		now := time.Now().In(loc)
		if now.Day() >= 1 {
			var ran bool
			if err := sr.DB.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM dunning_runs
					WHERE phase = 'GENERATE'
					  AND run_date >= date_trunc('month', CURRENT_DATE)
				)
			`).Scan(&ran); err == nil {
				a.BillingRunMissed = !ran
			}
		}
	}

	return a
}
