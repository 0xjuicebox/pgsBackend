package customer

// Customer is now identity-only. Route assignment, stop order, schedule and
// quantities all live on subscriptions — a customer can have up to two
// subscription rows (morning / evening), each independently routed. See
// internal/subscription for that half.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Order is kept here so subscription/override/delivery can all reuse the same
// shape without an import cycle back to this package.
type Order struct {
	Milk    int `json:"milkQuantity"`
	Curd    int `json:"curdQuantity"`
	Butter  int `json:"butterQuantity"`
	Ghee    int `json:"gheeQuantity"`
	Lassi   int `json:"lassiQuantity"`
	Paneer  int `json:"paneerQuantity"`
	Jaggery int `json:"jaggeryQuantity"`
	Khand   int `json:"khandQuantity"`
	Oil     int `json:"oilQuantity"`
	Atta    int `json:"attaQuantity"`
	Burfi   int `json:"burfiQuantity"`
}

type Customer struct {
	Id           uuid.UUID `json:"id"`
	Name         string    `json:"customer"`
	PhoneNumber  string    `json:"phoneNumber"`
	HouseAddress string    `json:"houseAddress"`
	GeoLatitude  string    `json:"geoLatitude"`
	GeoLongitude string    `json:"geoLongitude"`
	IsActive     bool      `json:"isActive"`
	// Status drives the approval workflow: "pending" (just registered, awaiting
	// admin review), "active" (approved — at least one slot routed), "disabled"
	// (was active, customer paused), or "rejected".
	Status string `json:"status"`
}

// nonDigits strips formatting from a phone search term so "98765 43210"
// matches a stored "+919876543210".
var nonDigits = regexp.MustCompile(`[^0-9]`)

type CustomerResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
	// Templates carries the approved Twilio content template SIDs. Approval
	// and rejection happen whenever an admin gets to the queue — possibly
	// days after the customer registered — so both must be templates or they
	// silently never arrive.
	Templates notification.TemplateSIDs
}

func (cr CustomerResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.With(middleware.Paginate).Get("/", cr.List)
	r.Post("/", cr.Create)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", cr.Get)
		r.Put("/", cr.Update)
		r.Delete("/", cr.Delete)
		r.Post("/approve", cr.Approve)
		r.Post("/reject", cr.Reject)
	})

	return r
}

// -------------------------------------------------------------------------
// CRUD
// -------------------------------------------------------------------------

func (cr CustomerResource) Create(w http.ResponseWriter, r *http.Request) {
	var c Customer
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	u, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}
	c.Id = u

	// New signups always start pending — admin has to approve + route at
	// least one slot before they go active, regardless of what the caller sent.
	query := `
		INSERT INTO customers (id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status)
		VALUES ($1, $2, $3, $4, $5, $6, false, 'pending')
	`
	_, err = cr.DB.Exec(r.Context(), query, c.Id, c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	c.IsActive = false
	c.Status = "pending"
	writeJSON(w, http.StatusCreated, c)
}

func (cr CustomerResource) List(w http.ResponseWriter, r *http.Request) {
	page, _ := r.Context().Value(middleware.PageKey).(int)
	limit, _ := r.Context().Value(middleware.LimitKey).(int)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	offset := (page - 1) * limit

	// Server-side search and status filter.
	//
	// The admin list used to fetch one page and filter it in the browser,
	// which is fine at 100 customers and quietly broken above that: searching
	// for customer #400 returned nothing, indistinguishable from "no such
	// customer". Filtering has to happen where all the rows are.
	//
	// Phone is matched on digits only, because an admin reads a number off a
	// missed call or a bill and types it without the +91 or the spaces the
	// database has.
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))

	where := []string{"1=1"}
	args := []any{}

	if q != "" {
		digits := nonDigits.ReplaceAllString(q, "")
		args = append(args, "%"+strings.ToLower(q)+"%")
		namePos := len(args)
		clause := fmt.Sprintf("(LOWER(name) LIKE $%d OR LOWER(house_address) LIKE $%d", namePos, namePos)
		// Only search phone when the query has enough digits to be meaningful.
		// Two digits would match most of the table and make the result look
		// broken rather than filtered.
		if len(digits) >= 3 {
			args = append(args, "%"+digits+"%")
			clause += fmt.Sprintf(" OR REGEXP_REPLACE(phone_number, '[^0-9]', '', 'g') LIKE $%d", len(args))
		}
		clause += ")"
		where = append(where, clause)
	}

	if status != "" && status != "all" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}

	args = append(args, limit, offset)
	query := fmt.Sprintf(`
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status
		FROM customers
		WHERE %s
		ORDER BY name ASC
		LIMIT $%d OFFSET $%d
	`, strings.Join(where, " AND "), len(args)-1, len(args))

	rows, err := cr.DB.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	customers := []Customer{}
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.Status); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		customers = append(customers, c)
	}

	writeJSON(w, http.StatusOK, customers)
}

func (cr CustomerResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status
		FROM customers
		WHERE id = $1
	`
	var c Customer
	err := cr.DB.QueryRow(r.Context(), query, id).Scan(
		&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.Status,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// Update touches identity fields only. Route assignment now lives on
// subscriptions — see subscription.CreateOrUpdate and customer.Approve.
func (cr CustomerResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var c Customer
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE customers
		SET name = $1, phone_number = $2, house_address = $3, geo_latitude = $4, geo_longitude = $5,
		    is_active = $6, status = $7
		WHERE id = $8
	`
	_, err := cr.DB.Exec(r.Context(), query,
		c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive, c.Status, id,
	)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Customer identity updated successfully"})
}

func (cr CustomerResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Two separate blockers, reported separately so the admin knows which
	// action to take: settle a bill, or raise a final invoice.
	//
	// The second check exists because deletion cascades through
	// delivery_logs. A customer removed mid-month takes their uninvoiced
	// DELIVERED rows with them — revenue that was earned and then vanished
	// with no record anywhere. The WhatsApp self-service delete has always
	// checked this; the admin endpoint didn't, which meant the safer path
	// was the customer's and the riskier one was the admin's.
	var unpaidCount, unbilledDeliveries int
	err := cr.DB.QueryRow(r.Context(), `
		SELECT
			(SELECT COUNT(*) FROM invoices
			  WHERE customer_id = $1 AND status = 'PENDING'),
			(SELECT COUNT(*) FROM delivery_logs
			  WHERE customer_id = $1
			    AND status = 'DELIVERED'
			    AND delivery_date >= date_trunc('month', CURRENT_DATE)
			    AND NOT EXISTS (
			        SELECT 1 FROM invoices i
			        WHERE i.customer_id = $1
			          AND i.billing_month = TO_CHAR(CURRENT_DATE, 'YYYY-MM')
			    ))
	`, id).Scan(&unpaidCount, &unbilledDeliveries)
	if err != nil {
		http.Error(w, "Failed checking outstanding balance: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if unpaidCount > 0 {
		http.Error(w, "Cannot delete: customer has unpaid invoices. Mark them paid first.", http.StatusConflict)
		return
	}
	if unbilledDeliveries > 0 {
		http.Error(w, fmt.Sprintf(
			"Cannot delete: %d delivery(s) this month haven't been billed yet. Raise a final invoice for this customer first, then delete once it's settled.",
			unbilledDeliveries,
		), http.StatusConflict)
		return
	}

	tx, err := cr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	_, _ = tx.Exec(r.Context(), `DELETE FROM subscriptions WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM order_overrides WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM delivery_logs WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM invoices WHERE customer_id = $1`, id)

	if _, err := tx.Exec(r.Context(), `DELETE FROM customers WHERE id = $1`, id); err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Customer deleted successfully"})
}

// -------------------------------------------------------------------------
// Approval workflow
// -------------------------------------------------------------------------

var validSlots = map[string]bool{"morning": true, "evening": true}

// SlotAssignment is one line of an approval: which slot, which route, where in
// the driver's stop sequence.
type SlotAssignment struct {
	Slot      string    `json:"slot"`
	RouteId   uuid.UUID `json:"routeId"`
	StopOrder int       `json:"stopOrder"`
}

type ApproveRequest struct {
	Assignments []SlotAssignment `json:"assignments"`
}

// Approve moves a customer from "pending" to "active" by routing one or more
// of their subscription slots. Each assignment requires that a subscription
// row already exist for that slot — Approve routes an existing plan, it
// doesn't create one. That row is normally created when the customer places
// their order via WhatsApp/the registration form, or by an admin calling
// subscription.CreateOrUpdate directly.
//
// A customer with morning-only service just sends one assignment; approving
// morning alone is enough to flip them active. Evening can be added later by
// calling this again with just the evening assignment — the customer stays
// "active" throughout, so re-approval never has to reset their status.
func (cr CustomerResource) Approve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if len(req.Assignments) == 0 {
		http.Error(w, "At least one slot assignment is required", http.StatusBadRequest)
		return
	}
	for _, a := range req.Assignments {
		if !validSlots[a.Slot] {
			http.Error(w, "Invalid slot: "+a.Slot, http.StatusBadRequest)
			return
		}
	}

	tx, err := cr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	for _, a := range req.Assignments {
		tag, err := tx.Exec(r.Context(), `
			UPDATE subscriptions
			SET route_id = $1, stop_order = $2, updated_at = NOW()
			WHERE customer_id = $3 AND slot = $4
		`, a.RouteId, a.StopOrder, id, a.Slot)
		if err != nil {
			http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
			http.Error(w, fmt.Sprintf(
				"Customer has no %s subscription yet — save their order for that slot before approving it",
				a.Slot,
			), http.StatusConflict)
			return
		}
	}

	// priorStatus distinguishes a brand-new customer from one coming back
	// after a pause. Both land here — RESUME sets status to 'pending' and an
	// admin approves it through this same endpoint — but they need different
	// messages. Telling a six-month customer their "registration has been
	// approved" reads as though we'd lost their account.
	// Read the status before changing it. Both a brand-new registration and a
	// customer returning from a pause arrive here as 'pending' — RESUME sets
	// that status and an admin approves it through this same endpoint — so
	// the prior value is the only way to tell them apart, and they need
	// different messages.
	//
	// Read separately rather than via a subquery in RETURNING: that would
	// work, but only because of statement-snapshot semantics, which is not
	// something the next reader should have to know.
	var priorStatus string
	if err := tx.QueryRow(r.Context(),
		`SELECT status FROM customers WHERE id = $1`, id).Scan(&priorStatus); err != nil && err != pgx.ErrNoRows {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var phoneNumber, name string
	err = tx.QueryRow(r.Context(), `
		UPDATE customers
		SET status = 'active', is_active = true
		WHERE id = $1
		RETURNING phone_number, name
	`, id).Scan(&phoneNumber, &name)

	if err == pgx.ErrNoRows {
		http.Error(w, "Customer not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if cr.WhatsApp != nil {
		cr.notifyApproved(r.Context(), id, phoneNumber, name, priorStatus)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":     "Customer approved, routed, and notified via WhatsApp",
		"phoneNumber": phoneNumber,
		"name":        name,
	})
}

type RejectRequest struct {
	Reason string `json:"reason"`
}

func (cr CustomerResource) Reject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req RejectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "A predefined rejection reason is required", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE customers
		SET status = 'rejected', is_active = false
		WHERE id = $1 AND status = 'pending'
		RETURNING phone_number, name
	`
	var phoneNumber, name string
	err := cr.DB.QueryRow(r.Context(), query, id).Scan(&phoneNumber, &name)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer not found or already processed", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if cr.WhatsApp != nil {
		cr.notifyDeclined(phoneNumber, name, req.Reason)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":     "Customer rejected successfully",
		"phoneNumber": phoneNumber,
		"name":        name,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// -------------------------------------------------------------------------
// Approval and decline notifications
// -------------------------------------------------------------------------

// notifyApproved tells a customer their account is live, and when milk
// actually starts.
//
// priorStatus decides which message: 'disabled' means they were paused and
// are coming back, anything else means this is their first approval. Both
// paths carry the first delivery date and the order, because "you're
// approved" on its own left the customer guessing whether to buy milk that
// evening.
//
// Falls back to the old free-form message when a template isn't configured —
// which still works inside WhatsApp's 24-hour window, just not outside it.
func (cr CustomerResource) notifyApproved(ctx context.Context, customerID, phone, name, priorStatus string) {
	slots, err := schedule.LoadSlots(ctx, cr.DB, customerID)
	if err != nil {
		fmt.Printf("⚠️ notifyApproved: couldn't load schedule for %s: %v\n", customerID, err)
	}

	firstDelivery := schedule.FirstDeliveryLabel(slots, time.Now())
	order := cr.orderSummary(ctx, customerID)

	resuming := priorStatus == "disabled"

	sid := cr.Templates.RegApproved
	if resuming {
		sid = cr.Templates.ResumeApproved
	}

	if sid != "" {
		var err error
		if resuming {
			err = cr.WhatsApp.SendResumeApproved(phone, sid, name, firstDelivery, order)
		} else {
			err = cr.WhatsApp.SendRegistrationApproved(phone, sid, name, firstDelivery, order)
		}
		if err == nil {
			return
		}
		fmt.Printf("⚠️ approval template failed for %s, falling back: %v\n", phone, err)
	}

	if err := cr.WhatsApp.SendApprovalNotification(phone, name); err != nil {
		fmt.Printf("❌ Failed to send approval notification to %s: %v\n", phone, err)
	}
}

func (cr CustomerResource) notifyDeclined(phone, name, reason string) {
	if cr.Templates.RegDeclined != "" {
		if err := cr.WhatsApp.SendRegistrationDeclined(
			phone, cr.Templates.RegDeclined, name, reason); err == nil {
			return
		} else {
			fmt.Printf("⚠️ decline template failed for %s, falling back: %v\n", phone, err)
		}
	}
	if err := cr.WhatsApp.SendRejectionNotification(phone, name, reason); err != nil {
		fmt.Printf("❌ Failed to send rejection notification to %s: %v\n", phone, err)
	}
}

// orderSummary renders a customer's standing order as one comma-separated
// line: "Milk 2 L, Curd 500 g".
//
// Single line, not bulleted: this goes into a WhatsApp template variable, and
// Meta rejects any parameter containing a newline.
func (cr CustomerResource) orderSummary(ctx context.Context, customerID string) string {
	type item struct {
		label string
		col   string
		litre bool
	}
	items := []item{
		{"Milk", "default_milk_qty", true},
		{"Curd", "default_curd_qty", true},
		{"Buttermilk", "default_lassi_qty", true},
		{"Mustard Oil", "default_oil_qty", true},
		{"Butter", "default_butter_qty", false},
		{"Ghee", "default_ghee_qty", false},
		{"Paneer", "default_paneer_qty", false},
		{"Jaggery", "default_jaggery_qty", false},
		{"Desi Khand", "default_khand_qty", false},
		{"Atta", "default_atta_qty", false},
		{"Burfi", "default_burfi_qty", false},
	}

	cols := make([]string, len(items))
	for i, it := range items {
		cols[i] = "COALESCE(SUM(" + it.col + "),0)"
	}

	row := cr.DB.QueryRow(ctx,
		"SELECT "+strings.Join(cols, ", ")+" FROM subscriptions WHERE customer_id = $1", customerID)

	vals := make([]int, len(items))
	ptrs := make([]any, len(items))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		fmt.Printf("⚠️ orderSummary: %v\n", err)
		return "your usual order"
	}

	var parts []string
	for i, it := range items {
		if vals[i] <= 0 {
			continue
		}
		parts = append(parts, it.label+" "+formatBaseQty(vals[i], it.litre))
	}
	if len(parts) == 0 {
		return "your usual order"
	}
	return strings.Join(parts, ", ")
}

// formatBaseQty converts a base-unit quantity to how a customer reads it:
// 500 -> "500 ml", 2000 -> "2 L".
func formatBaseQty(v int, litre bool) string {
	if v < 1000 {
		if litre {
			return fmt.Sprintf("%d ml", v)
		}
		return fmt.Sprintf("%d g", v)
	}
	unit := "kg"
	if litre {
		unit = "L"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.2f", float64(v)/1000), ".00") + " " + unit
}
