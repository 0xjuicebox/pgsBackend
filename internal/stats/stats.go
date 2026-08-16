package stats

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/billing"
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
const dueTodayCTE = `
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
				s.schedule_type = 'daily'
			 OR (s.schedule_type = 'custom'
				 AND EXTRACT(DOW FROM $1::date)::int = ANY(s.active_days))
			 OR (s.schedule_type = 'alternate'
				 AND ($1::date - s.anchor_date) % 2 = 0)
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
	// Capped at 50. If it ever gets that long the list isn't the problem.
	unroutedRows, err := sr.DB.Query(ctx, `
		SELECT s.customer_id::text, c.name, s.slot
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		WHERE c.status = 'active'
		  AND (s.route_id IS NULL OR s.stop_order = 0)
		ORDER BY c.name ASC, s.slot ASC
		LIMIT 50
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
