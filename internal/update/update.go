package update

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/registration"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/update.html
var templatesFS embed.FS
var updateTmpl = template.Must(template.ParseFS(templatesFS, "templates/update.html"))

type UpdateResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (ur UpdateResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", ur.ShowForm)
	r.Get("/state", ur.GetState)
	r.Post("/", ur.Submit)
	return r
}

func (ur UpdateResource) ShowForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := registration.ValidatePhone(ctx, ur.DB, token); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding:60px 20px;">
            <h2>This link has expired</h2>
            <p>Please request a new link from WhatsApp.</p>
        </body></html>`)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	updateTmpl.Execute(w, map[string]string{"Token": token})
}

type FullState struct {
	Name         string         `json:"name"`
	Address      string         `json:"address"`
	Latitude     string         `json:"latitude"`
	Longitude    string         `json:"longitude"`
	ScheduleType string         `json:"scheduleType"`
	ActiveDays   []int          `json:"activeDays"`
	StartDate    string         `json:"startDate"`
	DefaultOrder customer.Order `json:"defaultOrder"`
}

func (ur UpdateResource) GetState(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, ur.DB, token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	var state FullState
	var customerID string

	err = ur.DB.QueryRow(ctx, "SELECT id, name, house_address, geo_latitude, geo_longitude FROM customers WHERE phone_number = $1", phone).Scan(
		&customerID, &state.Name, &state.Address, &state.Latitude, &state.Longitude,
	)
	if err != nil {
		http.Error(w, "Customer not found", http.StatusNotFound)
		return
	}

	subQuery := `
        SELECT schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
               default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
               default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
               default_oil_qty, default_atta_qty, default_burfi_qty
        FROM subscriptions WHERE customer_id = $1`
	ur.DB.QueryRow(ctx, subQuery, customerID).Scan(
		&state.ScheduleType, &state.ActiveDays, &state.StartDate,
		&state.DefaultOrder.Milk, &state.DefaultOrder.Curd, &state.DefaultOrder.Butter, &state.DefaultOrder.Ghee,
		&state.DefaultOrder.Lassi, &state.DefaultOrder.Paneer, &state.DefaultOrder.Jaggery, &state.DefaultOrder.Khand,
		&state.DefaultOrder.Oil, &state.DefaultOrder.Atta, &state.DefaultOrder.Burfi,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// Reuses the SubmitPayload structure from registration
type UpdatePayload struct {
	Token        string         `json:"token"`
	Name         string         `json:"name"`
	Address      string         `json:"address"`
	Latitude     string         `json:"latitude"`
	Longitude    string         `json:"longitude"`
	ScheduleType string         `json:"scheduleType"`
	ActiveDays   []int          `json:"activeDays"`
	StartDate    string         `json:"startDate"`
	Items        customer.Order `json:"items"`
}

func (ur UpdateResource) Submit(w http.ResponseWriter, r *http.Request) {
	var payload UpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, ur.DB, payload.Token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	tx, _ := ur.DB.Begin(ctx)
	defer tx.Rollback(ctx)

	var customerID string
	// 1. Update Customer AND set them to pending, dropping their route_id
	custQuery := `
		UPDATE customers
		SET name = $1, house_address = $2, geo_latitude = $3, geo_longitude = $4,
		    status = 'pending', is_active = false, route_id = NULL
		WHERE phone_number = $5 RETURNING id`
	err = tx.QueryRow(ctx, custQuery, payload.Name, payload.Address, payload.Latitude, payload.Longitude, phone).Scan(&customerID)
	if err != nil {
		http.Error(w, "Failed to update profile", http.StatusInternalServerError)
		return
	}

	// 2. Update Subscription
	anchorDate := time.Now().Format("2006-01-02")
	if payload.ScheduleType == "alternate" && payload.StartDate != "" {
		anchorDate = payload.StartDate
	}

	subQuery := `
		UPDATE subscriptions SET
			schedule_type = $1, active_days = $2, anchor_date = $3,
			default_milk_qty = $4, default_curd_qty = $5, default_butter_qty = $6, default_ghee_qty = $7,
			default_lassi_qty = $8, default_paneer_qty = $9, default_jaggery_qty = $10, default_khand_qty = $11,
			default_oil_qty = $12, default_atta_qty = $13, default_burfi_qty = $14
		WHERE customer_id = $15`
	_, err = tx.Exec(ctx, subQuery,
		payload.ScheduleType, payload.ActiveDays, anchorDate,
		payload.Items.Milk, payload.Items.Curd, payload.Items.Butter, payload.Items.Ghee,
		payload.Items.Lassi, payload.Items.Paneer, payload.Items.Jaggery, payload.Items.Khand,
		payload.Items.Oil, payload.Items.Atta, payload.Items.Burfi,
		customerID,
	)
	if err != nil {
		http.Error(w, "Failed to update subscription", http.StatusInternalServerError)
		return
	}

	tx.Commit(ctx)
	registration.MarkUsed(ctx, ur.DB, payload.Token)

	// Send WhatsApp confirmation
	if ur.WhatsApp != nil {
		msg := "✅ Your details have been updated successfully.\n\nBecause your address or schedule changed, your account is temporarily pending while our team reviews the changes to assign your delivery route.\n\nWe will notify you once deliveries are ready to resume!"
		ur.WhatsApp.SendDeliveryUpdate(phone, msg)
	}

	w.WriteHeader(http.StatusOK)
}
