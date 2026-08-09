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

// SlotState is everything the page needs to render one slot: the customer's
// standing order for it, and any overrides they've already scheduled.
// Overrides are keyed by date string.
type SlotState struct {
	Subscribed   bool                      `json:"subscribed"`
	ScheduleType string                    `json:"scheduleType"`
	ActiveDays   []int                     `json:"activeDays"`
	AnchorDate   string                    `json:"anchorDate"`
	DefaultOrder customer.Order            `json:"defaultOrder"`
	Overrides    map[string]customer.Order `json:"overrides"`
}

// CustomerState carries both slots. A customer subscribed to morning only
// gets Evening.Subscribed = false, and the page hides that tab entirely.
type CustomerState struct {
	Morning SlotState `json:"morning"`
	Evening SlotState `json:"evening"`

	// Cutoff hour (HH:MM) after which today can no longer be changed. Sent
	// so the page can grey out today's chip rather than letting the customer
	// fill in a form that the server will reject.
	MorningCutoff string `json:"morningCutoff"`
}

func newSlotState() SlotState {
	return SlotState{Overrides: make(map[string]customer.Order)}
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
		Morning:       newSlotState(),
		Evening:       newSlotState(),
		MorningCutoff: "03:00",
	}

	// Best-effort: if system_config is missing we keep the 03:00 default
	// rather than failing the whole page.
	var cutoff string
	if err := or.DB.QueryRow(ctx,
		`SELECT TO_CHAR(morning_cutoff_time, 'HH24:MI') FROM system_config LIMIT 1`,
	).Scan(&cutoff); err == nil && cutoff != "" {
		state.MorningCutoff = cutoff
	}

	// --- Subscriptions, one row per slot ---
	subRows, err := or.DB.Query(ctx, `
        SELECT slot, schedule_type, active_days, TO_CHAR(anchor_date, 'YYYY-MM-DD'),
               default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
               default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
               default_oil_qty, default_atta_qty, default_burfi_qty
        FROM subscriptions WHERE customer_id = $1`, customerID)
	if err != nil {
		http.Error(w, "Failed to load subscription", http.StatusInternalServerError)
		return
	}
	defer subRows.Close()

	for subRows.Next() {
		var slot string
		s := newSlotState()
		if err := subRows.Scan(
			&slot, &s.ScheduleType, &s.ActiveDays, &s.AnchorDate,
			&s.DefaultOrder.Milk, &s.DefaultOrder.Curd, &s.DefaultOrder.Butter, &s.DefaultOrder.Ghee,
			&s.DefaultOrder.Lassi, &s.DefaultOrder.Paneer, &s.DefaultOrder.Jaggery, &s.DefaultOrder.Khand,
			&s.DefaultOrder.Oil, &s.DefaultOrder.Atta, &s.DefaultOrder.Burfi,
		); err != nil {
			continue
		}
		s.Subscribed = true
		if slot == "evening" {
			s.Overrides = state.Evening.Overrides
			state.Evening = s
		} else {
			s.Overrides = state.Morning.Overrides
			state.Morning = s
		}
	}

	// --- Existing overrides, also per slot ---
	ovRows, err := or.DB.Query(ctx, `
        SELECT slot, target_date, new_milk_qty, new_curd_qty, new_butter_qty, new_ghee_qty,
               new_lassi_qty, new_paneer_qty, new_jaggery_qty, new_khand_qty,
               new_oil_qty, new_atta_qty, new_burfi_qty
        FROM order_overrides
        WHERE customer_id = $1 AND target_date >= CURRENT_DATE`, customerID)
	if err != nil {
		http.Error(w, "Failed to load overrides", http.StatusInternalServerError)
		return
	}
	defer ovRows.Close()

	for ovRows.Next() {
		var slot string
		var date time.Time
		var o customer.Order
		if err := ovRows.Scan(
			&slot, &date, &o.Milk, &o.Curd, &o.Butter, &o.Ghee,
			&o.Lassi, &o.Paneer, &o.Jaggery, &o.Khand,
			&o.Oil, &o.Atta, &o.Burfi,
		); err != nil {
			continue
		}
		key := date.Format("2006-01-02")
		if slot == "evening" {
			state.Evening.Overrides[key] = o
		} else {
			state.Morning.Overrides[key] = o
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

type WebSubmitPayload struct {
	Token  string         `json:"token"`
	Slot   string         `json:"slot"` // "morning" | "evening"; defaults to morning
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

	// Anything that isn't explicitly "evening" is treated as morning. Keeps
	// a stale cached copy of the pre-slots page working instead of writing
	// rows with an invalid slot that the CHECK constraint would reject.
	if payload.Slot != "evening" {
		payload.Slot = "morning"
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

	// The customer must actually be subscribed to the slot they're editing —
	// otherwise an override row would exist for a slot with no standing
	// order, and the manifest would never pick it up.
	var subscribed bool
	or.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM subscriptions WHERE customer_id = $1 AND slot = $2)`,
		customerID, payload.Slot,
	).Scan(&subscribed)
	if !subscribed {
		http.Error(w, "You don't have a "+payload.Slot+" subscription to change.", http.StatusBadRequest)
		return
	}

	// Cutoff now comes from system_config rather than a hardcoded 03:00, so
	// the admin settings screen actually controls it. Falls back to 3 AM if
	// the row is missing.
	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	todayStr := now.Format("2006-01-02")

	cutoffHour, cutoffMin := 3, 0
	var cutoffStr string
	if err := or.DB.QueryRow(ctx,
		`SELECT TO_CHAR(morning_cutoff_time, 'HH24:MI') FROM system_config LIMIT 1`,
	).Scan(&cutoffStr); err == nil && cutoffStr != "" {
		var h, m int
		if _, err := fmt.Sscanf(cutoffStr, "%d:%d", &h, &m); err == nil {
			cutoffHour, cutoffMin = h, m
		}
	}
	cutoffTime := time.Date(now.Year(), now.Month(), now.Day(), cutoffHour, cutoffMin, 0, 0, loc)

	for _, targetDate := range payload.Dates {
		if targetDate == todayStr && now.After(cutoffTime) {
			http.Error(w, fmt.Sprintf("Cannot alter today's order after the %s cutoff.", cutoffTime.Format("3:04 PM")), http.StatusForbidden)
			return
		}
		// Two-week horizon, enforced server-side. The page only renders 14
		// chips, but the API shouldn't rely on the client for that.
		if t, err := time.ParseInLocation("2006-01-02", targetDate, loc); err == nil {
			if t.After(now.AddDate(0, 0, 14)) {
				http.Error(w, "You can only change orders up to 2 weeks ahead.", http.StatusBadRequest)
				return
			}
		}
	}

	tx, err := or.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "Could not save your changes. Please try again.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	if payload.Action == "delete" {
		for _, date := range payload.Dates {
			if _, err := tx.Exec(ctx,
				"DELETE FROM order_overrides WHERE customer_id = $1 AND target_date = $2 AND slot = $3",
				customerID, date, payload.Slot,
			); err != nil {
				http.Error(w, "Could not cancel that override.", http.StatusInternalServerError)
				return
			}
		}
	} else {
		// Conflict target is (customer_id, target_date, slot), matching the
		// unique_customer_target_date_slot constraint from the slots
		// migration. The old two-column target no longer exists and made
		// every override submission fail at plan time.
		query := `
            INSERT INTO order_overrides (
                id, customer_id, target_date, slot,
                new_milk_qty, new_curd_qty, new_butter_qty, new_ghee_qty,
                new_lassi_qty, new_paneer_qty, new_jaggery_qty, new_khand_qty,
                new_oil_qty, new_atta_qty, new_burfi_qty
            ) VALUES (
                $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
            )
            ON CONFLICT (customer_id, target_date, slot) DO UPDATE SET
                new_milk_qty = EXCLUDED.new_milk_qty, new_curd_qty = EXCLUDED.new_curd_qty,
                new_butter_qty = EXCLUDED.new_butter_qty, new_ghee_qty = EXCLUDED.new_ghee_qty,
                new_lassi_qty = EXCLUDED.new_lassi_qty, new_paneer_qty = EXCLUDED.new_paneer_qty,
                new_jaggery_qty = EXCLUDED.new_jaggery_qty, new_khand_qty = EXCLUDED.new_khand_qty,
                new_oil_qty = EXCLUDED.new_oil_qty, new_atta_qty = EXCLUDED.new_atta_qty,
                new_burfi_qty = EXCLUDED.new_burfi_qty;`

		for _, date := range payload.Dates {
			u, _ := uuid.NewV7()
			if _, err := tx.Exec(ctx, query, u, customerID, date, payload.Slot,
				payload.Items.Milk, payload.Items.Curd, payload.Items.Butter, payload.Items.Ghee,
				payload.Items.Lassi, payload.Items.Paneer, payload.Items.Jaggery, payload.Items.Khand,
				payload.Items.Oil, payload.Items.Atta, payload.Items.Burfi,
			); err != nil {
				http.Error(w, "Could not save that change.", http.StatusInternalServerError)
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "Could not save your changes. Please try again.", http.StatusInternalServerError)
		return
	}
	registration.MarkUsed(ctx, or.DB, payload.Token)

	// --- GENERATE DETAILED WHATSAPP NOTIFICATION ---
	if or.WhatsApp != nil {
		var formattedDates []string
		for _, d := range payload.Dates {
			if t, err := time.Parse("2006-01-02", d); err == nil {
				formattedDates = append(formattedDates, t.Format("Jan 02"))
			} else {
				formattedDates = append(formattedDates, d)
			}
		}
		dateStr := strings.Join(formattedDates, ", ")
		slotLabel := strings.Title(payload.Slot)

		var msg string
		if payload.Action == "delete" {
			msg = fmt.Sprintf("✅ Your %s override has been cancelled for: *%s*.\n\nThese dates will return to your default schedule.", strings.ToLower(slotLabel), dateStr)
		} else {
			totalQty := payload.Items.Milk + payload.Items.Curd + payload.Items.Butter + payload.Items.Ghee + payload.Items.Lassi + payload.Items.Paneer + payload.Items.Jaggery + payload.Items.Khand + payload.Items.Oil + payload.Items.Atta + payload.Items.Burfi
			if totalQty == 0 {
				msg = fmt.Sprintf("⏸️ Your *%s* deliveries have been paused for: *%s*.", slotLabel, dateStr)
			} else {
				orderDetails := buildOrderString(payload.Items)
				msg = fmt.Sprintf("✅ Your *%s* order has been updated for: *%s*.\n\n*New Order:*\n%s", slotLabel, dateStr, orderDetails)
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
			return fmt.Sprintf("%.1fL", float64(qty)/1000)
		}
		return fmt.Sprintf("%dml", qty)
	}
	if qty >= 1000 {
		return fmt.Sprintf("%.1fkg", float64(qty)/1000)
	}
	return fmt.Sprintf("%dg", qty)
}

func buildOrderString(o customer.Order) string {
	items := []struct {
		label    string
		qty      int
		isLiquid bool
	}{
		{"Milk", o.Milk, true},
		{"Curd", o.Curd, false},
		{"Butter", o.Butter, false},
		{"Ghee", o.Ghee, false},
		{"Buttermilk", o.Lassi, true},
		{"Paneer", o.Paneer, false},
		{"Jaggery", o.Jaggery, false},
		{"Desi Khand", o.Khand, false},
		{"Mustard Oil", o.Oil, true},
		{"Atta", o.Atta, false},
		{"Milk Burfi", o.Burfi, false},
	}

	var lines []string
	for _, it := range items {
		if it.qty > 0 {
			lines = append(lines, fmt.Sprintf("• %s: %s", it.label, formatUnit(it.qty, it.isLiquid)))
		}
	}
	if len(lines) == 0 {
		return "• (No items)"
	}
	return strings.Join(lines, "\n")
}
