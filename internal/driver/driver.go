package driver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/route"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Driver struct {
	Id          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	PhoneNumber string    `json:"PhoneNumber"`
	IsActive    bool      `json:"isActive"`
	CreatedAt   time.Time `json:"createdAt"`
}

type DriverResource struct {
	DB *pgxpool.Pool
}

func (dr DriverResource) Routes() chi.Router {
	r := chi.NewRouter()

	// 🖥️ ADMIN ROUTES
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Post("/", dr.Create)
	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", dr.Get)
		r.Put("/", dr.Update)
		r.Delete("/", dr.Delete)
	})

	// 📱 MOBILE ROUTES (Protected by Supabase)
	r.Group(func(r chi.Router) {
		r.Use(middleware.SupabaseAuth)
		r.Post("/sync", dr.Sync)
		r.Get("/manifest", dr.GetMobileManifest)
		r.Post("/route/close", dr.CloseRoute)
	})

	return r
}

// Sync ensures the Go Database matches Supabase after an OTP login
func (dr *DriverResource) Sync(w http.ResponseWriter, r *http.Request) {
	// Grab UUID from Supabase JWT
	userID := r.Context().Value(middleware.UserIDKey).(string)

	// Grab Phone securely from JWT
	var phone string
	if p, ok := r.Context().Value(middleware.PhoneKey).(string); ok {
		phone = p
	}

	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = "Unknown Driver"
	}

	// Updated query to match your original schema exactly
	query := `
		INSERT INTO drivers (id, name, phone_number)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET phone_number = EXCLUDED.phone_number;
	`

	_, err := dr.DB.Exec(r.Context(), query, userID, req.Name, phone)

	// If Postgres rejects it, it will print in huge red letters in your terminal
	if err != nil {
		fmt.Printf("\n🚨 🚨 🚨 DB SYNC ERROR: %v\n\n", err)
		http.Error(w, `{"error": "Sync failed"}`, http.StatusInternalServerError)
		return
	}

	fmt.Println("✅ Driver successfully synced to Postgres! UUID:", userID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "synced"}`))
}

// GetMobileManifest securely looks up the route based on the JWT
func (dr *DriverResource) GetMobileManifest(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var routeID string
	err := dr.DB.QueryRow(r.Context(), `SELECT id FROM routes WHERE driver_id = $1 LIMIT 1`, driverID).Scan(&routeID)
	if err != nil {
		http.Error(w, `{"error": "No active route assigned to this driver"}`, http.StatusNotFound)
		return
	}

	targetDate := r.URL.Query().Get("date")
	if targetDate == "" {
		targetDate = time.Now().Format("2006-01-02")
	}

	manifest, err := route.GenerateManifest(dr.DB, r.Context(), routeID, targetDate)
	if err != nil {
		http.Error(w, "Error generating manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

func (dr DriverResource) Create(w http.ResponseWriter, r *http.Request) {
	var d Driver
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	u, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}
	d.Id = u

	query := `
		INSERT INTO drivers (id, name, phone_number)
		VALUES ($1, $2, $3)
	`

	_, err = dr.DB.Exec(r.Context(), query, d.Id, d.Name, d.PhoneNumber)
	if err != nil {
		http.Error(w, "Database execution failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      d.Id.String(),
		"message": "Driver registered successfully",
	})
}

func (dr DriverResource) List(w http.ResponseWriter, r *http.Request) {
	page, ok := r.Context().Value(middleware.PageKey).(int)
	if !ok {
		page = 1
	}
	limit, ok := r.Context().Value(middleware.LimitKey).(int)
	if !ok {
		limit = 10
	}
	offset := (page - 1) * limit

	query := `
		SELECT id, name, phone_number, is_active, created_at
		FROM drivers
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := dr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Failed to query drivers: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	drivers := []Driver{}
	for rows.Next() {
		var d Driver
		err := rows.Scan(&d.Id, &d.Name, &d.PhoneNumber, &d.IsActive, &d.CreatedAt)
		if err != nil {
			http.Error(w, "Row scan failure: "+err.Error(), http.StatusInternalServerError)
			return
		}
		drivers = append(drivers, d)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(drivers)
}

func (dr DriverResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `SELECT id, name, phone_number, is_active, created_at FROM drivers WHERE id = $1`

	var d Driver
	err := dr.DB.QueryRow(r.Context(), query, id).Scan(&d.Id, &d.Name, &d.PhoneNumber, &d.IsActive, &d.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Driver not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}

func (dr DriverResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var d Driver
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE drivers
		SET name = $1, phone_number = $2, is_active = $3
		WHERE id = $4
	`

	_, err := dr.DB.Exec(r.Context(), query, d.Name, d.PhoneNumber, d.IsActive, id)
	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver profile updated successfully"})
}

func (dr DriverResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Soft delete pattern to preserve financial/delivery history chains
	query := `UPDATE drivers SET is_active = FALSE WHERE id = $1`

	_, err := dr.DB.Exec(r.Context(), query, id)
	if err != nil {
		http.Error(w, "Deactivation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver profile deactivated"})
}

// CloseRoute allows the driver to manually end their shift.
// Any pending deliveries are instantly marked as UNATTEMPTED.
func (dr *DriverResource) CloseRoute(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var routeID string
	err := dr.DB.QueryRow(r.Context(), `SELECT id FROM routes WHERE driver_id = $1 LIMIT 1`, driverID).Scan(&routeID)
	if err != nil {
		http.Error(w, `{"error": "No active route"}`, http.StatusNotFound)
		return
	}

	targetDate := time.Now().Format("2006-01-02")

	query := `
		INSERT INTO delivery_logs (customer_id, route_id, delivery_date, status)
		SELECT
			c.id, c.route_id, $2::date, 'UNATTEMPTED'
		FROM customers c
		INNER JOIN subscriptions s ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON c.id = o.customer_id AND o.target_date = $2::date
		LEFT JOIN delivery_logs dl ON c.id = dl.customer_id AND dl.delivery_date = $2::date
		WHERE c.route_id = $1 AND c.is_active = TRUE AND c.stop_order > 0
		  AND dl.id IS NULL
		  AND (
			  s.schedule_type = 'daily'
			  OR (s.schedule_type = 'custom' AND EXTRACT(DOW FROM $2::date)::int = ANY(s.active_days))
			  OR (s.schedule_type = 'alternate' AND ($2::date - s.anchor_date) % 2 = 0)
			  OR o.id IS NOT NULL
		  )
	`

	_, err = dr.DB.Exec(r.Context(), query, routeID, targetDate)
	if err != nil {
		http.Error(w, "Failed to close route: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message": "Route closed successfully"}`))
}
