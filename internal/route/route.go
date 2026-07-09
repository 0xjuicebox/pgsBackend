package route

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Route struct {
	Id          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	DriverId    *uuid.UUID `json:"driverId"` //Pointer because it can be null initially
	CreatedAt   time.Time  `json:"createdAt"`
}

type RouterResource struct {
	DB *pgxpool.Pool
}

func (rr RouterResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.With(middleware.Paginate).Get("/", rr.List)
	r.Post("/", rr.Create)

	return r
}

func (rr RouterResource) Create(w http.ResponseWriter, r *http.Request) {
	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Failed to create route:"+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"id":      rt.Id.String(),
		"message": "Delivery route created successfully",
	})
}

func (rr RouterResource) List(w http.ResponseWriter, r *http.Request) {
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
