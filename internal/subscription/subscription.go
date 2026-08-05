package subscription

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
	ScheduleType string         `json:"scheduleType"` // "daily", "custom", or "alternate"
	ActiveDays   []int          `json:"activeDays"`   // e.g., [1, 3, 5] for Mon/Wed/Fri
	AnchorDate   string         `json:"anchorDate"`   // e.g., "2026-07-10"
	DefaultOrder customer.Order `json:"defaultOrder"`
}

type SubscriptionResource struct {
	DB *pgxpool.Pool
}

func (sr SubscriptionResource) Routes() chi.Router {
	r := chi.NewRouter()
	// A single Upsert endpoint handles both creation and updates!
	r.Post("/", sr.CreateOrUpdate)
	r.Get("/customer/{customerId}", sr.GetByCustomer)
	return r
}

func (sr SubscriptionResource) CreateOrUpdate(w http.ResponseWriter, r *http.Request) {
	var sub Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	u, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}
	sub.Id = u
	if sub.ScheduleType == "" {
		sub.ScheduleType = "daily"
	}
	if len(sub.ActiveDays) == 0 {
		sub.ActiveDays = []int{0, 1, 2, 3, 4, 5, 6}
	}
	if sub.AnchorDate == "" {
		sub.AnchorDate = time.Now().Format("2006-01-02")
	}

	query := `
		INSERT INTO subscriptions (
			id, customer_id, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13,
			$14, $15, $16
		)
		ON CONFLICT (customer_id)
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
			updated_at = NOW();
	`
	_, err = sr.DB.Exec(
		r.Context(), query,
		sub.Id, sub.CustomerId, sub.ScheduleType, sub.ActiveDays, sub.AnchorDate,
		sub.DefaultOrder.Milk, sub.DefaultOrder.Curd, sub.DefaultOrder.Butter, sub.DefaultOrder.Ghee,
		sub.DefaultOrder.Lassi, sub.DefaultOrder.Paneer, sub.DefaultOrder.Jaggery, sub.DefaultOrder.Khand,
		sub.DefaultOrder.Oil, sub.DefaultOrder.Atta, sub.DefaultOrder.Burfi,
	)
	if err != nil {
		http.Error(w, "Failed to save subscription: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Subscription schedule saved successfully",
	})
}

func (sr SubscriptionResource) GetByCustomer(w http.ResponseWriter, r *http.Request) {
	customerId := chi.URLParam(r, "customerId")

	var sub Subscription
	// 🚀 CRITICAL FIX: TO_CHAR(anchor_date, 'YYYY-MM-DD') prevents pgx from crashing
	// when mapping a Postgres Date column into a Go String variable.
	query := `
		SELECT id, customer_id, schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty
		FROM subscriptions
		WHERE customer_id = $1
	`
	err := sr.DB.QueryRow(r.Context(), query, customerId).Scan(
		&sub.Id, &sub.CustomerId, &sub.ScheduleType, &sub.ActiveDays, &sub.AnchorDate,
		&sub.DefaultOrder.Milk, &sub.DefaultOrder.Curd, &sub.DefaultOrder.Butter, &sub.DefaultOrder.Ghee,
		&sub.DefaultOrder.Lassi, &sub.DefaultOrder.Paneer, &sub.DefaultOrder.Jaggery, &sub.DefaultOrder.Khand,
		&sub.DefaultOrder.Oil, &sub.DefaultOrder.Atta, &sub.DefaultOrder.Burfi,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "No subscription found for this customer", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sub)
}
