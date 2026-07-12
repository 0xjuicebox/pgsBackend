package override

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type OverridePayload struct {
	CustomerId  uuid.UUID      `json:"customerId"`
	TargetDates []string       `json:"targetDates"` // Upgraded to handle an array of dates
	NewOrder    customer.Order `json:"newOrder"`
}

type OverrideResource struct {
	DB *pgxpool.Pool
}

func (or OverrideResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Create or Update bulk overrides
	r.Post("/", or.CreateBulk)

	return r
}

func (or OverrideResource) CreateBulk(w http.ResponseWriter, r *http.Request) {
	var payload OverridePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Enforce the 3:00 AM Time-Lock for "Today"
	// Load the IST timezone (since you are based in India)
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.UTC // Fallback if timezone data is missing locally
	}

	now := time.Now().In(loc)
	todayStr := now.Format("2006-01-02")
	cutoffTime := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, loc)

	for _, targetDate := range payload.TargetDates {
		// If they are trying to change an order for today, and it is past 3:00 AM, block it!
		if targetDate == todayStr && now.After(cutoffTime) {
			http.Error(w, "Cannot alter today's order after the 3:00 AM cutoff. Dispatch is already locked.", http.StatusForbidden)
			return
		}
	}

	// 2. Begin a transaction for safe bulk insertion
	tx, err := or.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Failed to start transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	// 3. The UPSERT query for Overrides
	query := `
		INSERT INTO order_overrides (
			id, customer_id, target_date,
			new_milk_qty, new_curd_qty, new_butter_qty, new_ghee_qty,
			new_lassi_qty, new_paneer_qty, new_jaggery_qty, new_khand_qty,
			new_oil_qty, new_atta_qty, new_burfi_qty
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7,
			$8, $9, $10, $11,
			$12, $13, $14
		)
		ON CONFLICT (customer_id, target_date)
		DO UPDATE SET
			new_milk_qty = EXCLUDED.new_milk_qty,
			new_curd_qty = EXCLUDED.new_curd_qty,
			new_butter_qty = EXCLUDED.new_butter_qty,
			new_ghee_qty = EXCLUDED.new_ghee_qty,
			new_lassi_qty = EXCLUDED.new_lassi_qty,
			new_paneer_qty = EXCLUDED.new_paneer_qty,
			new_jaggery_qty = EXCLUDED.new_jaggery_qty,
			new_khand_qty = EXCLUDED.new_khand_qty,
			new_oil_qty = EXCLUDED.new_oil_qty,
			new_atta_qty = EXCLUDED.new_atta_qty,
			new_burfi_qty = EXCLUDED.new_burfi_qty;
	`

	// Loop through all dates requested, generate an ID, and UPSERT them
	for _, targetDate := range payload.TargetDates {
		overrideId, err := uuid.NewV7()
		if err != nil {
			http.Error(w, "ID generation failed for date "+targetDate, http.StatusInternalServerError)
			return
		}

		_, err = tx.Exec(
			r.Context(), query,
			overrideId, payload.CustomerId, targetDate,
			payload.NewOrder.Milk, payload.NewOrder.Curd, payload.NewOrder.Butter, payload.NewOrder.Ghee,
			payload.NewOrder.Lassi, payload.NewOrder.Paneer, payload.NewOrder.Jaggery, payload.NewOrder.Khand,
			payload.NewOrder.Oil, payload.NewOrder.Atta, payload.NewOrder.Burfi,
		)
		if err != nil {
			http.Error(w, "Failed to save override for date "+targetDate+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// 4. Commit the transaction to save all changes safely at once
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Transaction commit failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Order overrides successfully applied for requested dates",
	})
}
