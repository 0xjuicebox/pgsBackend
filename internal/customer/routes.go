package customer

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

// We keep the Order struct here so other packages (like overrides and subscriptions) can reuse it!
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
	// admin review + route assignment), "active" (approved, receiving deliveries),
	// or "disabled" (was active, customer paused). Distinct from IsActive so pause
	// and pending-approval don't collide.
	Status    string     `json:"status"`
	StopOrder int        `json:"stopOrder"`
	RouteId   *uuid.UUID `json:"routeId"`
	// Notice: defaultOrder is completely GONE. Identity only!
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
		r.Post("/reject", cr.Reject) // <-- Add this
	})

	return r
}

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

	// New signups always start pending — admin has to approve + assign a route
	// before they go active, regardless of what the caller sent.
	c.Status = "pending"
	c.IsActive = false

	query := `
		INSERT INTO customers (
			id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8
		)
	`

	_, err = cr.DB.Exec(
		r.Context(), query,
		c.Id, c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive, c.Status,
	)

	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      c.Id.String(),
		"message": "Customer identity created successfully — pending admin approval",
	})
}

func (cr CustomerResource) List(w http.ResponseWriter, r *http.Request) {
	page := r.Context().Value(middleware.PageKey).(int)
	limit := r.Context().Value(middleware.LimitKey).(int)
	offset := (page - 1) * limit

	// 🚀 Fixed: Removed non-existent created_at column
	query := `
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status, stop_order, route_id
		FROM customers
		ORDER BY id DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := cr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError) // Optional: add +err.Error() so you see exact errors next time!
		return
	}
	defer rows.Close()
	customers := []Customer{}
	for rows.Next() {
		var c Customer
		err := rows.Scan(
			&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.Status, &c.StopOrder, &c.RouteId,
		)
		if err != nil {
			http.Error(w, "Scan error", http.StatusInternalServerError)
			return
		}
		customers = append(customers, c)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(customers)
}

func (cr CustomerResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status, stop_order, route_id
		FROM customers
		WHERE id = $1
	`
	var c Customer
	err := cr.DB.QueryRow(r.Context(), query, id).Scan(
		&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.Status, &c.StopOrder, &c.RouteId,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

func (cr CustomerResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var c Customer
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE customers
		SET name = $1, phone_number = $2, house_address = $3, geo_latitude = $4, geo_longitude = $5, is_active = $6, status = $7, stop_order = $8, route_id = $9
		WHERE id = $10
	`

	_, err := cr.DB.Exec(
		r.Context(), query,
		c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive, c.Status, c.StopOrder, c.RouteId, id,
	)

	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer identity updated successfully"})
}

// ApproveRequest is what your admin dashboard sends when approving a pending
// customer: which route to put them on and their stop order within that route.
type ApproveRequest struct {
	RouteId   uuid.UUID `json:"routeId"`
	StopOrder int       `json:"stopOrder"`
}

// Approve moves a customer from "pending" to "active": assigns them to a
// route + stop order, flips is_active on, and automatically sends them a
// WhatsApp notification letting them know they're approved.
func (cr CustomerResource) Approve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE customers
		SET status = 'active', is_active = true, route_id = $1, stop_order = $2
		WHERE id = $3 AND status = 'pending'
		RETURNING phone_number, name
	`

	var phoneNumber, name string
	err := cr.DB.QueryRow(r.Context(), query, req.RouteId, req.StopOrder, id).Scan(&phoneNumber, &name)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer not found or not in pending status", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Fire the WhatsApp notification automatically. If this fails, we don't
	// fail the whole request — the approval itself already succeeded in the DB.
	if cr.WhatsApp != nil {
		if notifyErr := cr.WhatsApp.SendApprovalNotification(phoneNumber, name); notifyErr != nil {
			fmt.Printf("❌ Failed to send approval notification to %s: %v\n", phoneNumber, notifyErr)
		}
	} else {
		fmt.Printf("⚠️ WhatsApp service not configured on CustomerResource — skipped approval notification for %s\n", phoneNumber)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":     "Customer approved, assigned to route, and notified via WhatsApp",
		"phoneNumber": phoneNumber,
		"name":        name,
	})
}

func (cr CustomerResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	tx, err := cr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	// 1. Clean up dependent foreign key records safely
	_, _ = tx.Exec(r.Context(), `DELETE FROM subscriptions WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM order_overrides WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM delivery_logs WHERE customer_id = $1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM invoices WHERE customer_id = $1`, id)

	// 2. Delete the customer record
	query := `DELETE FROM customers WHERE id = $1`
	_, err = tx.Exec(r.Context(), query, id)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer deleted successfully"})
}

type RejectRequest struct {
	Reason string `json:"reason"`
}

// Reject moves a customer from "pending" to "rejected" and fires a WhatsApp notification.
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

	// Fire the Rejection WhatsApp notification
	if cr.WhatsApp != nil {
		if notifyErr := cr.WhatsApp.SendRejectionNotification(phoneNumber, name, req.Reason); notifyErr != nil {
			fmt.Printf("❌ Failed to send rejection notification to %s: %v\n", phoneNumber, notifyErr)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":     "Customer rejected successfully",
		"phoneNumber": phoneNumber,
		"name":        name,
	})
}
