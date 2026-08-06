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
// is" (via COALESCE against the existing row), so editing someone's Tuesday
// order can never accidentally un-route them. Routing is customer.Approve's
// job, or an explicit call here that does include them.
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

	query := `
		INSERT INTO subscriptions (
			id, customer_id, slot, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty,
			route_id, stop_order
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17,
			$18, $19
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
			route_id = COALESCE(EXCLUDED.route_id, subscriptions.route_id),
			stop_order = COALESCE(EXCLUDED.stop_order, subscriptions.stop_order),
			updated_at = NOW();
	`
	_, err = sr.DB.Exec(
		r.Context(), query,
		sub.Id, sub.CustomerId, sub.Slot, sub.ScheduleType, sub.ActiveDays, sub.AnchorDate,
		sub.DefaultOrder.Milk, sub.DefaultOrder.Curd, sub.DefaultOrder.Butter, sub.DefaultOrder.Ghee,
		sub.DefaultOrder.Lassi, sub.DefaultOrder.Paneer, sub.DefaultOrder.Jaggery, sub.DefaultOrder.Khand,
		sub.DefaultOrder.Oil, sub.DefaultOrder.Atta, sub.DefaultOrder.Burfi,
		sub.RouteId, sub.StopOrder,
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
	route_id, stop_order
`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var sub Subscription
	err := row.Scan(
		&sub.Id, &sub.CustomerId, &sub.Slot, &sub.ScheduleType, &sub.ActiveDays, &sub.AnchorDate,
		&sub.DefaultOrder.Milk, &sub.DefaultOrder.Curd, &sub.DefaultOrder.Butter, &sub.DefaultOrder.Ghee,
		&sub.DefaultOrder.Lassi, &sub.DefaultOrder.Paneer, &sub.DefaultOrder.Jaggery, &sub.DefaultOrder.Khand,
		&sub.DefaultOrder.Oil, &sub.DefaultOrder.Atta, &sub.DefaultOrder.Burfi,
		&sub.RouteId, &sub.StopOrder,
	)
	return sub, err
}

// ListByCustomer returns every slot a customer has — zero, one, or two rows.
// This is what the admin customer screen calls to render "Morning: Route A,
// stop 4" and "Evening: unrouted" as separate cards.
func (sr SubscriptionResource) ListByCustomer(w http.ResponseWriter, r *http.Request) {
	customerId := chi.URLParam(r, "customerId")

	rows, err := sr.DB.Query(r.Context(), "SELECT "+selectColumns+" FROM subscriptions WHERE customer_id = $1 ORDER BY slot ASC", customerId)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	subs := []Subscription{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		subs = append(subs, sub)
	}

	writeJSON(w, http.StatusOK, subs)
}

// GetOne returns a single slot's subscription — used wherever code needs to
// check exactly one plan (e.g. before allowing an override for that slot).
func (sr SubscriptionResource) GetOne(w http.ResponseWriter, r *http.Request) {
	customerId := chi.URLParam(r, "customerId")
	slot := chi.URLParam(r, "slot")

	sub, err := scanSubscription(sr.DB.QueryRow(r.Context(), "SELECT "+selectColumns+" FROM subscriptions WHERE customer_id = $1 AND slot = $2", customerId, slot))
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "No subscription found for this customer and slot", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, sub)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
