package delivery

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeliveryPayload represents what the driver app sends to the backend.
type DeliveryPayload struct {
	CustomerId  uuid.UUID       `json:"customerId"`
	RouteId     uuid.UUID       `json:"routeId"`
	Date        string          `json:"date"`   // YYYY-MM-DD format
	Status      string          `json:"status"` // "DELIVERED", "SKIPPED", "FAILED"
	ActualOrder *customer.Order `json:"actualOrder,omitempty"`

	// Anti-Fraud & Offline Sync Fields
	DriverLatitude  float64    `json:"driverLatitude"`
	DriverLongitude float64    `json:"driverLongitude"`
	CapturedAt      *time.Time `json:"capturedAt,omitempty"`
	ProofImageUrl   string     `json:"proofImageUrl,omitempty"`
}

// DeliveryLog represents the data sent back to the Admin dashboard for the List endpoint.
type DeliveryLog struct {
	Id             uuid.UUID      `json:"id"`
	CustomerId     uuid.UUID      `json:"customerId"`
	RouteId        *uuid.UUID     `json:"routeId"`
	Date           string         `json:"date"`
	Status         string         `json:"status"`
	IsFlagged      bool           `json:"isFlagged"`
	ProofImageUrl  string         `json:"proofImageUrl"` // Added this line!
	DeliveredOrder customer.Order `json:"deliveredOrder"`
}

// UpdateDeliveryPayload is used by the Admin to fix mistakes or un-flag a delivery.
type UpdateDeliveryPayload struct {
	Date        string         `json:"date"`
	Status      string         `json:"status"`
	IsFlagged   bool           `json:"isFlagged"`
	ActualOrder customer.Order `json:"actualOrder"`
}

type DeliveryResource struct {
	DB *pgxpool.Pool
}

func (dr DeliveryResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Mount the Paginated List and Create endpoints
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Post("/", dr.LogDelivery)

	// Mount the Admin Update endpoint
	r.Put("/{id}", dr.Update)

	return r
}

// haversine calculates the distance between two coordinates in meters.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000 // Earth radius in meters

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

	// 1. Fetch Customer Coordinates AND the Expected Order in one query
	var custLatStr, custLonStr string
	var defaultOrder customer.Order

	queryExpected := `
		SELECT
			c.geo_latitude, c.geo_longitude,
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
		WHERE c.id = $1
	`
	err := dr.DB.QueryRow(r.Context(), queryExpected, payload.CustomerId, payload.Date).Scan(
		&custLatStr, &custLonStr,
		&defaultOrder.Milk, &defaultOrder.Curd, &defaultOrder.Butter, &defaultOrder.Ghee,
		&defaultOrder.Lassi, &defaultOrder.Paneer, &defaultOrder.Jaggery, &defaultOrder.Khand,
		&defaultOrder.Oil, &defaultOrder.Atta, &defaultOrder.Burfi,
	)

	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Could not log delivery: Customer or Subscription not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "Database error fetching customer data: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. Determine final delivery quantities (Smart Defaulting)
	var finalOrder customer.Order
	if payload.Status != "DELIVERED" {
		finalOrder = customer.Order{} // Strict 0 for all items if skipped/failed
	} else if payload.ActualOrder != nil {
		finalOrder = *payload.ActualOrder // Driver typed in an override on the spot
	} else {
		finalOrder = defaultOrder // Driver just tapped "Delivered" with no changes
	}

	// 3. Distance Calculation (Proximity Check)
	custLat, _ := strconv.ParseFloat(custLatStr, 64)
	custLon, _ := strconv.ParseFloat(custLonStr, 64)

	var distanceMeters float64 = 0
	var isFlagged bool = false

	if custLat != 0 && custLon != 0 && payload.DriverLatitude != 0 {
		distanceMeters = haversine(payload.DriverLatitude, payload.DriverLongitude, custLat, custLon)
		if distanceMeters > 100 { // If driver is more than 100 meters away from the customer's coordinates, flag it!
			isFlagged = true
		}
	}

	// 4. Handle Offline Sync Timestamp
	actualCapturedAt := time.Now()
	if payload.CapturedAt != nil {
		actualCapturedAt = *payload.CapturedAt // Honor the time the driver was offline
	}

	// 5. Generate UUID & Upsert Log
	logId, err := uuid.NewV7()
	if err != nil {
		http.Error(w, "ID generation failed", http.StatusInternalServerError)
		return
	}

	queryUpsert := `
		INSERT INTO delivery_logs (
			id, customer_id, route_id, delivery_date, status,
			delivered_milk_qty, delivered_curd_qty, delivered_butter_qty, delivered_ghee_qty,
			delivered_lassi_qty, delivered_paneer_qty, delivered_jaggery_qty, delivered_khand_qty,
			delivered_oil_qty, delivered_atta_qty, delivered_burfi_qty,
			driver_latitude, driver_longitude, captured_at, distance_meters, proof_image_url, is_flagged
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
			$17, $18, $19, $20, $21, $22
		)
		ON CONFLICT (customer_id, delivery_date)
		DO UPDATE SET
			status = EXCLUDED.status, route_id = EXCLUDED.route_id,
			delivered_milk_qty = EXCLUDED.delivered_milk_qty, delivered_curd_qty = EXCLUDED.delivered_curd_qty,
			delivered_butter_qty = EXCLUDED.delivered_butter_qty, delivered_ghee_qty = EXCLUDED.delivered_ghee_qty,
			delivered_lassi_qty = EXCLUDED.delivered_lassi_qty, delivered_paneer_qty = EXCLUDED.delivered_paneer_qty,
			delivered_jaggery_qty = EXCLUDED.delivered_jaggery_qty, delivered_khand_qty = EXCLUDED.delivered_khand_qty,
			delivered_oil_qty = EXCLUDED.delivered_oil_qty, delivered_atta_qty = EXCLUDED.delivered_atta_qty,
			delivered_burfi_qty = EXCLUDED.delivered_burfi_qty,
			driver_latitude = EXCLUDED.driver_latitude, driver_longitude = EXCLUDED.driver_longitude,
			captured_at = EXCLUDED.captured_at, distance_meters = EXCLUDED.distance_meters,
			proof_image_url = EXCLUDED.proof_image_url, is_flagged = EXCLUDED.is_flagged,
			updated_at = NOW();
	`

	_, err = dr.DB.Exec(
		r.Context(), queryUpsert,
		logId, payload.CustomerId, payload.RouteId, payload.Date, payload.Status,
		finalOrder.Milk, finalOrder.Curd, finalOrder.Butter, finalOrder.Ghee,
		finalOrder.Lassi, finalOrder.Paneer, finalOrder.Jaggery, finalOrder.Khand,
		finalOrder.Oil, finalOrder.Atta, finalOrder.Burfi,
		payload.DriverLatitude, payload.DriverLongitude, actualCapturedAt, distanceMeters, payload.ProofImageUrl, isFlagged,
	)

	if err != nil {
		http.Error(w, "Failed to log delivery: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 6. Return Success Response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":        "Delivery logged successfully",
		"status":         payload.Status,
		"distanceMeters": distanceMeters,
		"isFlagged":      isFlagged,
		"loggedOrder":    finalOrder,
	})
}

// List fetches a paginated list of deliveries (useful for the admin dashboard)
func (dr DeliveryResource) List(w http.ResponseWriter, r *http.Request) {
	page := r.Context().Value(middleware.PageKey).(int)
	limit := r.Context().Value(middleware.LimitKey).(int)
	offset := (page - 1) * limit

	query := `
		SELECT
		id, customer_id, route_id, delivery_date, status, is_flagged, delivered_milk_qty, delivered_curd_qty,
		delivered_lassi_qty, delivered_paneer_qty, default_jaggery_qty, delivered_oil_qty, delivered_atta_qty,
		default_burfi_qty
		FROM delivery_logs
		ORDER BY delivery_date DESC, created_at DESC
		LIMIT $1 OFFSET $2
	`
	rows, err := dr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error"+err.Error(), http.StatusInternalServerError)
	}
	defer rows.Close()

	var deliveries []DeliveryLog

	for rows.Next() {
		var log DeliveryLog
		var deliveryDate time.Time
		var proofImgUrl *string

		err := rows.Scan(
			&log.Id, &log.CustomerId, &log.RouteId, &deliveryDate, &log.Status, &log.IsFlagged, &proofImgUrl,
			&log.DeliveredOrder.Milk, &log.DeliveredOrder.Curd, &log.DeliveredOrder.Butter, &log.DeliveredOrder.Ghee,
			&log.DeliveredOrder.Lassi, &log.DeliveredOrder.Paneer, &log.DeliveredOrder.Jaggery, &log.DeliveredOrder.Khand,
			&log.DeliveredOrder.Oil, &log.DeliveredOrder.Atta, &log.DeliveredOrder.Burfi,
		)
		if err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		log.Date = deliveryDate.Format("2006-01-02")
		if proofImgUrl != nil {
			log.ProofImageUrl = *proofImgUrl
		}

		deliveries = append(deliveries, log)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(deliveries)

}

// Update allows an Admin to manually override a delivery log, fix quantities, or un-flag it.
func (dr DeliveryResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var payload UpdateDeliveryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	query := `
		UPDATE delivery_logs
		SET
			delivery_date = $1,
			status = $2,
			is_flagged = $3,
			delivered_milk_qty = $4,
			delivered_curd_qty = $5,
			delivered_butter_qty = $6,
			delivered_ghee_qty = $7,
			delivered_lassi_qty = $8,
			delivered_paneer_qty = $9,
			delivered_jaggery_qty = $10,
			delivered_khand_qty = $11,
			delivered_oil_qty = $12,
			delivered_atta_qty = $13,
			delivered_burfi_qty = $14,
			updated_at = NOW()
		WHERE id = $15
	`

	_, err := dr.DB.Exec(
		r.Context(), query,
		payload.Date, payload.Status, payload.IsFlagged,
		payload.ActualOrder.Milk, payload.ActualOrder.Curd, payload.ActualOrder.Butter, payload.ActualOrder.Ghee,
		payload.ActualOrder.Lassi, payload.ActualOrder.Paneer, payload.ActualOrder.Jaggery, payload.ActualOrder.Khand,
		payload.ActualOrder.Oil, payload.ActualOrder.Atta, payload.ActualOrder.Burfi,
		id,
	)

	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Delivery log updated successfully"})
}
