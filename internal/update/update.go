package update

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

//go:embed templates/update.html
var templatesFS embed.FS
var updateTmpl = template.Must(template.ParseFS(templatesFS, "templates/update.html"))

type UpdateResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
	// Templates carries the approved Twilio content template SIDs. An admin
	// reviews a change whenever they get to the queue, possibly a day or more
	// after the customer submitted it — so both the approval and the decline
	// must be templates or they silently never arrive.
	Templates notification.TemplateSIDs
}

func (ur UpdateResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Customer-facing (token-gated)
	r.Get("/", ur.ShowForm)
	r.Get("/state", ur.GetState)
	r.Post("/", ur.Submit)

	// Admin: staged change review
	r.Get("/changes", ur.ListPendingChanges)
	r.Post("/changes/{id}/approve", ur.ApprovePendingChange)
	r.Post("/changes/{id}/reject", ur.RejectPendingChange)

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

// PendingSummary tells the page a change is already under review, so it can
// show a banner and disable the form rather than letting the customer submit
// something that will just be refused.
type PendingSummary struct {
	SubmittedAt time.Time `json:"submittedAt"`
	Status      string    `json:"status"`
}

type FullState struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`

	Morning SlotSubscription `json:"morning"`
	Evening SlotSubscription `json:"evening"`

	Pending *PendingSummary `json:"pending"`
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

	// One row per slot.
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

	// Is something already under review?
	var ps PendingSummary
	err = ur.DB.QueryRow(ctx, `
		SELECT submitted_at, review_status
		FROM pending_subscription_changes
		WHERE customer_id = $1 AND review_status = 'PENDING'
		LIMIT 1
	`, customerID).Scan(&ps.SubmittedAt, &ps.Status)
	if err == nil {
		state.Pending = &ps
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// UpdatePayload is what update.html submits.
//
// Subscriptions is authoritative: any slot NOT present in the array is
// removed when the change is eventually applied. That's how a customer
// cancels just their evening delivery while keeping morning.
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

// Submit stages a change for admin review. It does NOT modify the customer's
// live subscription, address, or active status.
//
// The previous version wrote straight into subscriptions and set
// is_active=false, which meant a customer editing their order at 10 AM lost
// that day's delivery entirely (the manifest filters on is_active) and had
// their account suspended. It also handed them a back door around the
// override cutoff: they couldn't legitimately change today's order, but they
// could silently cancel it.
//
// Now the request parks in pending_subscription_changes and the customer
// carries on receiving deliveries on their existing order until an admin
// approves it — at which point it takes effect from the following day,
// because production for today has already been planned.
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

	// Drop empty slots — submitting a slot with all zeros means "cancel this
	// slot", which the apply step handles by deleting it.
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

	// ONE SLOT PER CUSTOMER (v1) — same rule as registration.
	//
	// This endpoint is the other route to a second slot: a customer who
	// registered for mornings could otherwise add evenings here. The form
	// enforces it with mutually exclusive toggles; this is the guarantee.
	if len(payload.Subscriptions) > 1 {
		http.Error(w, "Please choose just one delivery slot — morning or evening.", http.StatusBadRequest)
		return
	}

	var customerID string
	var currentAddress, currentLat, currentLng string
	err = ur.DB.QueryRow(ctx, `
		SELECT id, house_address, COALESCE(geo_latitude, ''), COALESCE(geo_longitude, '')
		FROM customers WHERE phone_number = $1
	`, phone).Scan(&customerID, &currentAddress, &currentLat, &currentLng)
	if err != nil {
		http.Error(w, "Customer not found", http.StatusNotFound)
		return
	}

	// Flagged for the admin review screen so an address move is obvious
	// without diffing two blobs by eye — those are the ones needing a
	// re-route, not just a quantity tweak.
	addressChanged := payload.Address != currentAddress ||
		payload.Latitude != currentLat ||
		payload.Longitude != currentLng

	subsJSON, err := json.Marshal(payload.Subscriptions)
	if err != nil {
		http.Error(w, "Could not save your changes", http.StatusInternalServerError)
		return
	}

	changeID, _ := uuid.NewV7()
	_, err = ur.DB.Exec(ctx, `
		INSERT INTO pending_subscription_changes
			(id, customer_id, name, house_address, geo_latitude, geo_longitude,
			 subscriptions, address_changed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, changeID, customerID, payload.Name, payload.Address,
		payload.Latitude, payload.Longitude, subsJSON, addressChanged)

	if err != nil {
		// The partial unique index on (customer_id) WHERE review_status =
		// 'PENDING' is what rejects a second open request. Catching it here
		// rather than pre-checking avoids a read-then-write race.
		if strings.Contains(err.Error(), "idx_pending_change_one_per_customer") ||
			strings.Contains(err.Error(), "duplicate key") {
			http.Error(w, "You already have a change under review. Please wait for our team to confirm it before submitting another.", http.StatusConflict)
			return
		}
		http.Error(w, "Could not save your changes: "+err.Error(), http.StatusInternalServerError)
		return
	}

	registration.MarkUsed(ctx, ur.DB, payload.Token)

	if ur.WhatsApp != nil {
		msg := "📝 We've received your requested changes.\n\n" +
			"*Your deliveries continue as normal in the meantime* — nothing changes until our team reviews this.\n\n" +
			"Once approved, your new order starts from the following day. We'll message you either way."
		go ur.WhatsApp.SendDeliveryUpdate(phone, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "pending_review",
		"message": "Change request submitted for review",
	})
}
