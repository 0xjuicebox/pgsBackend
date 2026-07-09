package driver

import (
	"encoding/json"
	"net/http"
	"time"

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

	r.With(middleware.Paginate).Get("/", dr.List)
	r.Post("/", dr.Create)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", dr.Get)
		r.Put("/", dr.Update)
		r.Delete("/", dr.Delete)
	})

	return r
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
