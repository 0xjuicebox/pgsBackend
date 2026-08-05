package route

import (
	"context"
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

		// Driver-facing: Who gets what TODAY
		r.Get("/manifest", rr.GetManifest)
		// Admin-facing: EVERYONE assigned to this route
		r.Get("/roster", rr.GetRoster)
	})

	return r
}

func (rr RouteResource) Create(w http.ResponseWriter, r *http.Request) {
	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Failed to decode payload: "+err.Error(), http.StatusBadRequest)
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

	tx, err := rr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Failed to start transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

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

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Failed to commit transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Route sequence updated successfully"})
}

type DriverPayload struct {
	DriverId *string `json:"driverId"`
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

// -------------------------------------------------------------------------
// GET /roster
// Used by Admin for Drag & Drop sequence editing. Ignores daily schedules.
// -------------------------------------------------------------------------

type RosterStop struct {
	Id           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	PhoneNumber  string    `json:"phoneNumber"`
	HouseAddress string    `json:"houseAddress"`
	StopOrder    int       `json:"stopOrder"`
	IsActive     bool      `json:"isActive"`
}

type RosterResponse struct {
	RouteID   uuid.UUID    `json:"routeId"`
	RouteName string       `json:"routeName"`
	Stops     []RosterStop `json:"stops"`
}

func (rr RouteResource) GetRoster(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")
	var response RosterResponse

	metaQuery := `SELECT id, name FROM routes WHERE id = $1`
	err := rr.DB.QueryRow(r.Context(), metaQuery, routeId).Scan(&response.RouteID, &response.RouteName)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 🚀 CRITICAL FIX: COALESCE(house_address, '') prevents pgx from crashing
	// if a customer record has a NULL house_address.
	customerQuery := `
		SELECT id, name, phone_number, COALESCE(house_address, ''), stop_order, is_active
		FROM customers
		WHERE route_id = $1 AND status IN ('active', 'disabled')
		ORDER BY stop_order ASC
	`
	rows, err := rr.DB.Query(r.Context(), customerQuery, routeId)
	if err != nil {
		http.Error(w, "Error fetching roster: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	response.Stops = []RosterStop{}
	for rows.Next() {
		var stop RosterStop
		if err := rows.Scan(&stop.Id, &stop.Name, &stop.PhoneNumber, &stop.HouseAddress, &stop.StopOrder, &stop.IsActive); err != nil {
			http.Error(w, "Error scanning roster: "+err.Error(), http.StatusInternalServerError)
			return
		}
		response.Stops = append(response.Stops, stop)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// -------------------------------------------------------------------------
// GET /manifest
// Used by Drivers. Filters by target date and non-zero orders.
// -------------------------------------------------------------------------

type ManifestStop struct {
	customer.Customer
	DeliveryOrder customer.Order `json:"deliveryOrder"`
	Status        *string        `json:"status"`
	ActualOrder   customer.Order `json:"actualOrder"`
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
	targetDate := r.URL.Query().Get("date")
	if targetDate == "" {
		targetDate = time.Now().Format("2006-01-02")
	}

	manifest, err := GenerateManifest(rr.DB, r.Context(), routeId, targetDate)
	if err != nil {
		http.Error(w, "Error generating manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

func GenerateManifest(db *pgxpool.Pool, ctx context.Context, routeId string, targetDate string) (ManifestResponse, error) {
	var manifest ManifestResponse
	manifest.TargetDate = targetDate

	metaQuery := `
		SELECT r.id, r.name, d.name, d.phone_number
		FROM routes r
		LEFT JOIN drivers d ON r.driver_id = d.id
		WHERE r.id = $1
	`
	err := db.QueryRow(ctx, metaQuery, routeId).Scan(
		&manifest.RouteID, &manifest.RouteName, &manifest.DriverName, &manifest.DriverPhone,
	)
	if err != nil {
		return manifest, err
	}

	customerQuery := `
		SELECT
			c.id, c.name, c.phone_number, c.house_address, c.geo_latitude, c.geo_longitude, c.is_active, c.stop_order, c.route_id,
			COALESCE(o.new_milk_qty, s.default_milk_qty, 0), COALESCE(o.new_curd_qty, s.default_curd_qty, 0),
			COALESCE(o.new_butter_qty, s.default_butter_qty, 0), COALESCE(o.new_ghee_qty, s.default_ghee_qty, 0),
			COALESCE(o.new_lassi_qty, s.default_lassi_qty, 0), COALESCE(o.new_paneer_qty, s.default_paneer_qty, 0),
			COALESCE(o.new_jaggery_qty, s.default_jaggery_qty, 0), COALESCE(o.new_khand_qty, s.default_khand_qty, 0),
			COALESCE(o.new_oil_qty, s.default_oil_qty, 0), COALESCE(o.new_atta_qty, s.default_atta_qty, 0),
			COALESCE(o.new_burfi_qty, s.default_burfi_qty, 0),
			dl.status,
			COALESCE(dl.delivered_milk_qty, 0), COALESCE(dl.delivered_curd_qty, 0),
			COALESCE(dl.delivered_butter_qty, 0), COALESCE(dl.delivered_ghee_qty, 0),
			COALESCE(dl.delivered_lassi_qty, 0), COALESCE(dl.delivered_paneer_qty, 0),
			COALESCE(dl.delivered_jaggery_qty, 0), COALESCE(dl.delivered_khand_qty, 0),
			COALESCE(dl.delivered_oil_qty, 0), COALESCE(dl.delivered_atta_qty, 0),
			COALESCE(dl.delivered_burfi_qty, 0)
		FROM customers c
		INNER JOIN subscriptions s ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON c.id = o.customer_id AND o.target_date = $2
		LEFT JOIN delivery_logs dl ON c.id = dl.customer_id AND dl.delivery_date = $2
		WHERE c.route_id = $1 AND c.is_active = TRUE AND c.stop_order > 0
			AND (
			s.schedule_type = 'daily'
			OR (s.schedule_type = 'custom' AND EXTRACT(DOW FROM $2::date)::int = ANY(s.active_days))
			OR (s.schedule_type = 'alternate' AND ($2::date - s.anchor_date) % 2 = 0)
			OR o.id IS NOT NULL
			)
		ORDER BY c.stop_order ASC
	`
	rows, err := db.Query(ctx, customerQuery, routeId, targetDate)
	if err != nil {
		return manifest, err
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
			&stop.Status,
			&stop.ActualOrder.Milk, &stop.ActualOrder.Curd, &stop.ActualOrder.Butter, &stop.ActualOrder.Ghee,
			&stop.ActualOrder.Lassi, &stop.ActualOrder.Paneer, &stop.ActualOrder.Jaggery, &stop.ActualOrder.Khand,
			&stop.ActualOrder.Oil, &stop.ActualOrder.Atta, &stop.ActualOrder.Burfi,
		)
		if err != nil {
			return manifest, err
		}
		if stop.DeliveryOrder.Milk == 0 && stop.DeliveryOrder.Curd == 0 && stop.DeliveryOrder.Butter == 0 && stop.DeliveryOrder.Ghee == 0 && stop.DeliveryOrder.Lassi == 0 && stop.DeliveryOrder.Paneer == 0 && stop.DeliveryOrder.Jaggery == 0 && stop.DeliveryOrder.Khand == 0 && stop.DeliveryOrder.Oil == 0 && stop.DeliveryOrder.Atta == 0 && stop.DeliveryOrder.Burfi == 0 {
			continue
		}
		manifest.Stops = append(manifest.Stops, stop)
	}
	return manifest, nil
}
