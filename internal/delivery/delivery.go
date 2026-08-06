package delivery

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var Products = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

// -------------------------------------------------------------------------
// Types
// -------------------------------------------------------------------------

type DeliveryPayload struct {
	CustomerId      uuid.UUID       `json:"customerId"`
	RouteId         uuid.UUID       `json:"routeId"`
	Slot            string          `json:"slot"`
	Date            string          `json:"date"`
	Status          string          `json:"status"`
	ActualOrder     *customer.Order `json:"actualOrder,omitempty"`
	DriverLatitude  float64         `json:"driverLatitude"`
	DriverLongitude float64         `json:"driverLongitude"`
	CapturedAt      *time.Time      `json:"capturedAt,omitempty"`
	ProofImageUrl   string          `json:"proofImageUrl,omitempty"`
}

type FlaggedDelivery struct {
	Id               uuid.UUID `json:"id"`
	CustomerId       uuid.UUID `json:"customerId"`
	CustomerName     string    `json:"customerName"`
	PhoneNumber      string    `json:"phoneNumber"`
	HouseAddress     string    `json:"houseAddress"`
	Slot             string    `json:"slot"`
	DeliveryDate     time.Time `json:"deliveryDate"`
	Status           string    `json:"status"`
	CustomerFeedback string    `json:"customerFeedback"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// AdminDeliveryLog is the enriched row the admin UI renders.
type AdminDeliveryLog struct {
	Id           uuid.UUID      `json:"id"`
	CustomerId   uuid.UUID      `json:"customerId"`
	CustomerName string         `json:"customerName"`
	PhoneNumber  string         `json:"phoneNumber"`
	HouseAddress string         `json:"houseAddress"`
	StopOrder    int            `json:"stopOrder"`
	RouteId      *uuid.UUID     `json:"routeId"`
	RouteName    *string        `json:"routeName"`
	DriverName   *string        `json:"driverName"`
	Slot         string         `json:"slot"`
	DeliveryDate string         `json:"deliveryDate"`
	Status       string         `json:"status"`
	Quantities   map[string]int `json:"quantities"`
	IsFlagged    bool           `json:"isFlagged"`
	Feedback     *string        `json:"customerFeedback"`
	CapturedAt   *time.Time     `json:"capturedAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

type UpdateLogRequest struct {
	Status     *string         `json:"status"`
	Quantities *map[string]int `json:"quantities"`
	IsFlagged  *bool           `json:"isFlagged"`
	Feedback   *string         `json:"customerFeedback"`
}

// Mobile history structs
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

type DeliveryResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (dr DeliveryResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Admin routes
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Put("/{id}", dr.Update)
	r.Post("/admin", dr.AdminUpsert)
	r.With(middleware.Paginate).Get("/flagged", dr.ListFlagged)
	r.Post("/{id}/resolve", dr.Resolve)

	// Mobile routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.SupabaseAuth)
		r.Post("/", dr.LogDelivery)
		r.Get("/history", dr.DriverHistory)
	})

	return r
}

// -------------------------------------------------------------------------
// Mobile: LogDelivery — the driver's write path
// -------------------------------------------------------------------------

func (dr DeliveryResource) LogDelivery(w http.ResponseWriter, r *http.Request) {
	var payload DeliveryPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	driverID := r.Context().Value(middleware.UserIDKey).(string)

	if payload.Slot == "" {
		payload.Slot = "morning"
	}

	// Look up the expected order (override > subscription default) for all 11
	// products, plus the customer's geo for flagging, plus the route's current
	// prices for the snapshot.
	var custLatStr, custLonStr string
	var defaultOrder customer.Order
	prices := make([]float64, len(Products))

	queryExpected := `
		SELECT
			COALESCE(c.geo_latitude, ''), COALESCE(c.geo_longitude, ''),
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
			COALESCE(o.new_burfi_qty, s.default_burfi_qty, 0),
			rt.price_milk, rt.price_curd, rt.price_butter, rt.price_ghee,
			rt.price_lassi, rt.price_paneer, rt.price_jaggery, rt.price_khand,
			rt.price_oil, rt.price_atta, rt.price_burfi
		FROM customers c
		INNER JOIN subscriptions s ON c.id = s.customer_id AND s.slot = $3
		LEFT JOIN order_overrides o ON o.customer_id = c.id AND o.target_date = $2 AND o.slot = $3
		LEFT JOIN routes rt ON rt.id = s.route_id
		WHERE c.id = $1
	`
	err := dr.DB.QueryRow(r.Context(), queryExpected, payload.CustomerId, payload.Date, payload.Slot).Scan(
		&custLatStr, &custLonStr,
		&defaultOrder.Milk, &defaultOrder.Curd, &defaultOrder.Butter, &defaultOrder.Ghee,
		&defaultOrder.Lassi, &defaultOrder.Paneer, &defaultOrder.Jaggery, &defaultOrder.Khand,
		&defaultOrder.Oil, &defaultOrder.Atta, &defaultOrder.Burfi,
		&prices[0], &prices[1], &prices[2], &prices[3], &prices[4], &prices[5],
		&prices[6], &prices[7], &prices[8], &prices[9], &prices[10],
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Customer or subscription not found for this slot", http.StatusBadRequest)
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

	// Geo-flagging
	custLat, _ := strconv.ParseFloat(custLatStr, 64)
	custLon, _ := strconv.ParseFloat(custLonStr, 64)
	var distanceMeters float64
	var isFlagged bool
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

	logId, _ := uuid.NewV7()

	// The upsert now writes all 11 product quantities PLUS the 11 unit_price
	// snapshots from the route's current price list.
	queryUpsert := `
		INSERT INTO delivery_logs (
			id, customer_id, route_id, driver_id, delivery_date, status, slot,
			delivered_milk_qty, delivered_curd_qty, delivered_butter_qty,
			delivered_ghee_qty, delivered_lassi_qty, delivered_paneer_qty,
			delivered_jaggery_qty, delivered_khand_qty, delivered_oil_qty,
			delivered_atta_qty, delivered_burfi_qty,
			unit_price_milk, unit_price_curd, unit_price_butter,
			unit_price_ghee, unit_price_lassi, unit_price_paneer,
			unit_price_jaggery, unit_price_khand, unit_price_oil,
			unit_price_atta, unit_price_burfi,
			driver_latitude, driver_longitude, captured_at, distance_meters, proof_image_url, is_flagged
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
			$19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29,
			$30, $31, $32, $33, $34, $35
		)
		ON CONFLICT (customer_id, delivery_date, slot)
		DO UPDATE SET
			status = EXCLUDED.status, route_id = EXCLUDED.route_id, driver_id = EXCLUDED.driver_id,
			delivered_milk_qty = EXCLUDED.delivered_milk_qty, delivered_curd_qty = EXCLUDED.delivered_curd_qty,
			delivered_butter_qty = EXCLUDED.delivered_butter_qty, delivered_ghee_qty = EXCLUDED.delivered_ghee_qty,
			delivered_lassi_qty = EXCLUDED.delivered_lassi_qty, delivered_paneer_qty = EXCLUDED.delivered_paneer_qty,
			delivered_jaggery_qty = EXCLUDED.delivered_jaggery_qty, delivered_khand_qty = EXCLUDED.delivered_khand_qty,
			delivered_oil_qty = EXCLUDED.delivered_oil_qty, delivered_atta_qty = EXCLUDED.delivered_atta_qty,
			delivered_burfi_qty = EXCLUDED.delivered_burfi_qty,
			unit_price_milk = EXCLUDED.unit_price_milk, unit_price_curd = EXCLUDED.unit_price_curd,
			unit_price_butter = EXCLUDED.unit_price_butter, unit_price_ghee = EXCLUDED.unit_price_ghee,
			unit_price_lassi = EXCLUDED.unit_price_lassi, unit_price_paneer = EXCLUDED.unit_price_paneer,
			unit_price_jaggery = EXCLUDED.unit_price_jaggery, unit_price_khand = EXCLUDED.unit_price_khand,
			unit_price_oil = EXCLUDED.unit_price_oil, unit_price_atta = EXCLUDED.unit_price_atta,
			unit_price_burfi = EXCLUDED.unit_price_burfi,
			driver_latitude = EXCLUDED.driver_latitude, driver_longitude = EXCLUDED.driver_longitude,
			captured_at = EXCLUDED.captured_at, distance_meters = EXCLUDED.distance_meters,
			proof_image_url = EXCLUDED.proof_image_url, is_flagged = EXCLUDED.is_flagged,
			updated_at = NOW()
	`

	_, err = dr.DB.Exec(r.Context(), queryUpsert,
		logId, payload.CustomerId, payload.RouteId, driverID, payload.Date, payload.Status, payload.Slot,
		finalOrder.Milk, finalOrder.Curd, finalOrder.Butter,
		finalOrder.Ghee, finalOrder.Lassi, finalOrder.Paneer,
		finalOrder.Jaggery, finalOrder.Khand, finalOrder.Oil,
		finalOrder.Atta, finalOrder.Burfi,
		prices[0], prices[1], prices[2], prices[3], prices[4], prices[5],
		prices[6], prices[7], prices[8], prices[9], prices[10],
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

// -------------------------------------------------------------------------
// Mobile: DriverHistory
// -------------------------------------------------------------------------

func (dr DeliveryResource) DriverHistory(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	query := `
		SELECT dl.id, dl.delivery_date, dl.status, dl.captured_at,
		       COALESCE(rt.name, 'Unknown') as route_name, c.name as customer_name, COALESCE(c.house_address, ''),
		       dl.delivered_milk_qty, dl.delivered_curd_qty, dl.delivered_butter_qty,
		       dl.delivered_ghee_qty, dl.delivered_lassi_qty, dl.delivered_paneer_qty,
		       dl.delivered_jaggery_qty, dl.delivered_khand_qty, dl.delivered_oil_qty,
		       dl.delivered_atta_qty, dl.delivered_burfi_qty
		FROM delivery_logs dl
		LEFT JOIN routes rt ON dl.route_id = rt.id
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

	type productNames struct {
		Name string
		Unit string
	}
	productMeta := []productNames{
		{"Milk", "L"}, {"Curd", "kg"}, {"Butter", "pkt"}, {"Ghee", "ml"},
		{"Lassi", "L"}, {"Paneer", "kg"}, {"Jaggery", "kg"}, {"Khand", "kg"},
		{"Oil", "L"}, {"Atta", "kg"}, {"Burfi", "kg"},
	}

	historyMap := make(map[string]*HistoryRecord)
	var orderedKeys []string

	for rows.Next() {
		var logId, status, routeName, custName, address string
		var rawDate time.Time
		var capturedAt *time.Time
		qty := make([]int, len(Products))

		dest := []any{&logId, &rawDate, &status, &capturedAt, &routeName, &custName, &address}
		for i := range qty {
			dest = append(dest, &qty[i])
		}
		if err := rows.Scan(dest...); err != nil {
			continue
		}

		dateKey := rawDate.Format("2006-01-02")
		groupKey := dateKey + "|" + routeName

		if _, exists := historyMap[groupKey]; !exists {
			orderedKeys = append(orderedKeys, groupKey)
			historyMap[groupKey] = &HistoryRecord{
				Id: groupKey, Date: rawDate.Format("Jan 02, 2006"),
				Timestamp: dateKey, RouteName: routeName, Stops: []PastStop{},
			}
		}

		record := historyMap[groupKey]
		record.TotalStops++
		if status == "DELIVERED" {
			record.CompletedStops++
			record.MilkTotal += qty[0]
		}

		var items []PastItemDetail
		for i, pm := range productMeta {
			if qty[i] > 0 {
				items = append(items, PastItemDetail{Name: pm.Name, Expected: qty[i], Actual: qty[i], Unit: pm.Unit})
			}
		}

		deliveryTime := ""
		if capturedAt != nil {
			deliveryTime = capturedAt.Format("03:04 PM")
		}

		record.Stops = append(record.Stops, PastStop{
			Id: logId, Customer: custName, Address: address, Status: status,
			DeliveryTime: deliveryTime, Items: items,
		})
	}

	response := []HistoryRecord{}
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

// -------------------------------------------------------------------------
// Admin: List (day view with all filters)
// -------------------------------------------------------------------------

var allowedStatus = map[string]bool{
	"DELIVERED": true, "SKIPPED": true, "FAILED": true,
	"UNATTEMPTED": true, "SYSTEM_AUTO_CLOSED": true,
}

func (dr DeliveryResource) List(w http.ResponseWriter, r *http.Request) {
	page, _ := r.Context().Value(middleware.PageKey).(int)
	limit, _ := r.Context().Value(middleware.LimitKey).(int)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}

	conds := []string{"1=1"}
	args := []any{}
	add := func(clause string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}

	q := r.URL.Query()
	if from, to := q.Get("from"), q.Get("to"); from != "" && to != "" {
		add("dl.delivery_date >= $%d", from)
		add("dl.delivery_date <= $%d", to)
	} else {
		date := q.Get("date")
		if date == "" && q.Get("customerId") == "" {
			date = time.Now().Format("2006-01-02")
		}
		if date != "" {
			add("dl.delivery_date = $%d", date)
		}
	}
	if v := q.Get("routeId"); v != "" {
		add("dl.route_id = $%d", v)
	}
	if v := q.Get("customerId"); v != "" {
		add("dl.customer_id = $%d", v)
	}
	if v := q.Get("driverId"); v != "" {
		add("dl.driver_id = $%d", v)
	}
	if v := q.Get("status"); v != "" {
		add("dl.status = $%d", v)
	}
	if v := q.Get("slot"); v != "" {
		add("dl.slot = $%d", v)
	}
	if q.Get("flagged") == "true" {
		conds = append(conds, "dl.is_flagged = true")
	}

	qtyCols := make([]string, 0, len(Products))
	for _, p := range Products {
		qtyCols = append(qtyCols, "COALESCE(dl.delivered_"+p+"_qty, 0)")
	}

	args = append(args, limit, (page-1)*limit)
	query := fmt.Sprintf(`
		SELECT dl.id, dl.customer_id, c.name, c.phone_number,
		       COALESCE(c.house_address, ''),
		       COALESCE(s.stop_order, 0),
		       dl.route_id, rt.name, drv.name,
		       dl.slot, TO_CHAR(dl.delivery_date, 'YYYY-MM-DD'), dl.status,
		       COALESCE(dl.is_flagged, false), dl.customer_feedback,
		       dl.captured_at, dl.updated_at,
		       %s
		FROM delivery_logs dl
		JOIN customers c ON c.id = dl.customer_id
		LEFT JOIN subscriptions s ON s.customer_id = dl.customer_id AND s.slot = dl.slot
		LEFT JOIN routes rt ON rt.id = dl.route_id
		LEFT JOIN drivers drv ON drv.id = dl.driver_id
		WHERE %s
		ORDER BY dl.delivery_date DESC, COALESCE(s.stop_order, 0) ASC
		LIMIT $%d OFFSET $%d
	`, strings.Join(qtyCols, ", "), strings.Join(conds, " AND "), len(args)-1, len(args))

	rows, err := dr.DB.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	logs := []AdminDeliveryLog{}
	for rows.Next() {
		var l AdminDeliveryLog
		qty := make([]int, len(Products))
		dest := []any{
			&l.Id, &l.CustomerId, &l.CustomerName, &l.PhoneNumber,
			&l.HouseAddress, &l.StopOrder,
			&l.RouteId, &l.RouteName, &l.DriverName,
			&l.Slot, &l.DeliveryDate, &l.Status,
			&l.IsFlagged, &l.Feedback, &l.CapturedAt, &l.UpdatedAt,
		}
		for i := range qty {
			dest = append(dest, &qty[i])
		}
		if err := rows.Scan(dest...); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		l.Quantities = map[string]int{}
		for i, p := range Products {
			if qty[i] > 0 {
				l.Quantities[p] = qty[i]
			}
		}
		logs = append(logs, l)
	}

	writeJSON(w, http.StatusOK, logs)
}

// -------------------------------------------------------------------------
// Admin: Update (partial correction to a log)
// -------------------------------------------------------------------------

func (dr DeliveryResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req UpdateLogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Status != nil && !allowedStatus[*req.Status] {
		http.Error(w, "Unrecognised status: "+*req.Status, http.StatusBadRequest)
		return
	}

	sets := []string{"updated_at = NOW()"}
	args := []any{}
	set := func(clause string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf(clause, len(args)))
	}

	if req.Status != nil {
		set("status = $%d", *req.Status)
	}
	if req.IsFlagged != nil {
		set("is_flagged = $%d", *req.IsFlagged)
	}
	if req.Feedback != nil {
		set("customer_feedback = $%d", *req.Feedback)
	}
	if req.Quantities != nil {
		for _, p := range Products {
			set("delivered_"+p+"_qty = $%d", (*req.Quantities)[p])
		}
	}
	if len(sets) == 1 {
		http.Error(w, "Nothing to update", http.StatusBadRequest)
		return
	}

	args = append(args, id)
	query := fmt.Sprintf(`UPDATE delivery_logs SET %s WHERE id = $%d RETURNING id`,
		strings.Join(sets, ", "), len(args))

	var updated uuid.UUID
	err := dr.DB.QueryRow(r.Context(), query, args...).Scan(&updated)
	if err == pgx.ErrNoRows {
		http.Error(w, "Delivery log not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": updated.String(), "message": "Delivery log updated"})
}

// -------------------------------------------------------------------------
// Admin: AdminUpsert (create or replace a log for a customer/date/slot)
// -------------------------------------------------------------------------

type UpsertLogRequest struct {
	CustomerId string         `json:"customerId"`
	RouteId    *string        `json:"routeId"`
	Slot       string         `json:"slot"`
	Date       string         `json:"date"`
	Status     string         `json:"status"`
	Quantities map[string]int `json:"quantities"`
	Feedback   *string        `json:"customerFeedback"`
}

func (dr DeliveryResource) AdminUpsert(w http.ResponseWriter, r *http.Request) {
	var req UpsertLogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.CustomerId == "" {
		http.Error(w, "customerId is required", http.StatusBadRequest)
		return
	}
	if req.Slot == "" {
		req.Slot = "morning"
	}
	if req.Date == "" {
		req.Date = time.Now().Format("2006-01-02")
	}
	if req.Status == "" {
		req.Status = "DELIVERED"
	}
	if !allowedStatus[req.Status] {
		http.Error(w, "Unrecognised status: "+req.Status, http.StatusBadRequest)
		return
	}

	cols := []string{"id", "customer_id", "route_id", "slot", "delivery_date", "status", "customer_feedback"}
	logID, _ := uuid.NewV7()
	vals := []any{logID, req.CustomerId, req.RouteId, req.Slot, req.Date, req.Status, req.Feedback}

	for _, p := range Products {
		cols = append(cols, "delivered_"+p+"_qty")
		vals = append(vals, req.Quantities[p])
	}

	placeholders := make([]string, len(vals))
	updates := make([]string, 0, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		if c != "id" && c != "customer_id" {
			updates = append(updates, c+" = EXCLUDED."+c)
		}
	}
	updates = append(updates, "updated_at = NOW()")

	query := fmt.Sprintf(`
		INSERT INTO delivery_logs (%s) VALUES (%s)
		ON CONFLICT (customer_id, delivery_date, slot) DO UPDATE SET %s
		RETURNING id
	`, strings.Join(cols, ", "), strings.Join(placeholders, ", "), strings.Join(updates, ", "))

	var savedID uuid.UUID
	if err := dr.DB.QueryRow(r.Context(), query, vals...).Scan(&savedID); err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": savedID.String(), "message": "Delivery log saved"})
}

// -------------------------------------------------------------------------
// Admin: Flagged + Resolve
// -------------------------------------------------------------------------

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
		SELECT dl.id, dl.customer_id, c.name, c.phone_number, COALESCE(c.house_address, ''),
		       dl.slot, dl.delivery_date, dl.status, COALESCE(dl.customer_feedback, ''), dl.updated_at
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
			&f.Slot, &f.DeliveryDate, &f.Status, &f.CustomerFeedback, &f.UpdatedAt,
		); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		flagged = append(flagged, f)
	}

	writeJSON(w, http.StatusOK, flagged)
}

func (dr DeliveryResource) Resolve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var resolvedID uuid.UUID
	err := dr.DB.QueryRow(r.Context(), `
		UPDATE delivery_logs SET is_flagged = false, updated_at = NOW()
		WHERE id = $1 AND is_flagged = true RETURNING id
	`, id).Scan(&resolvedID)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Flagged delivery not found or already resolved", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Marked as resolved"})
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	deltaPhi := (lat2 - lat1) * math.Pi / 180
	deltaLambda := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
