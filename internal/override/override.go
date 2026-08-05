package override

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/registration"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/override.html
var templatesFS embed.FS
var overrideTmpl = template.Must(template.ParseFS(templatesFS, "templates/override.html"))

type OverrideResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (or OverrideResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", or.ShowForm)
	r.Get("/state", or.GetState)
	r.Post("/", or.Submit)
	return r
}

func (or OverrideResource) ShowForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := registration.ValidatePhone(ctx, or.DB, token); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding:60px 20px;">
            <h2>This link has expired</h2>
            <p>Please request a new override link from WhatsApp.</p>
        </body></html>`)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	overrideTmpl.Execute(w, map[string]string{"Token": token})
}

type CustomerState struct {
	DefaultOrder customer.Order            `json:"defaultOrder"`
	Overrides    map[string]customer.Order `json:"overrides"`
}

func (or OverrideResource) GetState(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, or.DB, token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	var customerID uuid.UUID
	err = or.DB.QueryRow(ctx, "SELECT id FROM customers WHERE phone_number = $1", phone).Scan(&customerID)
	if err != nil {
		http.Error(w, "Customer not found", http.StatusNotFound)
		return
	}

	state := CustomerState{
		Overrides: make(map[string]customer.Order),
	}

	subQuery := `
        SELECT default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
               default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
               default_oil_qty, default_atta_qty, default_burfi_qty
        FROM subscriptions WHERE customer_id = $1`
	or.DB.QueryRow(ctx, subQuery, customerID).Scan(
		&state.DefaultOrder.Milk, &state.DefaultOrder.Curd, &state.DefaultOrder.Butter, &state.DefaultOrder.Ghee,
		&state.DefaultOrder.Lassi, &state.DefaultOrder.Paneer, &state.DefaultOrder.Jaggery, &state.DefaultOrder.Khand,
		&state.DefaultOrder.Oil, &state.DefaultOrder.Atta, &state.DefaultOrder.Burfi,
	)

	overrideQuery := `
        SELECT target_date, new_milk_qty, new_curd_qty, new_butter_qty, new_ghee_qty,
               new_lassi_qty, new_paneer_qty, new_jaggery_qty, new_khand_qty,
               new_oil_qty, new_atta_qty, new_burfi_qty
        FROM order_overrides
        WHERE customer_id = $1 AND target_date >= CURRENT_DATE`
	rows, _ := or.DB.Query(ctx, overrideQuery, customerID)
	defer rows.Close()

	for rows.Next() {
		var date time.Time
		var o customer.Order
		rows.Scan(
			&date, &o.Milk, &o.Curd, &o.Butter, &o.Ghee,
			&o.Lassi, &o.Paneer, &o.Jaggery, &o.Khand,
			&o.Oil, &o.Atta, &o.Burfi,
		)
		state.Overrides[date.Format("2006-01-02")] = o
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

type WebSubmitPayload struct {
	Token  string         `json:"token"`
	Dates  []string       `json:"dates"`
	Action string         `json:"action"` // "set" or "delete"
	Items  customer.Order `json:"items"`
}

func (or OverrideResource) Submit(w http.ResponseWriter, r *http.Request) {
	var payload WebSubmitPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, or.DB, payload.Token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	var customerID uuid.UUID
	var customerName string
	or.DB.QueryRow(ctx, "SELECT id, name FROM customers WHERE phone_number = $1", phone).Scan(&customerID, &customerName)

	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	todayStr := now.Format("2006-01-02")
	cutoffTime := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, loc)

	for _, targetDate := range payload.Dates {
		if targetDate == todayStr && now.After(cutoffTime) {
			http.Error(w, "Cannot alter today's order after the 3:00 AM cutoff.", http.StatusForbidden)
			return
		}
	}

	tx, _ := or.DB.Begin(ctx)
	defer tx.Rollback(ctx)

	if payload.Action == "delete" {
		for _, date := range payload.Dates {
			tx.Exec(ctx, "DELETE FROM order_overrides WHERE customer_id = $1 AND target_date = $2", customerID, date)
		}
	} else {
		query := `
            INSERT INTO order_overrides (
                id, customer_id, target_date,
                new_milk_qty, new_curd_qty, new_butter_qty, new_ghee_qty,
                new_lassi_qty, new_paneer_qty, new_jaggery_qty, new_khand_qty,
                new_oil_qty, new_atta_qty, new_burfi_qty
            ) VALUES (
                $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
            )
            ON CONFLICT (customer_id, target_date) DO UPDATE SET
                new_milk_qty = EXCLUDED.new_milk_qty, new_curd_qty = EXCLUDED.new_curd_qty,
                new_butter_qty = EXCLUDED.new_butter_qty, new_ghee_qty = EXCLUDED.new_ghee_qty,
                new_lassi_qty = EXCLUDED.new_lassi_qty, new_paneer_qty = EXCLUDED.new_paneer_qty,
                new_jaggery_qty = EXCLUDED.new_jaggery_qty, new_khand_qty = EXCLUDED.new_khand_qty,
                new_oil_qty = EXCLUDED.new_oil_qty, new_atta_qty = EXCLUDED.new_atta_qty,
                new_burfi_qty = EXCLUDED.new_burfi_qty;`

		for _, date := range payload.Dates {
			u, _ := uuid.NewV7()
			tx.Exec(ctx, query, u, customerID, date,
				payload.Items.Milk, payload.Items.Curd, payload.Items.Butter, payload.Items.Ghee,
				payload.Items.Lassi, payload.Items.Paneer, payload.Items.Jaggery, payload.Items.Khand,
				payload.Items.Oil, payload.Items.Atta, payload.Items.Burfi,
			)
		}
	}

	tx.Commit(ctx)
	registration.MarkUsed(ctx, or.DB, payload.Token)

	// --- GENERATE DETAILED WHATSAPP NOTIFICATION ---
	if or.WhatsApp != nil {
		// Format dates cleanly (e.g., "2026-08-04" -> "Aug 04")
		var formattedDates []string
		for _, d := range payload.Dates {
			if t, err := time.Parse("2006-01-02", d); err == nil {
				formattedDates = append(formattedDates, t.Format("Jan 02"))
			} else {
				formattedDates = append(formattedDates, d)
			}
		}
		dateStr := strings.Join(formattedDates, ", ")

		var msg string
		if payload.Action == "delete" {
			msg = fmt.Sprintf("✅ Your override has been cancelled for: *%s*.\n\nThese dates will return to your default schedule.", dateStr)
		} else {
			// Check if this is a "Pause" (all items 0)
			totalQty := payload.Items.Milk + payload.Items.Curd + payload.Items.Butter + payload.Items.Ghee + payload.Items.Lassi + payload.Items.Paneer + payload.Items.Jaggery + payload.Items.Khand + payload.Items.Oil + payload.Items.Atta + payload.Items.Burfi
			if totalQty == 0 {
				msg = fmt.Sprintf("⏸️ Your deliveries have been paused for: *%s*.", dateStr)
			} else {
				orderDetails := buildOrderString(payload.Items)
				msg = fmt.Sprintf("✅ Your order has been updated for: *%s*.\n\n*New Order:*\n%s", dateStr, orderDetails)
			}
		}
		or.WhatsApp.SendDeliveryUpdate(phone, msg)
	}

	w.WriteHeader(http.StatusOK)
}

// --- HELPER FUNCTIONS FOR NOTIFICATIONS ---

func formatUnit(qty int, isLiquid bool) string {
	if qty == 0 {
		return ""
	}
	if isLiquid {
		if qty >= 1000 {
			return fmt.Sprintf("%.1f L", float64(qty)/1000)
		}
		return fmt.Sprintf("%d ml", qty)
	}
	if qty >= 1000 {
		return fmt.Sprintf("%.1f kg", float64(qty)/1000)
	}
	return fmt.Sprintf("%d g", qty)
}

func buildOrderString(o customer.Order) string {
	var parts []string
	if o.Milk > 0 {
		parts = append(parts, "🥛 Milk: "+formatUnit(o.Milk, true))
	}
	if o.Curd > 0 {
		parts = append(parts, "🍶 Curd: "+formatUnit(o.Curd, true))
	}
	if o.Butter > 0 {
		parts = append(parts, "🧈 Butter: "+formatUnit(o.Butter, false))
	}
	if o.Ghee > 0 {
		parts = append(parts, "🫙 Ghee: "+formatUnit(o.Ghee, false))
	}
	if o.Lassi > 0 {
		parts = append(parts, "🥤 Lassi: "+formatUnit(o.Lassi, true))
	}
	if o.Paneer > 0 {
		parts = append(parts, "🧀 Paneer: "+formatUnit(o.Paneer, false))
	}
	if o.Jaggery > 0 {
		parts = append(parts, "🍯 Jaggery: "+formatUnit(o.Jaggery, false))
	}
	if o.Khand > 0 {
		parts = append(parts, "🍚 Khand: "+formatUnit(o.Khand, false))
	}
	if o.Oil > 0 {
		parts = append(parts, "🫗 Oil: "+formatUnit(o.Oil, true))
	}
	if o.Atta > 0 {
		parts = append(parts, "🌾 Atta: "+formatUnit(o.Atta, false))
	}
	if o.Burfi > 0 {
		parts = append(parts, "🍬 Burfi: "+formatUnit(o.Burfi, false))
	}
	return strings.Join(parts, "\n")
}
