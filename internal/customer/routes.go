package customer

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
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
	IsActive        string    `json:"isActive"`
}

type customerResource struct {
	DB *pgxpool.Pool
}

func (cr customerResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/", cr.List)
	r.Post("/", cr.Create)
	//r.Delete("/", cr.Delete)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", cr.Get)
		r.Put("/", cr.Update)
		r.Delete("/", cr.Delete)
	})
	return r
}

func (cr customerResource) List(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Listing customers..."))
}
func (cr customerResource) Create(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Creating customer"))
}
func (cr customerResource) Delete(w http.ResponseWriter, r *http.Request) {
	customerId := r.PathValue("id")
	w.Write([]byte(fmt.Sprintf("Deleteing customer %s", customerId)))
}

func (cr customerResource) Get(w http.ResponseWriter, r *http.Request) {
	customerId := r.PathValue("id")
	w.Write([]byte(fmt.Sprintf("Getting customer %s", customerId)))
}

func (cr customerResource) Update(w http.ResponseWriter, r *http.Request) {
	customerId := r.PathValue("id")
	w.Write([]byte(fmt.Sprintf("Updatingg customer %s", customerId)))
}
