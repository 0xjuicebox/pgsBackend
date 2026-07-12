package route

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
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
	DriverId    *uuid.UUID `json:"driverId"` // Pointer because it can be null initially
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
		r.Get("/manifest", rr.GetManifest)
	})

	return r
}

func (rr RouteResource) Create(w http.ResponseWriter, r *http.Request) {
	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Failed to decode payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Generate the Server-Side UUID
	u, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "Failed to generate ID", http.StatusInternalServerError)
		return
	}
	rt.Id = u

	// 2. Insert into the database (driver_id defaults to null)
	query := `
		INSERT INTO routes (id, name, description)
		VALUES ($1, $2, $3)
	`

	_, err = rr.DB.Exec(r.Context(), query, rt.Id, rt.Name, rt.Description)
	if err != nil {
		http.Error(w, "Failed to create route in DB: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. Return the success response with the real ID
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
	defer tx.Rollback(r.Context())

	// 2. Loop through the array and update the stop_order based on array index.
	// Only targets customers who are ALREADY assigned to this route_id.
	query := `
		UPDATE customers
		SET stop_order = $1
		WHERE id = $2 AND route_id = $3
	`

	for index, customerId := range payload.CustomerIDs {
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
		http.Error(w, "Assignment execution failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver link updated on route"})
}

// We define a specialized ManifestStop that encapsulates a customer's identity
// along with their dynamically calculated daily order.
type ManifestStop struct {
	customer.Customer
	DeliveryOrder customer.Order `json:"deliveryOrder"`
}

type ManifestResponse struct {
	RouteID     uuid.UUID      `json:"routeId"`
	RouteName   string         `json:"routeName"`
	DriverName  *string        `json:"driverName"`
	DriverPhone *string        `json:"driverPhone"`
	TargetDate  string         `json:"targetDate"`
	Stops       []ManifestStop `json:"stops"`
}

func (rr RouteResource) GetManifest(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")

	// Determine the Target Date (YYYY-MM-DD), defaulting to today
	targetDate := r.URL.Query().Get("date")
	if targetDate == "" {
		targetDate = time.Now().Format("2006-01-02")
	}

	// 1. Fetch Route Metadata
	metaQuery := `
		SELECT r.id, r.name, d.name, d.phone_number
		FROM routes r
		LEFT JOIN drivers d ON r.driver_id = d.id
		WHERE r.id = $1
	`

	var manifest ManifestResponse
	manifest.TargetDate = targetDate

	err := rr.DB.QueryRow(r.Context(), metaQuery, routeId).Scan(
		&manifest.RouteID,
		&manifest.RouteName,
		&manifest.DriverName,
		&manifest.DriverPhone,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error fetching manifest metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. Query customers matching the target date's delivery schedule, or customers with active overrides.
	// COALESCE prioritize the override quantities first, falling back to subscription defaults.
	customerQuery := `
		SELECT
			c.id, c.name, c.phone_number, c.house_address, c.geo_latitude, c.geo_longitude, c.is_active, c.stop_order, c.route_id,
			COALESCE(o.new_milk_qty, s.default_milk_qty, 0),
			COALESCE(o.new_curd_qty, s.default_curd_qty, 0),
			COALESCE(o.new_butter_qty, s.default_butter_qty, 0),
			COALESCE(o.new_ghee_qty, s.default_ghee_qty, 0),
			COALESCE(o.new_lassi_qty, s.default_lassi_qty, 0),
			COALESCE(o.new_paneer_qty, s.default_paneer_qty, 0),
			COALESCE(o.new_jaggery_qty, s.default_jaggery_qty, 0),
			COALESCE(o.new_khand_qty, s.default_khand_qty, 0),
			COALESCE(o.new_oil_qty, s.default_oil_qty, 0),
			COALESCE(o.new_atta_qty, s.default_atta_qty, 0),
			COALESCE(o.new_burfi_qty, s.default_burfi_qty, 0)
		FROM customers c
		INNER JOIN subscriptions s ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON c.id = o.customer_id AND o.target_date = $2
		WHERE c.route_id = $1 AND c.is_active = TRUE AND c.stop_order > 0
		  AND (
			s.schedule_type = 'daily'
			OR (s.schedule_type = 'custom' AND EXTRACT(DOW FROM $2::date)::int = ANY(s.active_days))
			OR (s.schedule_type = 'alternate' AND ($2::date - s.anchor_date) % 2 = 0)
			OR o.id IS NOT NULL
		  )
		ORDER BY c.stop_order ASC
	`

	rows, err := rr.DB.Query(r.Context(), customerQuery, routeId, targetDate)
	if err != nil {
		http.Error(w, "Database error fetching manifest stops: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	manifest.Stops = []ManifestStop{}

	for rows.Next() {
		var stop ManifestStop
		err := rows.Scan(
			&stop.Id, &stop.Name, &stop.PhoneNumber, &stop.HouseAddress, &stop.GeoLatitude, &stop.GeoLongitude, &stop.IsActive, &stop.StopOrder, &stop.RouteId,
			&stop.DeliveryOrder.Milk, &stop.DeliveryOrder.Curd, &stop.DeliveryOrder.Butter, &stop.DeliveryOrder.Ghee,
			&stop.DeliveryOrder.Lassi, &stop.DeliveryOrder.Paneer, &stop.DeliveryOrder.Jaggery, &stop.DeliveryOrder.Khand,
			&stop.DeliveryOrder.Oil, &stop.DeliveryOrder.Atta, &stop.DeliveryOrder.Burfi,
		)
		if err != nil {
			http.Error(w, "Row scan error compiling manifest: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Skip this stop if every single product quantity is 0 (e.g., Vacation Pause)
		if stop.DeliveryOrder.Milk == 0 && stop.DeliveryOrder.Curd == 0 &&
			stop.DeliveryOrder.Butter == 0 && stop.DeliveryOrder.Ghee == 0 &&
			stop.DeliveryOrder.Lassi == 0 && stop.DeliveryOrder.Paneer == 0 &&
			stop.DeliveryOrder.Jaggery == 0 && stop.DeliveryOrder.Khand == 0 &&
			stop.DeliveryOrder.Oil == 0 && stop.DeliveryOrder.Atta == 0 &&
			stop.DeliveryOrder.Burfi == 0 {
			continue
		}

		manifest.Stops = append(manifest.Stops, stop)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}
