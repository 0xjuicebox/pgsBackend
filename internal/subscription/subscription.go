package subscription

// Subscription is now the real delivery-plan table: one row per
// (customer, slot). A customer with both morning and evening service has two
// rows, independently routed, independently scheduled, independently priced
// by quantity. customer.Approve routes an existing row; it never creates one.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Subscription struct {
	Id           uuid.UUID      `json:"id"`
	CustomerId   uuid.UUID      `json:"customerId"`
	Slot         string         `json:"slot"`         // "morning" | "evening"
	ScheduleType string         `json:"scheduleType"` // "daily", "custom", or "alternate"
	ActiveDays   []int          `json:"activeDays"`   // e.g. [1, 3, 5] for Mon/Wed/Fri
	AnchorDate   string         `json:"anchorDate"`   // e.g. "2026-07-10"
	DefaultOrder customer.Order `json:"defaultOrder"`
	RouteId      *uuid.UUID     `json:"routeId,omitempty"`
	StopOrder    *int           `json:"stopOrder,omitempty"`

	// ItemSchedules overrides the slot schedule per product. Absent products
	// follow scheduleType above.
	ItemSchedules map[string]schedule.ItemSchedule `json:"itemSchedules,omitempty"`

	// ItemSchedulesRaw is the JSONB as stored, used only for scanning.
	// Excluded from JSON so the API exposes the decoded map, not both.
	ItemSchedulesRaw []byte `json:"-"`
}

type SubscriptionResource struct {
	DB *pgxpool.Pool
}

func (sr SubscriptionResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", sr.CreateOrUpdate)
	r.Get("/customer/{customerId}", sr.ListByCustomer)
	r.Get("/customer/{customerId}/slot/{slot}", sr.GetOne)
	return r
}

var validSlots = map[string]bool{"morning": true, "evening": true}

// CreateOrUpdate is the main "set up this customer's order" endpoint — called
// from WhatsApp registration, the admin customer editor, or an update flow.
//
// RouteId and StopOrder are pointers and deliberately left out of most calls
// to this endpoint: this is where schedules and quantities get edited, not
// where routing decisions get made. Omitting them means "leave routing as it
// is", so editing someone's Tuesday order can never accidentally un-route
// them. Routing is customer.Approve's job, or an explicit call here that does
// include them.
func (sr SubscriptionResource) CreateOrUpdate(w http.ResponseWriter, r *http.Request) {
	var sub Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if sub.CustomerId == uuid.Nil {
		http.Error(w, "customerId is required", http.StatusBadRequest)
		return
	}
	if sub.Slot == "" {
		sub.Slot = "morning"
	}
	if !validSlots[sub.Slot] {
		http.Error(w, "slot must be 'morning' or 'evening'", http.StatusBadRequest)
		return
	}
	if sub.ScheduleType == "" {
		sub.ScheduleType = "daily"
	}
	if len(sub.ActiveDays) == 0 {
		sub.ActiveDays = []int{0, 1, 2, 3, 4, 5, 6}
	}
	if sub.AnchorDate == "" {
		sub.AnchorDate = time.Now().Format("2006-01-02")
	}

	id, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}
	sub.Id = id

	// Two things to notice in this statement, both about stop_order.
	//
	// In VALUES it's COALESCE($19, 0). The column is INT NOT NULL DEFAULT 0,
	// and Postgres does NOT substitute a column default when you explicitly
	// pass NULL — it rejects the row. Since every schedule/quantity edit
	// omits routing, $19 is nil on the common path, and without the COALESCE
	// this endpoint failed with a not-null violation on every call.
	//
	// In DO UPDATE the routing columns reference $18/$19 directly rather than
	// EXCLUDED. That matters: EXCLUDED.stop_order is the post-COALESCE value
	// (0), so COALESCE(EXCLUDED.stop_order, ...) would resolve to 0 and wipe
	// an existing customer's stop position every time someone edited their
	// milk quantity. Reading the raw parameter preserves the distinction
	// between "not supplied" and "supplied as zero".
	query := `
		INSERT INTO subscriptions (
			id, customer_id, slot, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty,
			route_id, stop_order, item_schedules
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17,
			$18, COALESCE($19, 0), $20
		)
		ON CONFLICT (customer_id, slot)
		DO UPDATE SET
			schedule_type = EXCLUDED.schedule_type,
			active_days = EXCLUDED.active_days,
			anchor_date = EXCLUDED.anchor_date,
			default_milk_qty = EXCLUDED.default_milk_qty,
			default_curd_qty = EXCLUDED.default_curd_qty,
			default_butter_qty = EXCLUDED.default_butter_qty,
			default_ghee_qty = EXCLUDED.default_ghee_qty,
			default_lassi_qty = EXCLUDED.default_lassi_qty,
			default_paneer_qty = EXCLUDED.default_paneer_qty,
			default_jaggery_qty = EXCLUDED.default_jaggery_qty,
			default_khand_qty = EXCLUDED.default_khand_qty,
			default_oil_qty = EXCLUDED.default_oil_qty,
			default_atta_qty = EXCLUDED.default_atta_qty,
			default_burfi_qty = EXCLUDED.default_burfi_qty,
			-- COALESCE, not EXCLUDED: an admin editing a quantity sends no
			-- item_schedules, and taking EXCLUDED directly would wipe the
			-- customer's per-item frequencies every time. Same reasoning as
			-- route_id and stop_order below.
			item_schedules = COALESCE($20, subscriptions.item_schedules),
			route_id = COALESCE($18, subscriptions.route_id),
			stop_order = COALESCE($19, subscriptions.stop_order),
			updated_at = NOW();
	`
	// nil when the caller sent no schedules, so COALESCE in the upsert keeps
	// whatever the customer already had. An admin editing a milk quantity
	// must not silently clear someone's alternate-day butter.
	var encodedSchedules []byte
	if len(sub.ItemSchedules) > 0 {
		encodedSchedules, err = schedule.ValidateItemSchedules(sub.ItemSchedules, sub.AnchorDate)
		if err != nil {
			http.Error(w, "Delivery frequency: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	_, err = sr.DB.Exec(
		r.Context(), query,
		sub.Id, sub.CustomerId, sub.Slot, sub.ScheduleType, sub.ActiveDays, sub.AnchorDate,
		sub.DefaultOrder.Milk, sub.DefaultOrder.Curd, sub.DefaultOrder.Butter, sub.DefaultOrder.Ghee,
		sub.DefaultOrder.Lassi, sub.DefaultOrder.Paneer, sub.DefaultOrder.Jaggery, sub.DefaultOrder.Khand,
		sub.DefaultOrder.Oil, sub.DefaultOrder.Atta, sub.DefaultOrder.Burfi,
		sub.RouteId, sub.StopOrder, encodedSchedules,
	)
	if err != nil {
		http.Error(w, "Failed to save subscription: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Subscription schedule saved successfully",
		"slot":    sub.Slot,
	})
}

const selectColumns = `
	id, customer_id, slot, schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
	default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
	default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
	default_oil_qty, default_atta_qty, default_burfi_qty,
	route_id, stop_order, item_schedules
`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(
		&s.Id, &s.CustomerId, &s.Slot, &s.ScheduleType, &s.ActiveDays, &s.AnchorDate,
		&s.DefaultOrder.Milk, &s.DefaultOrder.Curd, &s.DefaultOrder.Butter, &s.DefaultOrder.Ghee,
		&s.DefaultOrder.Lassi, &s.DefaultOrder.Paneer, &s.DefaultOrder.Jaggery, &s.DefaultOrder.Khand,
		&s.DefaultOrder.Oil, &s.DefaultOrder.Atta, &s.DefaultOrder.Burfi,
		&s.RouteId, &s.StopOrder, &s.ItemSchedulesRaw,
	)
	if err != nil {
		return s, err
	}
	s.ItemSchedules = schedule.ParseItemSchedules(s.ItemSchedulesRaw)
	return s, nil
}

// ListByCustomer returns every slot this customer is subscribed to — zero,
// one, or two rows. The admin customer screen renders one panel per slot from
// this.
func (sr SubscriptionResource) ListByCustomer(w http.ResponseWriter, r *http.Request) {
	customerId := chi.URLParam(r, "customerId")

	rows, err := sr.DB.Query(r.Context(),
		`SELECT `+selectColumns+` FROM subscriptions WHERE customer_id = $1 ORDER BY slot ASC`,
		customerId)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	subs := []Subscription{}
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		subs = append(subs, s)
	}

	writeJSON(w, http.StatusOK, subs)
}

// GetOne fetches a single slot's plan.
func (sr SubscriptionResource) GetOne(w http.ResponseWriter, r *http.Request) {
	customerId := chi.URLParam(r, "customerId")
	slot := chi.URLParam(r, "slot")

	if !validSlots[slot] {
		http.Error(w, "slot must be 'morning' or 'evening'", http.StatusBadRequest)
		return
	}

	s, err := scanSubscription(sr.DB.QueryRow(r.Context(),
		`SELECT `+selectColumns+` FROM subscriptions WHERE customer_id = $1 AND slot = $2`,
		customerId, slot))
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "No "+slot+" subscription for this customer", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, s)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
