package registration

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/register.html
var templatesFS embed.FS

var registerTmpl = template.Must(template.ParseFS(templatesFS, "templates/register.html"))

type Resource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (rr Resource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", rr.ShowForm)
	r.Post("/", rr.Submit)
	return r
}

// ShowForm serves the registration page for a valid token, or a plain
// "link expired" message otherwise.
func (rr Resource) ShowForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing registration token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := ValidatePhone(ctx, rr.DB, token); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `<html><body style="font-family:sans-serif;text-align:center;padding:60px 20px;">
			<h2>This registration link has expired</h2>
			<p>Please message us on WhatsApp again to get a fresh link.</p>
		</body></html>`)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if err := registerTmpl.Execute(w, map[string]string{"Token": token}); err != nil {
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
	}
}

// SlotSubscription is one slot's standing order. A customer can register for
// morning only, evening only, or both — each with its own schedule and its
// own product quantities.
type SlotSubscription struct {
	Slot         string         `json:"slot"`         // "morning" | "evening"
	ScheduleType string         `json:"scheduleType"` // "daily" | "alternate" | "custom"
	ActiveDays   []int          `json:"activeDays"`   // only meaningful for "custom"
	StartDate    string         `json:"startDate"`    // only meaningful for "alternate", YYYY-MM-DD
	Items        map[string]int `json:"items"`        // item key -> quantity
}

// SubmitPayload mirrors what register.html's JS sends as JSON.
//
// The Subscriptions array is the current shape. The flat ScheduleType/
// ActiveDays/StartDate/Items fields are the pre-slots shape, kept so a
// browser holding a cached copy of the old page still registers successfully
// instead of silently failing. normalize() collapses the legacy form into a
// single morning subscription.
type SubmitPayload struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`

	Subscriptions []SlotSubscription `json:"subscriptions"`

	// Legacy (pre-slots) fields.
	ScheduleType string         `json:"scheduleType"`
	ActiveDays   []int          `json:"activeDays"`
	StartDate    string         `json:"startDate"`
	Items        map[string]int `json:"items"`
}

// normalize guarantees Subscriptions is populated and every entry has a valid
// slot. Called immediately after decode so the rest of Submit never has to
// think about which shape arrived.
func (p *SubmitPayload) normalize() {
	if len(p.Subscriptions) == 0 && len(p.Items) > 0 {
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

// Submit validates the token, creates the customer (status "pending") and one
// subscription row per requested slot, consumes the token, and sends a
// WhatsApp confirmation.
func (rr Resource) Submit(w http.ResponseWriter, r *http.Request) {
	var payload SubmitPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	payload.normalize()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	phone, err := ValidatePhone(ctx, rr.DB, payload.Token)
	if err != nil {
		http.Error(w, "This registration link is invalid or has expired. Please message us on WhatsApp for a new one.", http.StatusGone)
		return
	}

	if payload.Name == "" || payload.Address == "" {
		http.Error(w, "Name and address are required", http.StatusBadRequest)
		return
	}

	// At least one slot must carry at least one item. Slots that arrive
	// empty are dropped rather than rejected — a customer who fills in
	// morning and leaves evening blank means "morning only".
	kept := payload.Subscriptions[:0]
	for _, s := range payload.Subscriptions {
		total := 0
		for _, q := range s.Items {
			total += q
		}
		if total > 0 {
			kept = append(kept, s)
		}
	}
	payload.Subscriptions = kept

	if len(payload.Subscriptions) == 0 {
		http.Error(w, "Please select at least one item", http.StatusBadRequest)
		return
	}

	// Customer + subscriptions go in one transaction: a customer row with no
	// subscription would show up in the admin approval queue as an
	// un-approvable ghost.
	tx, err := rr.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "Failed to save your details. Please try again.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	// New signups always start pending.
	customerID, _ := uuid.NewV7()
	var finalCustomerID uuid.UUID
	customerQuery := `
		INSERT INTO customers (id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status)
		VALUES ($1, $2, $3, $4, $5, $6, false, 'pending')
		ON CONFLICT (phone_number)
		DO UPDATE SET name = EXCLUDED.name, house_address = EXCLUDED.house_address,
			geo_latitude = EXCLUDED.geo_latitude, geo_longitude = EXCLUDED.geo_longitude
		RETURNING id;
	`
	err = tx.QueryRow(ctx, customerQuery,
		customerID, payload.Name, phone, payload.Address, payload.Latitude, payload.Longitude,
	).Scan(&finalCustomerID)
	if err != nil {
		http.Error(w, "Failed to save your details. Please try again.", http.StatusInternalServerError)
		return
	}

	// The conflict target is (customer_id, slot) — matching the
	// unique_customer_slot constraint added by the slots migration. The old
	// (customer_id) target no longer exists and made this statement fail at
	// plan time, which is why registration was returning 500s.
	subQuery := `
		INSERT INTO subscriptions (
			id, customer_id, slot, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_paneer_qty,
			default_butter_qty, default_ghee_qty, default_lassi_qty,
			default_jaggery_qty, default_khand_qty, default_oil_qty,
			default_atta_qty, default_burfi_qty
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
		)
		ON CONFLICT (customer_id, slot)
		DO UPDATE SET
			schedule_type = EXCLUDED.schedule_type, active_days = EXCLUDED.active_days, anchor_date = EXCLUDED.anchor_date,
			default_milk_qty = EXCLUDED.default_milk_qty, default_curd_qty = EXCLUDED.default_curd_qty,
			default_paneer_qty = EXCLUDED.default_paneer_qty, default_butter_qty = EXCLUDED.default_butter_qty,
			default_ghee_qty = EXCLUDED.default_ghee_qty, default_lassi_qty = EXCLUDED.default_lassi_qty,
			default_jaggery_qty = EXCLUDED.default_jaggery_qty, default_khand_qty = EXCLUDED.default_khand_qty,
			default_oil_qty = EXCLUDED.default_oil_qty, default_atta_qty = EXCLUDED.default_atta_qty,
			default_burfi_qty = EXCLUDED.default_burfi_qty;
	`

	for _, sub := range payload.Subscriptions {
		activeDays := sub.ActiveDays
		if sub.ScheduleType != "custom" {
			activeDays = []int{0, 1, 2, 3, 4, 5, 6}
		}

		anchorDate := time.Now().Format("2006-01-02")
		if sub.ScheduleType == "alternate" && sub.StartDate != "" {
			anchorDate = sub.StartDate
		}

		subID, _ := uuid.NewV7()
		_, err = tx.Exec(ctx, subQuery,
			subID, finalCustomerID, sub.Slot, sub.ScheduleType, activeDays, anchorDate,
			sub.Items["milk"], sub.Items["curd"], sub.Items["paneer"],
			sub.Items["butter"], sub.Items["ghee"], sub.Items["lassi"],
			sub.Items["jaggery"], sub.Items["khand"], sub.Items["oil"],
			sub.Items["atta"], sub.Items["burfi"],
		)
		if err != nil {
			http.Error(w, "Failed to save your order. Please try again.", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "Failed to save your order. Please try again.", http.StatusInternalServerError)
		return
	}

	if err := MarkUsed(ctx, rr.DB, payload.Token); err != nil {
		fmt.Printf("⚠️ Failed to mark registration token used: %v\n", err)
		// Non-fatal — registration itself already succeeded.
	}

	if rr.WhatsApp != nil {
		slotWord := "deliveries"
		if len(payload.Subscriptions) == 1 {
			slotWord = payload.Subscriptions[0].Slot + " deliveries"
		}
		msg := fmt.Sprintf(
			"✅ Thanks %s! We've received your registration for %s.\n\nOur team will review your details and confirm once your deliveries are scheduled to begin. 🥛",
			payload.Name, slotWord,
		)
		if err := rr.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
			fmt.Printf("⚠️ Failed to send registration confirmation to %s: %v\n", phone, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Registration received!"})
}
