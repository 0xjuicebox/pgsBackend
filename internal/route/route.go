package route

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

type Route struct {
	Id          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	DriverId    *uuid.UUID `json:"driverId"` //Pointer because it can be null initially
	CreatedAt   time.Time  `json:"createdAt"`
}

type RouteResource struct {
	DB *pgxpool.Pool
}

func (rr RouteResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.With(middleware.Paginate).Get("/", rr.List)
	r.Post("/", rr.Create)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", rr.Get)
		r.Put("/", rr.Update)
		r.Delete("/", rr.Delete)

		r.Put("/sequence", rr.UpdateSequence)
		r.Put("/driver", rr.UpdateDriver)
	})

	return r
}

func (rr RouteResource) Create(w http.ResponseWriter, r *http.Request) {
	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Failed to create route:"+err.Error(), http.StatusInternalServerError)
		return
	}

	u, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "Failed to generate ID", http.StatusInternalServerError)
		return
	}
	rt.Id = u

	query := `
	INSERT INTO routes (id, name, description)
	VALUES ($1, $2, $3)
	`

	_, err = rr.DB.Exec(r.Context(), query, rt.Id, rt.Name, rt.Description)
	if err != nil {
		http.Error(w, "Failed to create route in DB: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      rt.Id.String(),
		"message": "Delivery route created successfully",
	})
}

func (rr RouteResource) List(w http.ResponseWriter, r *http.Request) {
	page := r.Context().Value(middleware.PageKey).(int)
	limit := r.Context().Value(middleware.LimitKey).(int)

	offset := (page - 1) * limit

	query := `
		SELECT id, name, description, driver_id, created_at
		FROM routes
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := rr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Row scan error:"+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	routes := []Route{}

	for rows.Next() {
		var rt Route
		err := rows.Scan(&rt.Id, &rt.Name, &rt.Description, &rt.DriverId, &rt.CreatedAt)
		if err != nil {
			http.Error(w, "Row scan error:"+err.Error(), http.StatusInternalServerError)
			return
		}
		routes = append(routes, rt)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(routes)
}

func (rr RouteResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `
		SELECT id, name ,description, driver_id, created_at
		FROM routes
		WHERE id = $1
	`
	var rt Route

	err := rr.DB.QueryRow(r.Context(), query, id).Scan(
		&rt.Id, &rt.Name, &rt.Description, &rt.DriverId, &rt.CreatedAt,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error:"+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rt)
}

// Update allows you to only update the Name and Description of the Route
func (rr RouteResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `
		UPDATE routes
		SET name = $1, description = $2
		WHERE id = $3
	`

	_, err := rr.DB.Exec(r.Context(), query, rt.Name, rt.Description, id)
	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Route updated successfully"})
}

func (rr RouteResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Postgres automatically safely unassigns all customers attached to this ID!
	query := `DELETE FROM routes WHERE id = $1`

	_, err := rr.DB.Exec(r.Context(), query, id)
	if err != nil {
		http.Error(w, "Failed to delete route: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Route deleted and customers safely unassigned"})
}

type SequencePayload struct {
	CustomerIDs []string `json:"customerIds"`
}

func (rr RouteResource) UpdateSequence(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")

	var payload SequencePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Begin a safe database transaction block
	tx, err := rr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Failed to start transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Defer a rollback. If the function exits early without calling tx.Commit(),
	// changes are cleanly undone automatically.
	defer tx.Rollback(r.Context())

	// 2. Loop through the array and update the stop_order based on array index
	query := `
		UPDATE customers
		SET stop_order = $1
		WHERE id = $2 AND route_id = $3
	`

	for index, customerId := range payload.CustomerIDs {
		// Stop order starts at 1 for the driver's first stop
		stopOrder := index + 1

		_, err := tx.Exec(r.Context(), query, stopOrder, customerId, routeId)
		if err != nil {
			http.Error(w, "Failed updating sequence at customer "+customerId+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// 3. Commit the transaction to save all changes safely at once
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Failed to commit transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Route sequence updated successfully"})
}

type DriverPayload struct {
	DriverId *string `json:"driverId"` // Pointer allows passing null to completely unassign a driver
}

func (rr RouteResource) UpdateDriver(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")

	var payload DriverPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `UPDATE routes SET driver_id = $1 WHERE id = $2`

	_, err := rr.DB.Exec(r.Context(), query, payload.DriverId, routeId)
	if err != nil {
		// This will catch if you try to assign a driver that does not exist in the drivers table
		http.Error(w, "Assignment execution failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver link updated on route"})
}
