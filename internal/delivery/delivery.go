package delivery

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/registration"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/issue.html
var issueTemplateFS embed.FS
var issueTmpl = template.Must(template.ParseFS(issueTemplateFS, "templates/issue.html"))

var Products = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

// ProductLabels / litreProducts drive customer-facing formatting. Kept next to
// Products so adding a twelfth item is one edit in three obvious places.
var ProductLabels = map[string]string{
	"milk": "Milk", "curd": "Curd", "butter": "Butter", "ghee": "Ghee",
	"lassi": "Buttermilk", "paneer": "Paneer", "jaggery": "Jaggery",
	"khand": "Desi Khand", "oil": "Mustard Oil", "atta": "Atta", "burfi": "Burfi",
}

var litreProducts = map[string]bool{"milk": true, "curd": true, "lassi": true, "oil": true}

// formatQty turns a stored base-unit quantity into something a customer can
// read: 500 -> "500 ml", 2000 -> "2 L". Matches the formatter used by the
// customer-facing HTML pages and the driver app.
func formatQty(v int, product string) string {
	litre := litreProducts[product]
	if v < 1000 {
		if litre {
			return fmt.Sprintf("%d ml", v)
		}
		return fmt.Sprintf("%d g", v)
	}
	val := float64(v) / 1000
	unit := "kg"
	if litre {
		unit = "L"
	}
	// Trim a trailing .0 so 2000 reads "2 L" rather than "2.0 L".
	return strings.TrimSuffix(fmt.Sprintf("%.2f", val), ".00") + " " + unit
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
	// Quantities as recorded. The admin needs these in the flagged list so
	// they can correct the bill in the same step as clearing the flag —
	// without them, resolving a complaint and fixing the charge were two
	// separate journeys and the second one rarely happened.
	Quantities map[string]int `json:"quantities"`
	DayTotal   float64        `json:"dayTotal"`
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
	// Templates carries the approved Twilio content template SIDs. An empty
	// DeliveryDone SID falls back to the free-form confirmation.
	Templates notification.TemplateSIDs
}

func (dr DeliveryResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Admin routes
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Put("/{id}", dr.Update)
	r.Post("/admin", dr.AdminUpsert)
	r.With(middleware.Paginate).Get("/flagged", dr.ListFlagged)
	r.Post("/{id}/resolve", dr.Resolve)

	// Customer issue-reporting (token-gated HTML page, not admin, not driver)
	r.Get("/issue", dr.ShowIssueForm)
	r.Get("/issue/deliveries", dr.ListReportable)
	r.Post("/issue", dr.SubmitIssue)

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

	// Notify the customer. Fire-and-forget deliberately: a failed WhatsApp
	// send must never fail the delivery log itself — the driver has already
	// left the doorstep by the time this runs.
	if dr.WhatsApp != nil && payload.Status == "DELIVERED" {
		go dr.notifyDelivered(payload.CustomerId, payload.Slot, finalOrder)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"message": "Delivery logged successfully", "status": payload.Status})
}

// notifyDelivered sends the post-delivery WhatsApp confirmation. Runs on its
// own short-lived context since the parent request has already responded.
func (dr DeliveryResource) notifyDelivered(customerID uuid.UUID, slot string, order customer.Order) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var phone string
	if err := dr.DB.QueryRow(ctx, `SELECT phone_number FROM customers WHERE id = $1`, customerID).Scan(&phone); err != nil {
		fmt.Printf("⚠️ notifyDelivered: couldn't find phone for customer %s: %v\n", customerID, err)
		return
	}

	items := []struct {
		key string
		qty int
	}{
		{"milk", order.Milk}, {"curd", order.Curd}, {"butter", order.Butter}, {"ghee", order.Ghee},
		{"lassi", order.Lassi}, {"paneer", order.Paneer}, {"jaggery", order.Jaggery},
		{"khand", order.Khand}, {"oil", order.Oil}, {"atta", order.Atta}, {"burfi", order.Burfi},
	}
	var lines []string
	var flat []string
	for _, it := range items {
		if it.qty > 0 {
			// Quantities are stored in base units (ml / g). Printing the raw
			// number gave customers "Milk: 2000" several times a week.
			pretty := formatQty(it.qty, it.key)
			lines = append(lines, fmt.Sprintf("• %s: %s", ProductLabels[it.key], pretty))
			// Template variables cannot contain newlines, so the same list is
			// also built as one comma-separated line: "Milk 2 L, Curd 500 g".
			flat = append(flat, fmt.Sprintf("%s %s", ProductLabels[it.key], pretty))
		}
	}

	slotLabel := "Morning"
	if slot == "evening" {
		slotLabel = "Evening"
	}

	// Template first. This confirmation goes out after every delivery, to
	// customers who mostly haven't messaged us that day — the largest volume
	// of sends that the free-form 24-hour rule would reject with error 63016.
	if dr.Templates.DeliveryDone != "" {
		if err := dr.WhatsApp.SendDeliveryComplete(
			phone, dr.Templates.DeliveryDone, slotLabel, notification.JoinItems(flat),
		); err == nil {
			return
		} else {
			fmt.Printf("⚠️ notifyDelivered: template send failed for %s, falling back: %v\n", phone, err)
		}
	}

	msg := fmt.Sprintf("✅ Your %s delivery is complete!\n\n%s", slotLabel, strings.Join(lines, "\n"))
	dr.WhatsApp.SendDeliveryUpdate(phone, msg)
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
//
// Note this deliberately never touches unit_price_*. Those are the snapshot
// of what the customer was actually charged; re-reading today's route price
// while correcting a quantity would silently rewrite history if the route's
// price had moved since the delivery happened.
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

	// Resolve the price list to snapshot onto this log.
	//
	// Without this the unit_price_* columns fall back to their NUMERIC NOT
	// NULL DEFAULT 0, and billing — which computes delivered_qty ×
	// unit_price — silently produces ₹0 for every admin-created delivery.
	// That hit exactly the recovery path meant to prevent lost revenue: a
	// driver's offline delivery that permanently failed to sync and got
	// recreated by an admin from the sync-failures bucket.
	//
	// Prefer the route named on the request; fall back to whatever route the
	// customer's subscription for this slot points at, so an admin who
	// doesn't specify one still gets correct pricing.
	var routeArg any
	if req.RouteId != nil && *req.RouteId != "" {
		routeArg = *req.RouteId
	}

	prices := make([]float64, len(Products))
	priceCols := make([]string, 0, len(Products))
	priceDest := make([]any, 0, len(Products))
	for i, p := range Products {
		priceCols = append(priceCols, "COALESCE(rt.price_"+p+", 0)")
		priceDest = append(priceDest, &prices[i])
	}

	priceQuery := `
		SELECT ` + strings.Join(priceCols, ", ") + `
		FROM subscriptions s
		LEFT JOIN routes rt ON rt.id = COALESCE($2::uuid, s.route_id)
		WHERE s.customer_id = $1 AND s.slot = $3
		LIMIT 1
	`
	if err := dr.DB.QueryRow(r.Context(), priceQuery, req.CustomerId, routeArg, req.Slot).Scan(priceDest...); err != nil {
		if err == pgx.ErrNoRows {
			// Admin authority is absolute elsewhere, but a log we can't price
			// is worse than no log at all — it looks correct and bills nothing.
			http.Error(w, "Cannot determine pricing: this customer has no "+req.Slot+" subscription. Assign one first, or use a different slot.", http.StatusBadRequest)
			return
		}
		http.Error(w, "Failed to resolve pricing: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cols := []string{"id", "customer_id", "route_id", "slot", "delivery_date", "status", "customer_feedback"}
	logID, _ := uuid.NewV7()
	vals := []any{logID, req.CustomerId, req.RouteId, req.Slot, req.Date, req.Status, req.Feedback}

	for _, p := range Products {
		cols = append(cols, "delivered_"+p+"_qty")
		vals = append(vals, req.Quantities[p])
	}
	for i, p := range Products {
		cols = append(cols, "unit_price_"+p)
		vals = append(vals, prices[i])
	}

	placeholders := make([]string, len(vals))
	updates := make([]string, 0, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		// Prices are written on INSERT but never on UPDATE. Editing an
		// existing log must not reprice it — see the note on Update above.
		if c != "id" && c != "customer_id" && !strings.HasPrefix(c, "unit_price_") {
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

// ListFlagged now returns the recorded quantities and what the day was
// charged at, so the admin can correct the bill in the same action as
// clearing the flag.
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

	qtyCols := make([]string, 0, len(Products))
	valueTerms := make([]string, 0, len(Products))
	for _, p := range Products {
		qtyCols = append(qtyCols, "COALESCE(dl.delivered_"+p+"_qty, 0)")
		valueTerms = append(valueTerms,
			// Divided by 1000: prices are per LITRE or per KILOGRAM, quantities are in
			// millilitres or grams.
			//
			// routes.price_milk = 105 means rupees 105 per litre — which is what an admin
			// types and what anyone reading the table would assume. delivery_logs stores
			// that same figure as a snapshot at delivery time.
			//
			// Without the division, 2 L of milk billed as 2000 x 105 = rupees 210,000.
			// Every invoice was out by a factor of a thousand.
			//
			// Storing a per-millilitre price instead would remove the division but round
			// rupees 105/L to 0.11 in NUMERIC(10,2) — a 4.7% overcharge baked into the
			// schema. Keeping the human-readable price and dividing at the point of use is
			// both exact and legible.
			fmt.Sprintf("COALESCE(dl.delivered_%s_qty, 0) * COALESCE(dl.unit_price_%s, 0) / 1000.0", p, p))
	}

	query := fmt.Sprintf(`
		SELECT dl.id, dl.customer_id, c.name, c.phone_number, COALESCE(c.house_address, ''),
		       dl.slot, dl.delivery_date, dl.status, COALESCE(dl.customer_feedback, ''), dl.updated_at,
		       (%s) AS day_total,
		       %s
		FROM delivery_logs dl
		JOIN customers c ON c.id = dl.customer_id
		WHERE dl.is_flagged = true
		ORDER BY dl.updated_at DESC
		LIMIT $1 OFFSET $2
	`, strings.Join(valueTerms, " + "), strings.Join(qtyCols, ", "))

	rows, err := dr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	flagged := []FlaggedDelivery{}
	for rows.Next() {
		var f FlaggedDelivery
		qty := make([]int, len(Products))
		dest := []any{
			&f.Id, &f.CustomerId, &f.CustomerName, &f.PhoneNumber, &f.HouseAddress,
			&f.Slot, &f.DeliveryDate, &f.Status, &f.CustomerFeedback, &f.UpdatedAt,
			&f.DayTotal,
		}
		for i := range qty {
			dest = append(dest, &qty[i])
		}
		if err := rows.Scan(dest...); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		f.Quantities = map[string]int{}
		for i, p := range Products {
			f.Quantities[p] = qty[i]
		}
		flagged = append(flagged, f)
	}

	writeJSON(w, http.StatusOK, flagged)
}

// ResolveRequest is what the admin sends when closing out a complaint.
//
// Quantities is optional: omit it to simply clear the flag (the customer was
// mistaken, or the issue was handled another way). Supply it to correct what
// was recorded — which is what actually adjusts the bill, since billing sums
// delivered_qty × unit_price.
type ResolveRequest struct {
	Quantities *map[string]int `json:"quantities"`
	Note       *string         `json:"note"`
	// Defaults to true when quantities change. Set false to correct the
	// record silently — e.g. fixing a driver's typo the customer never saw.
	NotifyCustomer *bool `json:"notifyCustomer"`
}

// Resolve closes a flagged delivery, optionally correcting the quantities.
//
// This used to only flip is_flagged to false. That left the most common
// complaint — "an item was missing" — with the flag cleared and the customer
// still billed for the item they never received. The two halves of the fix
// were separate journeys and the second one rarely happened, so they're one
// action now.
func (dr DeliveryResource) Resolve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req ResolveRequest
	// An empty body is valid: "resolve, change nothing".
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Load the current state first — we need the old quantities and the
	// frozen unit prices to work out what the correction is worth.
	qtyCols := make([]string, 0, len(Products))
	priceCols := make([]string, 0, len(Products))
	for _, p := range Products {
		qtyCols = append(qtyCols, "COALESCE(delivered_"+p+"_qty, 0)")
		priceCols = append(priceCols, "COALESCE(unit_price_"+p+", 0)")
	}

	var customerID uuid.UUID
	var slot, deliveryDate string
	oldQty := make([]int, len(Products))
	unitPrice := make([]float64, len(Products))

	loadDest := []any{&customerID, &slot, &deliveryDate}
	for i := range oldQty {
		loadDest = append(loadDest, &oldQty[i])
	}
	for i := range unitPrice {
		loadDest = append(loadDest, &unitPrice[i])
	}

	loadQuery := fmt.Sprintf(`
		SELECT customer_id, slot, TO_CHAR(delivery_date, 'YYYY-MM-DD'), %s, %s
		FROM delivery_logs WHERE id = $1 AND is_flagged = true
	`, strings.Join(qtyCols, ", "), strings.Join(priceCols, ", "))

	if err := dr.DB.QueryRow(r.Context(), loadQuery, id).Scan(loadDest...); err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Flagged delivery not found or already resolved", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sets := []string{"is_flagged = false", "updated_at = NOW()"}
	args := []any{}

	// Work out the value change and a human-readable list of what moved.
	var amountAdjusted float64
	var changeLines []string

	if req.Quantities != nil {
		for i, p := range Products {
			newQ := (*req.Quantities)[p]
			if newQ < 0 {
				http.Error(w, "Quantity for "+p+" cannot be negative", http.StatusBadRequest)
				return
			}
			args = append(args, newQ)
			sets = append(sets, fmt.Sprintf("delivered_%s_qty = $%d", p, len(args)))

			if newQ != oldQty[i] {
				amountAdjusted += float64(oldQty[i]-newQ) * unitPrice[i]
				changeLines = append(changeLines, fmt.Sprintf(
					"• %s: %s → %s",
					ProductLabels[p], formatQty(oldQty[i], p), formatQty(newQ, p),
				))
			}
		}
	}

	// The note is appended rather than replacing the customer's own words —
	// their description of the problem is the audit trail.
	if req.Note != nil && *req.Note != "" {
		args = append(args, "\n[Admin] "+*req.Note)
		sets = append(sets, fmt.Sprintf("customer_feedback = COALESCE(customer_feedback, '') || $%d", len(args)))
	}

	args = append(args, id)
	updateQuery := fmt.Sprintf(`UPDATE delivery_logs SET %s WHERE id = $%d`,
		strings.Join(sets, ", "), len(args))

	if _, err := dr.DB.Exec(r.Context(), updateQuery, args...); err != nil {
		http.Error(w, "Failed to resolve: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// If this month is already invoiced, the frozen invoice is now stale.
	// We don't regenerate automatically — that's the admin's call, and a
	// paid invoice must not be silently rewritten — but we tell the UI so it
	// can offer the button rather than leaving a wrong bill in place.
	var invoiceID *uuid.UUID
	var invoiceStatus *string
	if len(changeLines) > 0 {
		month := deliveryDate[:7]
		dr.DB.QueryRow(r.Context(),
			`SELECT id, status FROM invoices WHERE customer_id = $1 AND billing_month = $2`,
			customerID, month,
		).Scan(&invoiceID, &invoiceStatus)
	}

	notify := len(changeLines) > 0
	if req.NotifyCustomer != nil {
		notify = *req.NotifyCustomer && len(changeLines) > 0
	}
	if notify && dr.WhatsApp != nil {
		go dr.notifyCorrection(customerID, slot, deliveryDate, changeLines, amountAdjusted)
	}

	resp := map[string]any{
		"message":        "Delivery resolved",
		"amountAdjusted": amountAdjusted,
		"changed":        len(changeLines) > 0,
	}
	if invoiceID != nil {
		resp["invoiceNeedsRegeneration"] = true
		resp["invoiceId"] = invoiceID.String()
		if invoiceStatus != nil {
			resp["invoiceStatus"] = *invoiceStatus
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// notifyCorrection tells the customer their record — and therefore their
// bill — has been put right. Leads with the acknowledgement rather than the
// numbers: someone who just complained wants to know they were heard.
func (dr DeliveryResource) notifyCorrection(
	customerID uuid.UUID, slot, deliveryDate string, changes []string, amountAdjusted float64,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var phone, name string
	if err := dr.DB.QueryRow(ctx,
		`SELECT phone_number, name FROM customers WHERE id = $1`, customerID,
	).Scan(&phone, &name); err != nil {
		fmt.Printf("⚠️ notifyCorrection: no phone for %s: %v\n", customerID, err)
		return
	}

	prettyDate := deliveryDate
	if t, err := time.Parse("2006-01-02", deliveryDate); err == nil {
		prettyDate = t.Format("2 January")
	}

	slotLabel := "morning"
	if slot == "evening" {
		slotLabel = "evening"
	}

	var billLine string
	switch {
	case amountAdjusted > 0:
		billLine = fmt.Sprintf("\n*Your bill has been reduced by ₹%.0f.*", amountAdjusted)
	case amountAdjusted < 0:
		billLine = fmt.Sprintf("\n*Your bill has been increased by ₹%.0f.*", -amountAdjusted)
	default:
		// Quantities moved but netted to zero — swapped items, say. No point
		// claiming a refund that isn't there.
		billLine = "\n*Your bill total is unchanged.*"
	}

	msg := fmt.Sprintf(
		"✅ Thanks for letting us know, %s.\n\n"+
			"We've checked your %s delivery from %s and corrected our records:\n\n%s\n%s\n\n"+
			"Sorry for the trouble.",
		name, slotLabel, prettyDate, strings.Join(changes, "\n"), billLine,
	)

	// Template first: an admin resolves a complaint whenever they get to it,
	// so the customer has usually not messaged us since reporting it and the
	// free-form version would be dropped outside the 24-hour window.
	//
	// The outcome is passed as a complete sentence rather than an amount.
	// That lets the one template cover a bill reduction, an increase, and a
	// net-zero correction — without it, a complaint resolved by explanation
	// closes silently and the customer never hears back at all.
	if dr.Templates.IssueResolved != "" {
		var outcome string
		switch {
		case amountAdjusted > 0:
			outcome = fmt.Sprintf("Your bill has been reduced by ₹%.0f.", amountAdjusted)
		case amountAdjusted < 0:
			outcome = fmt.Sprintf("Your bill has been increased by ₹%.0f.", -amountAdjusted)
		default:
			outcome = "Your bill total is unchanged."
		}

		// Template variables cannot contain newlines, so the per-item changes
		// collapse to one comma-separated line here.
		whatChanged := fmt.Sprintf("%s delivery on %s — %s",
			capitalise(slotLabel), prettyDate, strings.Join(changes, ", "))

		if err := dr.WhatsApp.SendIssueResolved(
			phone, dr.Templates.IssueResolved, name, whatChanged, outcome,
		); err == nil {
			return
		} else {
			fmt.Printf("⚠️ issue-resolved template failed for %s, falling back: %v\n", phone, err)
		}
	}

	if err := dr.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
		fmt.Printf("⚠️ notifyCorrection: send failed for %s: %v\n", phone, err)
	}
}

// capitalise upper-cases the first letter of a single lowercase word.
func capitalise(v string) string {
	if v == "" {
		return v
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

// -------------------------------------------------------------------------
// Customer: Issue Reporting
//
// A customer taps "Report an issue" in WhatsApp → webhook generates a token
// and sends the link to /delivery/issue?token=... → this file serves the HTML,
// lists the customer's recent DELIVERED logs still inside the complaint
// window, and accepts a submission that writes to is_flagged +
// customer_feedback. The admin dashboard's flagged list reads the same
// columns, so nothing on the admin side needed changing.
// -------------------------------------------------------------------------

func (dr DeliveryResource) ShowIssueForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := registration.ValidatePhone(ctx, dr.DB, token); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`<h2 style="font-family:sans-serif;padding:24px">This link has expired.</h2><p style="font-family:sans-serif;padding:0 24px">Please tap "Report an issue" from the main menu again to get a fresh link.</p>`))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	issueTmpl.Execute(w, map[string]string{"Token": token})
}

// reportableDelivery is what the page renders as a picker option.
type reportableDelivery struct {
	LogId          uuid.UUID      `json:"logId"`
	Date           string         `json:"date"`
	Slot           string         `json:"slot"`
	Status         string         `json:"status"`
	AlreadyFlagged bool           `json:"alreadyFlagged"`
	Items          map[string]int `json:"items"`
}

// ListReportable returns the customer's recent DELIVERED logs that are still
// inside the complaint window. The cutoff hour lives in system_config as
// complaint_cutoff_time and is interpreted as "this hour on the day AFTER
// delivery" — so a 2 AM value gives evening customers ~8 hours and morning
// customers ~22 hours. A NULL cutoff disables the window entirely.
func (dr DeliveryResource) ListReportable(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, dr.DB, token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	var cutoffTimeStr *string
	dr.DB.QueryRow(ctx, `SELECT complaint_cutoff_time::text FROM system_config LIMIT 1`).Scan(&cutoffTimeStr)

	// Always allow today. Allow yesterday only if we haven't crossed today's
	// cutoff yet. NULL cutoff = no window enforcement (useful for launch).
	dates := []string{today}
	includeYesterday := cutoffTimeStr == nil
	if cutoffTimeStr != nil {
		if cutoff, err := time.ParseInLocation("15:04:05", *cutoffTimeStr, loc); err == nil {
			cutoffToday := time.Date(now.Year(), now.Month(), now.Day(), cutoff.Hour(), cutoff.Minute(), 0, 0, loc)
			if now.Before(cutoffToday) {
				includeYesterday = true
			}
		}
	}
	if includeYesterday {
		dates = append(dates, yesterday)
	}

	// Only DELIVERED rows are surfaced — there's nothing meaningful to
	// complain about on a skipped/unattempted stop. Ordering: newest first.
	q := `
		SELECT dl.id, dl.delivery_date::text, dl.slot, dl.status, COALESCE(dl.is_flagged, false),
		       dl.delivered_milk_qty, dl.delivered_curd_qty, dl.delivered_butter_qty,
		       dl.delivered_ghee_qty, dl.delivered_lassi_qty, dl.delivered_paneer_qty,
		       dl.delivered_jaggery_qty, dl.delivered_khand_qty, dl.delivered_oil_qty,
		       dl.delivered_atta_qty, dl.delivered_burfi_qty
		FROM delivery_logs dl
		JOIN customers c ON c.id = dl.customer_id
		WHERE c.phone_number = $1
		  AND dl.status = 'DELIVERED'
		  AND dl.delivery_date::text = ANY($2)
		ORDER BY dl.delivery_date DESC, dl.slot ASC
	`
	rows, err := dr.DB.Query(ctx, q, phone, dates)
	if err != nil {
		http.Error(w, "Failed to load deliveries: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []reportableDelivery{}
	for rows.Next() {
		var d reportableDelivery
		var milk, curd, butter, ghee, lassi, paneer, jaggery, khand, oil, atta, burfi int
		if err := rows.Scan(&d.LogId, &d.Date, &d.Slot, &d.Status, &d.AlreadyFlagged,
			&milk, &curd, &butter, &ghee, &lassi, &paneer, &jaggery, &khand, &oil, &atta, &burfi); err != nil {
			continue
		}
		d.Items = map[string]int{
			"milk": milk, "curd": curd, "butter": butter, "ghee": ghee,
			"lassi": lassi, "paneer": paneer, "jaggery": jaggery, "khand": khand,
			"oil": oil, "atta": atta, "burfi": burfi,
		}
		out = append(out, d)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"deliveries": out})
}

type issueSubmitPayload struct {
	Token    string `json:"token"`
	LogId    string `json:"logId"`
	Feedback string `json:"feedback"`
}

// SubmitIssue flags the chosen log. The token authorises the phone; we
// re-check ownership on the log row itself so a leaked or replayed token
// can't be used to flag deliveries belonging to a different customer.
func (dr DeliveryResource) SubmitIssue(w http.ResponseWriter, r *http.Request) {
	var p issueSubmitPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if p.LogId == "" || p.Feedback == "" {
		http.Error(w, "logId and feedback are both required", http.StatusBadRequest)
		return
	}
	if len(p.Feedback) > 500 {
		p.Feedback = p.Feedback[:500]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	phone, err := registration.ValidatePhone(ctx, dr.DB, p.Token)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// One statement, one round trip: the WHERE clause both scopes to the
	// caller's customer_id (via phone) and guards against cross-customer
	// flagging via a leaked logId.
	tag, err := dr.DB.Exec(ctx, `
		UPDATE delivery_logs
		SET is_flagged = true, customer_feedback = $1, updated_at = NOW()
		WHERE id = $2
		  AND customer_id = (SELECT id FROM customers WHERE phone_number = $3)
	`, p.Feedback, p.LogId, phone)
	if err != nil {
		http.Error(w, "Failed to log issue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "That delivery couldn't be found or doesn't belong to you.", http.StatusNotFound)
		return
	}

	registration.MarkUsed(ctx, dr.DB, p.Token)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	fmt.Printf("✅ Issue logged for %s on log %s: %q\n", phone, p.LogId, p.Feedback)
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
