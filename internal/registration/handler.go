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

// SubmitPayload mirrors what register.html's JS sends as JSON.
type SubmitPayload struct {
	Token        string         `json:"token"`
	Name         string         `json:"name"`
	Address      string         `json:"address"`
	Latitude     string         `json:"latitude"`
	Longitude    string         `json:"longitude"`
	ScheduleType string         `json:"scheduleType"` // "daily" | "alternate" | "custom"
	ActiveDays   []int          `json:"activeDays"`   // only meaningful for "custom"
	StartDate    string         `json:"startDate"`    // only meaningful for "alternate", format YYYY-MM-DD
	Items        map[string]int `json:"items"`        // item key -> quantity
}

// Submit validates the token, creates the customer (status "pending", exactly
// like the old Flow-based onboarding) and their subscription, consumes the
// token, and sends a WhatsApp confirmation.
func (rr Resource) Submit(w http.ResponseWriter, r *http.Request) {
	var payload SubmitPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

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

	totalQty := 0
	for _, q := range payload.Items {
		totalQty += q
	}
	if totalQty <= 0 {
		http.Error(w, "Please select at least one item", http.StatusBadRequest)
		return
	}

	// New signups always start pending — same rule as before, just now fed
	// by a web form instead of a WhatsApp Flow.
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
	err = rr.DB.QueryRow(ctx, customerQuery,
		customerID, payload.Name, phone, payload.Address, payload.Latitude, payload.Longitude,
	).Scan(&finalCustomerID)
	if err != nil {
		http.Error(w, "Failed to save your details. Please try again.", http.StatusInternalServerError)
		return
	}

	activeDays := payload.ActiveDays
	if payload.ScheduleType != "custom" {
		activeDays = []int{0, 1, 2, 3, 4, 5, 6}
	}

	anchorDate := time.Now().Format("2006-01-02")
	if payload.ScheduleType == "alternate" && payload.StartDate != "" {
		anchorDate = payload.StartDate
	}

	subID, _ := uuid.NewV7()
	subQuery := `
		INSERT INTO subscriptions (
			id, customer_id, schedule_type, active_days, anchor_date,
			default_milk_qty, default_curd_qty, default_paneer_qty,
			default_butter_qty, default_ghee_qty, default_lassi_qty,
			default_jaggery_qty, default_khand_qty, default_oil_qty,
			default_atta_qty, default_burfi_qty
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
		)
		ON CONFLICT (customer_id)
		DO UPDATE SET
			schedule_type = EXCLUDED.schedule_type, active_days = EXCLUDED.active_days, anchor_date = EXCLUDED.anchor_date,
			default_milk_qty = EXCLUDED.default_milk_qty, default_curd_qty = EXCLUDED.default_curd_qty,
			default_paneer_qty = EXCLUDED.default_paneer_qty, default_butter_qty = EXCLUDED.default_butter_qty,
			default_ghee_qty = EXCLUDED.default_ghee_qty, default_lassi_qty = EXCLUDED.default_lassi_qty,
			default_jaggery_qty = EXCLUDED.default_jaggery_qty, default_khand_qty = EXCLUDED.default_khand_qty,
			default_oil_qty = EXCLUDED.default_oil_qty, default_atta_qty = EXCLUDED.default_atta_qty,
			default_burfi_qty = EXCLUDED.default_burfi_qty;
	`
	_, err = rr.DB.Exec(ctx, subQuery,
		subID, finalCustomerID, payload.ScheduleType, activeDays, anchorDate,
		payload.Items["milk"], payload.Items["curd"], payload.Items["paneer"],
		payload.Items["butter"], payload.Items["ghee"], payload.Items["lassi"],
		payload.Items["jaggery"], payload.Items["khand"], payload.Items["oil"],
		payload.Items["atta"], payload.Items["burfi"],
	)
	if err != nil {
		http.Error(w, "Failed to save your order. Please try again.", http.StatusInternalServerError)
		return
	}

	if err := MarkUsed(ctx, rr.DB, payload.Token); err != nil {
		fmt.Printf("⚠️ Failed to mark registration token used: %v\n", err)
		// Non-fatal — registration itself already succeeded.
	}

	if rr.WhatsApp != nil {
		msg := fmt.Sprintf(
			"✅ Thanks %s! We've received your registration.\n\nOur team will review your details and confirm once your deliveries are scheduled to begin. 🥛",
			payload.Name,
		)
		if err := rr.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
			fmt.Printf("⚠️ Failed to send registration confirmation to %s: %v\n", phone, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Registration received!"})
}
