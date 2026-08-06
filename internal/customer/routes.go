package customer

// Customer is now identity-only. Route assignment, stop order, schedule and
// quantities all live on subscriptions — a customer can have up to two
// subscription rows (morning / evening), each independently routed. See
// internal/subscription for that half.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
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

type CustomerResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
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

	query := `
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status
		FROM customers
		ORDER BY id DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := cr.DB.Query(r.Context(), query, limit, offset)
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
		if notifyErr := cr.WhatsApp.SendApprovalNotification(phoneNumber, name); notifyErr != nil {
			fmt.Printf("❌ Failed to send approval notification to %s: %v\n", phoneNumber, notifyErr)
		}
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
		if notifyErr := cr.WhatsApp.SendRejectionNotification(phoneNumber, name, req.Reason); notifyErr != nil {
			fmt.Printf("❌ Failed to send rejection notification to %s: %v\n", phoneNumber, notifyErr)
		}
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
