package customer

import (
	"encoding/json"
	"net/http"

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
	Id           uuid.UUID  `json:"id"`
	Name         string     `json:"customer"`
	PhoneNumber  string     `json:"phoneNumber"`
	HouseAddress string     `json:"houseAddress"`
	GeoLatitude  string     `json:"geoLatitude"`
	GeoLongitude string     `json:"geoLongitude"`
	IsActive     bool       `json:"isActive"`
	StopOrder    int        `json:"stopOrder"`
	RouteId      *uuid.UUID `json:"routeId"`
	// Notice: defaultOrder is completely GONE. Identity only!
}

type CustomerResource struct {
	DB *pgxpool.Pool
}

func (cr CustomerResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.With(middleware.Paginate).Get("/", cr.List)
	r.Post("/", cr.Create)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", cr.Get)
		r.Put("/", cr.Update)
		r.Delete("/", cr.Delete)
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

	// Stripped down SQL: Only inserting Identity data
	query := `
		INSERT INTO customers (
			id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7
		)
	`

	_, err = cr.DB.Exec(
		r.Context(), query,
		c.Id, c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive,
	)

	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      c.Id.String(),
		"message": "Customer identity created successfully",
	})
}

func (cr CustomerResource) List(w http.ResponseWriter, r *http.Request) {
	page := r.Context().Value(middleware.PageKey).(int)
	limit := r.Context().Value(middleware.LimitKey).(int)
	offset := (page - 1) * limit

	query := `
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, stop_order, route_id
		FROM customers
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := cr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	customers := []Customer{}
	for rows.Next() {
		var c Customer
		err := rows.Scan(
			&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.StopOrder, &c.RouteId,
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
		SELECT id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, stop_order, route_id
		FROM customers
		WHERE id = $1
	`
	var c Customer
	err := cr.DB.QueryRow(r.Context(), query, id).Scan(
		&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive, &c.StopOrder, &c.RouteId,
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
		SET name = $1, phone_number = $2, house_address = $3, geo_latitude = $4, geo_longitude = $5, is_active = $6, stop_order = $7, route_id = $8
		WHERE id = $9
	`

	_, err := cr.DB.Exec(
		r.Context(), query,
		c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive, c.StopOrder, c.RouteId, id,
	)

	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer identity updated successfully"})
}

func (cr CustomerResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `DELETE FROM customers WHERE id = $1`
	_, err := cr.DB.Exec(r.Context(), query, id)

	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer deleted successfully"})
}
