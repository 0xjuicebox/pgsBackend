package customer

import (
	"encoding/json"
	"net/http"

	"github.com/0xjuicebox/goService/middleware" // Make sure this matches your mod name
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Item int

const (
	Milk Item = iota
	Ghee
	Paneer
)

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
	Id              uuid.UUID `json:"uuid"`
	Name            string    `json:"customer"`
	PhoneNumber     string    `json:"phoneNumber"`
	HouseAddress    string    `json:"houseAddress"`
	GeoLatitude     string    `json:"geoLatitude"`
	GeoLongitude    string    `json:"geoLonitude"`
	DefaultQuantity Order     `json:"defaultOrder"`
	IsActive        bool      `json:"isActive"` // Changed to bool to match standard DB boolean types
}

type CustomerResource struct {
	DB *pgxpool.Pool
}

func (cr CustomerResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/", cr.List)
	r.Post("/", cr.Create)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", cr.Get)
		r.Put("/", cr.Update)
		r.Delete("/", cr.Delete)
	})
	return r
}

func (cr CustomerResource) List(w http.ResponseWriter, r *http.Request) {
	page := r.Context().Value(middleware.PageKey).(int)
	limit := r.Context().Value(middleware.LimitKey).(int)
	offset := (page - 1) * limit

	// Order matches the scan block below precisely
	query := `
		SELECT
			id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty
		FROM customers
		ORDER BY id DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := cr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Failed to fetch customers: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	customers := []Customer{}

	for rows.Next() {
		var c Customer
		err := rows.Scan(
			&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive,
			&c.DefaultQuantity.Milk, &c.DefaultQuantity.Curd, &c.DefaultQuantity.Butter, &c.DefaultQuantity.Ghee,
			&c.DefaultQuantity.Lassi, &c.DefaultQuantity.Paneer, &c.DefaultQuantity.Jaggery, &c.DefaultQuantity.Khand,
			&c.DefaultQuantity.Oil, &c.DefaultQuantity.Atta, &c.DefaultQuantity.Burfi,
		)
		if err != nil {
			http.Error(w, "Error scanning database row: "+err.Error(), http.StatusInternalServerError)
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
		SELECT
			id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active,
			default_milk_qty, default_curd_qty, default_butter_qty, default_ghee_qty,
			default_lassi_qty, default_paneer_qty, default_jaggery_qty, default_khand_qty,
			default_oil_qty, default_atta_qty, default_burfi_qty
		FROM customers
		WHERE id = $1
	`

	var c Customer
	err := cr.DB.QueryRow(r.Context(), query, id).Scan(
		&c.Id, &c.Name, &c.PhoneNumber, &c.HouseAddress, &c.GeoLatitude, &c.GeoLongitude, &c.IsActive,
		&c.DefaultQuantity.Milk, &c.DefaultQuantity.Curd, &c.DefaultQuantity.Butter, &c.DefaultQuantity.Ghee,
		&c.DefaultQuantity.Lassi, &c.DefaultQuantity.Paneer, &c.DefaultQuantity.Jaggery, &c.DefaultQuantity.Khand,
		&c.DefaultQuantity.Oil, &c.DefaultQuantity.Atta, &c.DefaultQuantity.Burfi,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
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
		SET
			name = $1, phone_number = $2, house_address = $3, geo_latitude = $4, geo_longitude = $5, is_active = $6,
			default_milk_qty = $7, default_curd_qty = $8, default_butter_qty = $9, default_ghee_qty = $10,
			default_lassi_qty = $11, default_paneer_qty = $12, default_jaggery_qty = $13, default_khand_qty = $14,
			default_oil_qty = $15, default_atta_qty = $16, default_burfi_qty = $17
		WHERE id = $18
	`

	_, err := cr.DB.Exec(
		r.Context(), query,
		c.Name, c.PhoneNumber, c.HouseAddress, c.GeoLatitude, c.GeoLongitude, c.IsActive,
		c.DefaultQuantity.Milk, c.DefaultQuantity.Curd, c.DefaultQuantity.Butter, c.DefaultQuantity.Ghee,
		c.DefaultQuantity.Lassi, c.DefaultQuantity.Paneer, c.DefaultQuantity.Jaggery, c.DefaultQuantity.Khand,
		c.DefaultQuantity.Oil, c.DefaultQuantity.Atta, c.DefaultQuantity.Burfi,
		id,
	)

	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer updated successfully"})
}

func (cr CustomerResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `UPDATE customers SET is_active = FALSE WHERE id = $1`

	_, err := cr.DB.Exec(r.Context(), query, id)
	if err != nil {
		http.Error(w, "Failed to delete customer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Customer deactivated successfully"})
}
