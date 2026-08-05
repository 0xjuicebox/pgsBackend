package delivery

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DeliveryPayload struct {
	CustomerId      uuid.UUID       `json:"customerId"`
	RouteId         uuid.UUID       `json:"routeId"`
	Date            string          `json:"date"`
	Status          string          `json:"status"`
	ActualOrder     *customer.Order `json:"actualOrder,omitempty"`
	DriverLatitude  float64         `json:"driverLatitude"`
	DriverLongitude float64         `json:"driverLongitude"`
	CapturedAt      *time.Time      `json:"capturedAt,omitempty"`
	ProofImageUrl   string          `json:"proofImageUrl,omitempty"`
}

type DeliveryLog struct {
	Id             uuid.UUID      `json:"id"`
	CustomerId     uuid.UUID      `json:"customerId"`
	RouteId        *uuid.UUID     `json:"routeId"`
	Date           string         `json:"date"`
	Status         string         `json:"status"`
	IsFlagged      bool           `json:"isFlagged"`
	ProofImageUrl  string         `json:"proofImageUrl"`
	DeliveredOrder customer.Order `json:"deliveredOrder"`
}

type UpdateDeliveryPayload struct {
	Date        string         `json:"date"`
	Status      string         `json:"status"`
	IsFlagged   bool           `json:"isFlagged"`
	ActualOrder customer.Order `json:"actualOrder"`
}

// --- HISTORY STRUCTS FOR MOBILE APP ---
type PastItemDetail struct {
	Name     string `json:"name"`
	Expected int    `json:"expected"`
	Actual   int    `json:"actual"`
	Unit     string `json:"unit"`
}

type PastStop struct {
	Id           string           `json:"id"`
	Customer     string           `json:"customer"`
	Address      string           `json:"address"`
	Status       string           `json:"status"`
	DeliveryTime string           `json:"deliveryTime"`
	Items        []PastItemDetail `json:"items"`
}

type HistoryRecord struct {
	Id             string     `json:"id"`
	Date           string     `json:"date"`
	Timestamp      string     `json:"timestamp"`
	RouteName      string     `json:"routeName"`
	TotalStops     int        `json:"totalStops"`
	CompletedStops int        `json:"completedStops"`
	MilkTotal      int        `json:"milkTotal"`
	Stops          []PastStop `json:"stops"`
}

// FlaggedDelivery is what the admin dashboard needs to triage a customer
// complaint: enough customer context to act on it without a second lookup.
type FlaggedDelivery struct {
	Id               uuid.UUID `json:"id"`
	CustomerId       uuid.UUID `json:"customerId"`
	CustomerName     string    `json:"customerName"`
	PhoneNumber      string    `json:"phoneNumber"`
	HouseAddress     string    `json:"houseAddress"`
	DeliveryDate     time.Time `json:"deliveryDate"`
	Status           string    `json:"status"`
	CustomerFeedback string    `json:"customerFeedback"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type DeliveryResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (dr DeliveryResource) Routes() chi.Router {
	r := chi.NewRouter()

	// 🖥️ ADMIN ROUTES
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Put("/{id}", dr.Update)
	r.With(middleware.Paginate).Get("/flagged", dr.ListFlagged)
	r.Post("/{id}/resolve", dr.Resolve)

	// 📱 MOBILE ROUTES (Protected by Supabase)
	r.Group(func(r chi.Router) {
		r.Use(middleware.SupabaseAuth)
		r.Post("/", dr.LogDelivery)
		r.Get("/history", dr.DriverHistory) // 🚀 NEW ENDPOINT
	})

	return r
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	deltaPhi := (lat2 - lat1) * math.Pi / 180
	deltaLambda := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*
			math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}

func (dr DeliveryResource) LogDelivery(w http.ResponseWriter, r *http.Request) {
	var payload DeliveryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 🚀 Extract Driver ID to stamp onto the log permanently
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var custLatStr, custLonStr, custPhone string
	var defaultOrder customer.Order

	queryExpected := `
		SELECT
			c.geo_latitude, c.geo_longitude, c.phone_number,
			COALESCE(o.new_milk_qty, s.default_milk_qty, 0),
			COALESCE(o.new_curd_qty, s.default_curd_qty, 0),
			COALESCE(o.new_butter_qty, s.default_butter_qty, 0)
		FROM customers c
		INNER JOIN subscriptions s ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON c.id = o.customer_id AND o.target_date = $2
		WHERE c.id = $1
	`
	err := dr.DB.QueryRow(r.Context(), queryExpected, payload.CustomerId, payload.Date).Scan(
		&custLatStr, &custLonStr, &custPhone,
		&defaultOrder.Milk, &defaultOrder.Curd, &defaultOrder.Butter,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Could not log delivery: Customer or Subscription not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var finalOrder customer.Order
	if payload.Status != "DELIVERED" {
		finalOrder = customer.Order{}
	} else if payload.ActualOrder != nil {
		finalOrder = *payload.ActualOrder
	} else {
		finalOrder = defaultOrder
	}

	custLat, _ := strconv.ParseFloat(custLatStr, 64)
	custLon, _ := strconv.ParseFloat(custLonStr, 64)
	var distanceMeters float64 = 0
	var isFlagged bool = false

	if custLat != 0 && custLon != 0 && payload.DriverLatitude != 0 {
		distanceMeters = haversine(payload.DriverLatitude, payload.DriverLongitude, custLat, custLon)
		if distanceMeters > 100 {
			isFlagged = true
		}
	}

	actualCapturedAt := time.Now()
	if payload.CapturedAt != nil {
		actualCapturedAt = *payload.CapturedAt
	}

	logId, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}

	queryUpsert := `
		INSERT INTO delivery_logs (
			id, customer_id, route_id, driver_id, delivery_date, status,
			delivered_milk_qty, delivered_curd_qty, delivered_butter_qty,
			driver_latitude, driver_longitude, captured_at, distance_meters, proof_image_url, is_flagged
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)
		ON CONFLICT (customer_id, delivery_date)
		DO UPDATE SET
			status = EXCLUDED.status, route_id = EXCLUDED.route_id, driver_id = EXCLUDED.driver_id,
			delivered_milk_qty = EXCLUDED.delivered_milk_qty, delivered_curd_qty = EXCLUDED.delivered_curd_qty,
			delivered_butter_qty = EXCLUDED.delivered_butter_qty,
			driver_latitude = EXCLUDED.driver_latitude, driver_longitude = EXCLUDED.driver_longitude,
			captured_at = EXCLUDED.captured_at, distance_meters = EXCLUDED.distance_meters,
			proof_image_url = EXCLUDED.proof_image_url, is_flagged = EXCLUDED.is_flagged,
			updated_at = NOW();
	`

	_, err = dr.DB.Exec(
		r.Context(), queryUpsert,
		logId, payload.CustomerId, payload.RouteId, driverID, payload.Date, payload.Status,
		finalOrder.Milk, finalOrder.Curd, finalOrder.Butter,
		payload.DriverLatitude, payload.DriverLongitude, actualCapturedAt, distanceMeters, payload.ProofImageUrl, isFlagged,
	)

	if err != nil {
		http.Error(w, "Failed to log delivery: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"message": "Delivery logged successfully", "status": payload.Status})
}

// DriverHistory constructs the exact JSON schema needed by the React Native History Screen
func (dr DeliveryResource) DriverHistory(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	query := `
		SELECT
			dl.id, dl.delivery_date, dl.status, dl.captured_at,
			r.name as route_name, c.name as customer_name, c.house_address,
			dl.delivered_milk_qty, dl.delivered_curd_qty, dl.delivered_butter_qty
		FROM delivery_logs dl
		JOIN routes r ON dl.route_id = r.id
		JOIN customers c ON dl.customer_id = c.id
		WHERE dl.driver_id = $1
		ORDER BY dl.delivery_date DESC, dl.captured_at ASC
	`
	rows, err := dr.DB.Query(r.Context(), query, driverID)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// Use a map to group stops by Date+Route
	historyMap := make(map[string]*HistoryRecord)
	var orderedKeys []string

	for rows.Next() {
		var logId, status, routeName, custName, address string
		var rawDate, capturedAt time.Time
		var milk, curd, butter int

		err := rows.Scan(&logId, &rawDate, &status, &capturedAt, &routeName, &custName, &address, &milk, &curd, &butter)
		if err != nil {
			continue
		}

		dateKey := rawDate.Format("2006-01-02")
		groupKey := dateKey + "|" + routeName

		if _, exists := historyMap[groupKey]; !exists {
			orderedKeys = append(orderedKeys, groupKey)
			historyMap[groupKey] = &HistoryRecord{
				Id:             groupKey,
				Date:           rawDate.Format("Jan 02, 2006"),
				Timestamp:      dateKey,
				RouteName:      routeName,
				TotalStops:     0,
				CompletedStops: 0,
				MilkTotal:      0,
				Stops:          []PastStop{},
			}
		}

		record := historyMap[groupKey]
		record.TotalStops++

		if status == "DELIVERED" {
			record.CompletedStops++
			record.MilkTotal += milk
		}

		// Build Item Details
		var items []PastItemDetail
		if milk > 0 {
			items = append(items, PastItemDetail{Name: "Milk", Expected: milk, Actual: milk, Unit: "L"})
		}
		if curd > 0 {
			items = append(items, PastItemDetail{Name: "Curd", Expected: curd, Actual: curd, Unit: "kg"})
		}
		if butter > 0 {
			items = append(items, PastItemDetail{Name: "Butter", Expected: butter, Actual: butter, Unit: "pkt"})
		}

		record.Stops = append(record.Stops, PastStop{
			Id:           logId,
			Customer:     custName,
			Address:      address,
			Status:       status,
			DeliveryTime: capturedAt.Format("03:04 PM"),
			Items:        items,
		})
	}

	// Flatten map to array
	var response []HistoryRecord
	for _, key := range orderedKeys {
		response = append(response, *historyMap[key])
	}

	w.Header().Set("Content-Type", "application/json")
	if len(response) == 0 {
		w.Write([]byte(`[]`))
		return
	}
	json.NewEncoder(w).Encode(response)
}

// ListFlagged returns delivery_logs rows currently flagged by a customer
// complaint (via the WhatsApp issue-report flow), newest first, joined with
// the customer's identity/contact info so the dashboard needs no second call.
func (dr DeliveryResource) ListFlagged(w http.ResponseWriter, r *http.Request) {
	page, ok := r.Context().Value(middleware.PageKey).(int)
	if !ok {
		page = 1
	}
	limit, ok := r.Context().Value(middleware.LimitKey).(int)
	if !ok {
		limit = 20
	}
	offset := (page - 1) * limit

	query := `
		SELECT dl.id, dl.customer_id, c.name, c.phone_number, c.house_address,
		       dl.delivery_date, dl.status, dl.customer_feedback, dl.updated_at
		FROM delivery_logs dl
		JOIN customers c ON c.id = dl.customer_id
		WHERE dl.is_flagged = true
		ORDER BY dl.updated_at DESC
		LIMIT $1 OFFSET $2
	`

	rows, err := dr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	flagged := []FlaggedDelivery{}
	for rows.Next() {
		var f FlaggedDelivery
		if err := rows.Scan(
			&f.Id, &f.CustomerId, &f.CustomerName, &f.PhoneNumber, &f.HouseAddress,
			&f.DeliveryDate, &f.Status, &f.CustomerFeedback, &f.UpdatedAt,
		); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		flagged = append(flagged, f)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(flagged)
}

// Resolve clears the flag once admin has dealt with a complaint. Kept
// deliberately simple for now — just unflags the row. If you later want a
// "resolved by / resolved at / resolution notes" audit trail, this is the
// natural place to extend.
func (dr DeliveryResource) Resolve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `
		UPDATE delivery_logs
		SET is_flagged = false, updated_at = NOW()
		WHERE id = $1 AND is_flagged = true
		RETURNING id
	`

	var resolvedID uuid.UUID
	err := dr.DB.QueryRow(r.Context(), query, id).Scan(&resolvedID)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Flagged delivery not found or already resolved", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Marked as resolved"})
}

// (List and Update handlers for Admin remain unchanged, omitted for brevity)
func (dr DeliveryResource) List(w http.ResponseWriter, r *http.Request) {
	// ... your existing code
}

func (dr DeliveryResource) Update(w http.ResponseWriter, r *http.Request) {
	// ... your existing code
}
