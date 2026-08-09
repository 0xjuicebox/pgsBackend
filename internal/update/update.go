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
	"github.com/gofrs/uuid/v5"
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

// SlotSubscription is one slot's standing order as the page reads and writes
// it. Subscribed=false means the customer has no subscription for that slot;
// the page renders it as an "Add evening delivery" prompt.
type SlotSubscription struct {
	Slot         string         `json:"slot"`
	Subscribed   bool           `json:"subscribed"`
	ScheduleType string         `json:"scheduleType"`
	ActiveDays   []int          `json:"activeDays"`
	StartDate    string         `json:"startDate"`
	Items        customer.Order `json:"items"`
}

type FullState struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`

	Morning SlotSubscription `json:"morning"`
	Evening SlotSubscription `json:"evening"`
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

	state := FullState{
		Morning: SlotSubscription{Slot: "morning"},
		Evening: SlotSubscription{Slot: "evening"},
	}
	var customerID string

	err = ur.DB.QueryRow(ctx, "SELECT id, name, house_address, geo_latitude, geo_longitude FROM customers WHERE phone_number = $1", phone).Scan(
		&customerID, &state.Name, &state.Address, &state.Latitude, &state.Longitude,
	)
	if err != nil {
		http.Error(w, "Customer not found", http.StatusNotFound)
		return
	}

	// One row per slot. Previously this used QueryRow with no slot filter,
	// which returned whichever row Postgres happened to hand back first —
	// so a two-slot customer saw a random slot's data in the form.
	rows, err := ur.DB.Query(ctx, `
        SELECT slot, schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
               default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
               default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
               default_oil_qty, default_atta_qty, default_burfi_qty
        FROM subscriptions WHERE customer_id = $1`, customerID)
	if err != nil {
		http.Error(w, "Failed to load subscriptions", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var slot string
		var s SlotSubscription
		if err := rows.Scan(
			&slot, &s.ScheduleType, &s.ActiveDays, &s.StartDate,
			&s.Items.Milk, &s.Items.Curd, &s.Items.Butter, &s.Items.Ghee,
			&s.Items.Lassi, &s.Items.Paneer, &s.Items.Jaggery, &s.Items.Khand,
			&s.Items.Oil, &s.Items.Atta, &s.Items.Burfi,
		); err != nil {
			continue
		}
		s.Subscribed = true
		s.Slot = slot
		if slot == "evening" {
			state.Evening = s
		} else {
			state.Morning = s
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// UpdatePayload is what update.html submits.
//
// Subscriptions is authoritative: any slot NOT present in the array is
// removed. That's how a customer cancels just their evening delivery while
// keeping morning — they submit with only the morning entry.
type UpdatePayload struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`

	Subscriptions []SlotSubscription `json:"subscriptions"`

	// Legacy (pre-slots) fields, normalized into Subscriptions on decode.
	ScheduleType string         `json:"scheduleType"`
	ActiveDays   []int          `json:"activeDays"`
	StartDate    string         `json:"startDate"`
	Items        customer.Order `json:"items"`
}

func (p *UpdatePayload) normalize() {
	if len(p.Subscriptions) == 0 {
		p.Subscriptions = []SlotSubscription{{
			Slot:         "morning",
			ScheduleType: p.ScheduleType,
			ActiveDays:   p.ActiveDays,
			StartDate:    p.StartDate,
			Items:        p.Items,
		}}
	}
	for i := range p.Subscriptions {
		if p.Subscriptions[i].Slot != "evening" {
			p.Subscriptions[i].Slot = "morning"
		}
	}
}

func orderTotal(o customer.Order) int {
	return o.Milk + o.Curd + o.Butter + o.Ghee + o.Lassi + o.Paneer +
		o.Jaggery + o.Khand + o.Oil + o.Atta + o.Burfi
}

func (ur UpdateResource) Submit(w http.ResponseWriter, r *http.Request) {
	var payload UpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	payload.normalize()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, ur.DB, payload.Token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	if payload.Name == "" || payload.Address == "" {
		http.Error(w, "Name and address are required", http.StatusBadRequest)
		return
	}

	// Drop empty slots — a slot submitted with all zeros means "cancel this
	// slot", which the delete step below handles.
	kept := payload.Subscriptions[:0]
	for _, s := range payload.Subscriptions {
		if orderTotal(s.Items) > 0 {
			kept = append(kept, s)
		}
	}
	payload.Subscriptions = kept

	if len(payload.Subscriptions) == 0 {
		http.Error(w, "Please keep at least one item on at least one delivery slot. To stop deliveries entirely, reply PAUSE on WhatsApp.", http.StatusBadRequest)
		return
	}

	tx, err := ur.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "Failed to update profile", http.StatusInternalServerError)
		return
	}
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

	// 2. Upsert each submitted slot. Scoped by (customer_id, slot) — the old
	// statement filtered on customer_id alone and overwrote BOTH slots with
	// the same values for anyone subscribed to morning and evening.
	subQuery := `
		INSERT INTO subscriptions (
			id, customer_id, slot, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
		)
		ON CONFLICT (customer_id, slot) DO UPDATE SET
			schedule_type = EXCLUDED.schedule_type, active_days = EXCLUDED.active_days,
			anchor_date = EXCLUDED.anchor_date,
			default_milk_qty = EXCLUDED.default_milk_qty, default_curd_qty = EXCLUDED.default_curd_qty,
			default_butter_qty = EXCLUDED.default_butter_qty, default_ghee_qty = EXCLUDED.default_ghee_qty,
			default_lassi_qty = EXCLUDED.default_lassi_qty, default_paneer_qty = EXCLUDED.default_paneer_qty,
			default_jaggery_qty = EXCLUDED.default_jaggery_qty, default_khand_qty = EXCLUDED.default_khand_qty,
			default_oil_qty = EXCLUDED.default_oil_qty, default_atta_qty = EXCLUDED.default_atta_qty,
			default_burfi_qty = EXCLUDED.default_burfi_qty`

	submitted := map[string]bool{}
	for _, s := range payload.Subscriptions {
		submitted[s.Slot] = true

		activeDays := s.ActiveDays
		if s.ScheduleType != "custom" {
			activeDays = []int{0, 1, 2, 3, 4, 5, 6}
		}
		anchorDate := time.Now().Format("2006-01-02")
		if s.ScheduleType == "alternate" && s.StartDate != "" {
			anchorDate = s.StartDate
		}

		subID, _ := uuid.NewV7()
		_, err = tx.Exec(ctx, subQuery,
			subID, customerID, s.Slot, s.ScheduleType, activeDays, anchorDate,
			s.Items.Milk, s.Items.Curd, s.Items.Butter, s.Items.Ghee,
			s.Items.Lassi, s.Items.Paneer, s.Items.Jaggery, s.Items.Khand,
			s.Items.Oil, s.Items.Atta, s.Items.Burfi,
		)
		if err != nil {
			http.Error(w, "Failed to update subscription", http.StatusInternalServerError)
			return
		}
	}

	// 3. Remove slots the customer dropped. Future overrides for that slot go
	// too — leaving them would resurrect deliveries for a slot with no
	// standing order. Past delivery_logs are untouched: they're billing
	// history and must survive a subscription change.
	for _, slot := range []string{"morning", "evening"} {
		if submitted[slot] {
			continue
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM order_overrides WHERE customer_id = $1 AND slot = $2 AND target_date >= CURRENT_DATE`,
			customerID, slot,
		); err != nil {
			http.Error(w, "Failed to clear old schedule", http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM subscriptions WHERE customer_id = $1 AND slot = $2`,
			customerID, slot,
		); err != nil {
			http.Error(w, "Failed to update subscription", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "Failed to update subscription", http.StatusInternalServerError)
		return
	}
	registration.MarkUsed(ctx, ur.DB, payload.Token)

	// Send WhatsApp confirmation
	if ur.WhatsApp != nil {
		msg := "✅ Your details have been updated successfully.\n\nBecause your address or schedule changed, your account is temporarily pending while our team reviews the changes to assign your delivery route.\n\nWe will notify you once deliveries are ready to resume!"
		ur.WhatsApp.SendDeliveryUpdate(phone, msg)
	}

	w.WriteHeader(http.StatusOK)
}
