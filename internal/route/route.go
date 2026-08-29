package route

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Route no longer carries a single driver_id — driver assignment is per-slot
// and lives in route_slot_drivers. Prices are per-route.
type Route struct {
	Id          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`

	// Per-route price list, returned on Get/List so the admin UI can render
	// and edit them without a separate call.
	Prices map[string]float64 `json:"prices,omitempty"`

	// Populated by List/Get via a secondary query — tells the admin who's
	// running this route right now without navigating to a separate screen.
	SlotDrivers []SlotDriver `json:"slotDrivers,omitempty"`
}

type SlotDriver struct {
	Slot        string     `json:"slot"`
	DriverId    *uuid.UUID `json:"driverId"`
	DriverName  *string    `json:"driverName"`
	DriverPhone *string    `json:"driverPhone"`
}

// Products is the canonical list, same order as every other package.
var Products = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
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

		r.Put("/prices", rr.SetPrices)
		r.Get("/prices/pending", rr.GetPendingPrices)
		r.Delete("/prices/pending", rr.CancelPendingPrices)
		r.Put("/sequence", rr.UpdateSequence)
		r.Put("/driver", rr.UpdateDriver)

		r.Get("/manifest", rr.GetManifest)
		// Admin correction to a locked manifest. A cutoff should stop
		// customers changing their order, not stop the business fixing a
		// mistake before the van leaves.
		r.Put("/manifest/plan", rr.EditPlannedOrder)
		r.Get("/roster", rr.GetRoster)
	})

	return r
}

// -------------------------------------------------------------------------
// CRUD
// -------------------------------------------------------------------------

func (rr RouteResource) Create(w http.ResponseWriter, r *http.Request) {
	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Failed to decode payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	u, _ := uuid.NewV7()
	rt.Id = u

	query := `INSERT INTO routes (id, name, description) VALUES ($1, $2, $3)`
	_, err := rr.DB.Exec(r.Context(), query, rt.Id, rt.Name, rt.Description)
	if err != nil {
		http.Error(w, "Failed to create route: "+err.Error(), http.StatusInternalServerError)
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
	page, _ := r.Context().Value(middleware.PageKey).(int)
	limit, _ := r.Context().Value(middleware.LimitKey).(int)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		// Unreachable in practice: middleware.Paginate always puts a value in
		// the context, so this branch only fires if the handler is ever
		// mounted without that middleware. Kept as a floor, not a default —
		// the real default lives in middleware/pagination.go.
		limit = 100
	}
	offset := (page - 1) * limit

	query := `
		SELECT id, name, COALESCE(description, ''), created_at,
		       price_milk, price_curd, price_butter, price_ghee, price_lassi,
		       price_paneer, price_jaggery, price_khand, price_oil, price_atta, price_burfi
		FROM routes
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`
	rows, err := rr.DB.Query(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	routes := []Route{}
	for rows.Next() {
		rt, err := scanRoute(rows)
		if err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		routes = append(routes, rt)
	}

	// Attach slot-driver info in one batch instead of N+1 queries.
	if len(routes) > 0 {
		if err := attachSlotDrivers(r.Context(), rr.DB, routes); err != nil {
			http.Error(w, "Driver lookup error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusOK, routes)
}

func (rr RouteResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	query := `
		SELECT id, name, COALESCE(description, ''), created_at,
		       price_milk, price_curd, price_butter, price_ghee, price_lassi,
		       price_paneer, price_jaggery, price_khand, price_oil, price_atta, price_burfi
		FROM routes WHERE id = $1
	`
	rt, err := scanRoute(rr.DB.QueryRow(r.Context(), query, id))
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	routes := []Route{rt}
	_ = attachSlotDrivers(r.Context(), rr.DB, routes)
	rt = routes[0]

	writeJSON(w, http.StatusOK, rt)
}

func (rr RouteResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var rt Route
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	query := `UPDATE routes SET name = $1, description = $2 WHERE id = $3`
	_, err := rr.DB.Exec(r.Context(), query, rt.Name, rt.Description, id)
	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Route updated successfully"})
}

func (rr RouteResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Cascades through route_slot_drivers and nulls subscriptions.route_id
	// via ON DELETE SET NULL.
	_, err := rr.DB.Exec(r.Context(), `DELETE FROM routes WHERE id = $1`, id)
	if err != nil {
		http.Error(w, "Failed to delete route: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Route deleted and subscriptions safely unassigned"})
}

// -------------------------------------------------------------------------
// Prices
// -------------------------------------------------------------------------

type PricePayload struct {
	Prices map[string]float64 `json:"prices"`
}

// UpdatePrices edits the current price list for a route. Partial maps are
// fine — products you omit keep their current value.
func (rr RouteResource) UpdatePrices(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var payload PricePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if len(payload.Prices) == 0 {
		http.Error(w, "No prices provided", http.StatusBadRequest)
		return
	}

	sets := []string{}
	args := []any{}
	for _, p := range Products {
		val, ok := payload.Prices[p]
		if !ok {
			continue
		}
		if val < 0 {
			http.Error(w, "Price for "+p+" cannot be negative", http.StatusBadRequest)
			return
		}
		args = append(args, val)
		sets = append(sets, "price_"+p+" = $"+itoa(len(args)))
	}
	if len(sets) == 0 {
		http.Error(w, "No recognised products in payload", http.StatusBadRequest)
		return
	}

	args = append(args, id)
	query := "UPDATE routes SET " + join(sets, ", ") + " WHERE id = $" + itoa(len(args))
	_, err := rr.DB.Exec(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Route prices updated"})
}

// -------------------------------------------------------------------------
// Driver assignment (per slot)
// -------------------------------------------------------------------------

type DriverPayload struct {
	Slot     string  `json:"slot"`     // "morning" or "evening"
	DriverId *string `json:"driverId"` // null to unassign
}

// UpdateDriver assigns or clears the driver for one slot on a route.
func (rr RouteResource) UpdateDriver(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")

	var payload DriverPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if payload.Slot == "" {
		payload.Slot = "morning"
	}
	if payload.Slot != "morning" && payload.Slot != "evening" {
		http.Error(w, "slot must be 'morning' or 'evening'", http.StatusBadRequest)
		return
	}

	query := `
		INSERT INTO route_slot_drivers (route_id, slot, driver_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (route_id, slot) DO UPDATE SET driver_id = EXCLUDED.driver_id, updated_at = NOW()
	`
	_, err := rr.DB.Exec(r.Context(), query, routeId, payload.Slot, payload.DriverId)
	if err != nil {
		http.Error(w, "Assignment failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Driver link updated on route for " + payload.Slot})
}

// -------------------------------------------------------------------------
// Roster — admin sequence editor
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
	Slot      string       `json:"slot"`
	Stops     []RosterStop `json:"stops"`
}

// GetRoster returns every subscription assigned to this route+slot, used by
// the admin's drag-and-drop sequence editor. ?slot defaults to "morning".
func (rr RouteResource) GetRoster(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")
	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = "morning"
	}

	var response RosterResponse
	response.Slot = slot

	err := rr.DB.QueryRow(r.Context(), `SELECT id, name FROM routes WHERE id = $1`, routeId).Scan(&response.RouteID, &response.RouteName)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Roster now reads from subscriptions (where route assignment lives)
	// joined to customers (where identity lives).
	customerQuery := `
		SELECT c.id, c.name, c.phone_number, COALESCE(c.house_address, ''), s.stop_order, c.is_active
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		WHERE s.route_id = $1 AND s.slot = $2 AND c.status IN ('active', 'disabled')
		ORDER BY s.stop_order ASC
	`
	rows, err := rr.DB.Query(r.Context(), customerQuery, routeId, slot)
	if err != nil {
		http.Error(w, "Error fetching roster: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	response.Stops = []RosterStop{}
	for rows.Next() {
		var stop RosterStop
		if err := rows.Scan(&stop.Id, &stop.Name, &stop.PhoneNumber, &stop.HouseAddress, &stop.StopOrder, &stop.IsActive); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		response.Stops = append(response.Stops, stop)
	}

	writeJSON(w, http.StatusOK, response)
}

// -------------------------------------------------------------------------
// Sequence
// -------------------------------------------------------------------------

type SequencePayload struct {
	Slot        string   `json:"slot"` // defaults to "morning"
	CustomerIDs []string `json:"customerIds"`
}

// UpdateSequence reorders stops on a route+slot. stop_order now lives on
// subscriptions, not customers.
func (rr RouteResource) UpdateSequence(w http.ResponseWriter, r *http.Request) {
	routeId := chi.URLParam(r, "id")

	var payload SequencePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Slot == "" {
		payload.Slot = "morning"
	}

	tx, err := rr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction start failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	query := `
		UPDATE subscriptions
		SET stop_order = $1, updated_at = NOW()
		WHERE customer_id = $2 AND route_id = $3 AND slot = $4
	`

	for index, customerId := range payload.CustomerIDs {
		stopOrder := index + 1
		_, err := tx.Exec(r.Context(), query, stopOrder, customerId, routeId, payload.Slot)
		if err != nil {
			http.Error(w, "Failed at customer "+customerId+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Route sequence updated successfully"})
}

// -------------------------------------------------------------------------
// Manifest — driver-facing, slot-aware
// -------------------------------------------------------------------------

type ManifestStop struct {
	CustomerId   uuid.UUID `json:"id"`
	CustomerName string    `json:"customer"`
	PhoneNumber  string    `json:"phoneNumber"`
	HouseAddress string    `json:"houseAddress"`
	GeoLatitude  string    `json:"geoLatitude"`
	GeoLongitude string    `json:"geoLongitude"`
	IsActive     bool      `json:"isActive"`
	StopOrder    int       `json:"stopOrder"`

	DeliveryOrder customer.Order `json:"deliveryOrder"`
	Status        *string        `json:"status"`
	ActualOrder   customer.Order `json:"actualOrder"`

	// Locked means this stop's plan was frozen at the slot's cutoff and is
	// authoritative. False means the manifest is still being computed live —
	// either the cutoff hasn't passed, or the date predates locking.
	Locked bool `json:"locked"`
	// PlanEdited means an admin changed the plan after it was locked. Shown
	// to the driver so a list that changes mid-round reads as deliberate
	// rather than as the app misbehaving.
	PlanEdited bool `json:"planEdited"`
}

type ManifestResponse struct {
	RouteID     uuid.UUID      `json:"routeId"`
	RouteName   string         `json:"routeName"`
	Slot        string         `json:"slot"`
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
	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = "morning"
	}

	manifest, err := GenerateManifest(rr.DB, r.Context(), routeId, targetDate, slot)
	if err != nil {
		http.Error(w, "Error generating manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, manifest)
}

// GenerateManifest is the shared function used by both the admin route
// endpoint and the driver's mobile manifest. It is now slot-aware:
// subscriptions, overrides, and delivery logs all filter on the same slot.
func GenerateManifest(db *pgxpool.Pool, ctx context.Context, routeId string, targetDate string, slot string) (ManifestResponse, error) {
	var manifest ManifestResponse
	manifest.TargetDate = targetDate
	manifest.Slot = slot

	// Route meta + driver for this slot from route_slot_drivers.
	metaQuery := `
		SELECT r.id, r.name, d.name, d.phone_number
		FROM routes r
		LEFT JOIN route_slot_drivers rsd ON rsd.route_id = r.id AND rsd.slot = $2
		LEFT JOIN drivers d ON d.id = rsd.driver_id
		WHERE r.id = $1
	`
	err := db.QueryRow(ctx, metaQuery, routeId, slot).Scan(
		&manifest.RouteID, &manifest.RouteName, &manifest.DriverName, &manifest.DriverPhone,
	)
	if err != nil {
		return manifest, err
	}

	// The big manifest query.
	//
	// The planned quantities now read locked plan FIRST, then override, then
	// subscription default. That order is the whole point of manifest
	// locking: once a round is frozen at its cutoff, delivery_logs.planned_order
	// is the authoritative list, and later edits to a subscription must not
	// retroactively change what the driver was asked to deliver.
	//
	// Without that precedence, paging back to a past date rebuilt the plan
	// from TODAY's subscriptions — so a customer who changed their order last
	// week saw this week's quantities against last week's date. The delivered
	// column was right; the planned column was fiction.
	//
	// Unlocked rounds (before the cutoff, or dates predating this feature)
	// fall through to the live computation exactly as before.
	customerQuery := `
		SELECT
			c.id, c.name, c.phone_number, COALESCE(c.house_address, ''),
			COALESCE(c.geo_latitude, ''), COALESCE(c.geo_longitude, ''),
			c.is_active, s.stop_order,

` + schedule.ScheduledQtyList("s", "o", "dl", "$2", "\t\t\t") + `,

			dl.status,
			dl.locked_at IS NOT NULL,
			dl.plan_edited_at IS NOT NULL,

			COALESCE(dl.delivered_milk_qty, 0), COALESCE(dl.delivered_curd_qty, 0),
			COALESCE(dl.delivered_butter_qty, 0), COALESCE(dl.delivered_ghee_qty, 0),
			COALESCE(dl.delivered_lassi_qty, 0), COALESCE(dl.delivered_paneer_qty, 0),
			COALESCE(dl.delivered_jaggery_qty, 0), COALESCE(dl.delivered_khand_qty, 0),
			COALESCE(dl.delivered_oil_qty, 0), COALESCE(dl.delivered_atta_qty, 0),
			COALESCE(dl.delivered_burfi_qty, 0)

		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON o.customer_id = c.id AND o.target_date = $2 AND o.slot = $3
		LEFT JOIN delivery_logs dl ON dl.customer_id = c.id AND dl.delivery_date = $2 AND dl.slot = $3
		WHERE s.route_id = $1 AND s.slot = $3
		  AND c.is_active = TRUE AND s.stop_order > 0
		  AND (
			  -- Any item due, or an explicit override for this date.
			  --
			  -- Per-item schedules mean a customer can have days with nothing
			  -- scheduled at all — alternate-day butter and nothing else, on
			  -- an off day. Such a stop must not appear: there is nothing to
			  -- deliver, and it is not a missed delivery.
			  ` + schedule.AnyItemDueExpr("s", "$2") + `
			  OR o.id IS NOT NULL
		  )
		ORDER BY s.stop_order ASC
	`

	rows, err := db.Query(ctx, customerQuery, routeId, targetDate, slot)
	if err != nil {
		return manifest, err
	}
	defer rows.Close()

	manifest.Stops = []ManifestStop{}
	for rows.Next() {
		var stop ManifestStop
		err := rows.Scan(
			&stop.CustomerId, &stop.CustomerName, &stop.PhoneNumber, &stop.HouseAddress,
			&stop.GeoLatitude, &stop.GeoLongitude, &stop.IsActive, &stop.StopOrder,

			&stop.DeliveryOrder.Milk, &stop.DeliveryOrder.Curd, &stop.DeliveryOrder.Butter, &stop.DeliveryOrder.Ghee,
			&stop.DeliveryOrder.Lassi, &stop.DeliveryOrder.Paneer, &stop.DeliveryOrder.Jaggery, &stop.DeliveryOrder.Khand,
			&stop.DeliveryOrder.Oil, &stop.DeliveryOrder.Atta, &stop.DeliveryOrder.Burfi,

			&stop.Status,
			&stop.Locked,
			&stop.PlanEdited,

			&stop.ActualOrder.Milk, &stop.ActualOrder.Curd, &stop.ActualOrder.Butter, &stop.ActualOrder.Ghee,
			&stop.ActualOrder.Lassi, &stop.ActualOrder.Paneer, &stop.ActualOrder.Jaggery, &stop.ActualOrder.Khand,
			&stop.ActualOrder.Oil, &stop.ActualOrder.Atta, &stop.ActualOrder.Burfi,
		)
		if err != nil {
			return manifest, err
		}

		// Skip zero-quantity stops (customer paused for the day via override).
		o := stop.DeliveryOrder
		if o.Milk == 0 && o.Curd == 0 && o.Butter == 0 && o.Ghee == 0 && o.Lassi == 0 &&
			o.Paneer == 0 && o.Jaggery == 0 && o.Khand == 0 && o.Oil == 0 && o.Atta == 0 && o.Burfi == 0 {
			continue
		}

		manifest.Stops = append(manifest.Stops, stop)
	}

	return manifest, nil
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

func scanRoute(row pgx.Row) (Route, error) {
	var rt Route
	prices := make([]float64, len(Products))
	dest := []any{&rt.Id, &rt.Name, &rt.Description, &rt.CreatedAt}
	for i := range prices {
		dest = append(dest, &prices[i])
	}
	if err := row.Scan(dest...); err != nil {
		return rt, err
	}
	rt.Prices = map[string]float64{}
	for i, p := range Products {
		rt.Prices[p] = prices[i]
	}
	return rt, nil
}

// attachSlotDrivers batch-loads driver info for a slice of routes — one query
// for the whole page instead of N+1 individual lookups.
func attachSlotDrivers(ctx context.Context, db *pgxpool.Pool, routes []Route) error {
	ids := make([]uuid.UUID, len(routes))
	byId := map[uuid.UUID]int{}
	for i, rt := range routes {
		ids[i] = rt.Id
		byId[rt.Id] = i
		routes[i].SlotDrivers = []SlotDriver{}
	}

	query := `
		SELECT rsd.route_id, rsd.slot, d.id, d.name, d.phone_number
		FROM route_slot_drivers rsd
		LEFT JOIN drivers d ON d.id = rsd.driver_id
		WHERE rsd.route_id = ANY($1)
		ORDER BY rsd.route_id, rsd.slot
	`
	rows, err := db.Query(ctx, query, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var routeId uuid.UUID
		var sd SlotDriver
		if err := rows.Scan(&routeId, &sd.Slot, &sd.DriverId, &sd.DriverName, &sd.DriverPhone); err != nil {
			return err
		}
		if idx, ok := byId[routeId]; ok {
			routes[idx].SlotDrivers = append(routes[idx].SlotDrivers, sd)
		}
	}
	return rows.Err()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// Tiny helpers to avoid importing fmt/strconv just for query building.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func join(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// -------------------------------------------------------------------------
// Admin: edit a locked manifest
// -------------------------------------------------------------------------

// EditPlanRequest changes what a driver is asked to deliver at one stop.
type EditPlanRequest struct {
	CustomerID string         `json:"customerId"`
	Date       string         `json:"date"`
	Slot       string         `json:"slot"`
	Items      map[string]int `json:"items"` // base units, keyed milk/curd/...
}

// EditPlannedOrder updates a locked stop's planned quantities.
//
// PUT /route/{id}/manifest/plan
//
// # WHY THIS HAS TO EXIST
//
// Locking a manifest at the cutoff is what stops the driver's list changing
// under them mid-round. But it also means an admin who spots a mistake at
// 03:00 — a customer who rang to cancel after the deadline, an order entered
// wrong — has no way to correct it, and the van goes out with the wrong load.
//
// A cutoff should stop CUSTOMERS changing their order, not stop the business
// fixing an error. So this exists, and it stamps plan_edited_at, which the
// driver's app surfaces: a list that changes mid-round then reads as a
// deliberate correction rather than a glitch.
func (rr RouteResource) EditPlannedOrder(w http.ResponseWriter, r *http.Request) {
	var req EditPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.CustomerID == "" || req.Date == "" || req.Slot == "" {
		http.Error(w, "customerId, date and slot are required", http.StatusBadRequest)
		return
	}

	// Only meaningful on a locked row. An unlocked manifest is still computed
	// live from subscriptions and overrides, so editing the plan there would
	// be silently overwritten on the next request — the admin should change
	// the subscription or the override instead.
	var locked bool
	err := rr.DB.QueryRow(r.Context(), `
		SELECT locked_at IS NOT NULL
		FROM delivery_logs
		WHERE customer_id = $1::uuid AND delivery_date = $2::date AND slot = $3
	`, req.CustomerID, req.Date, req.Slot).Scan(&locked)
	if err == pgx.ErrNoRows {
		http.Error(w, "This stop isn't on the manifest for that date.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !locked {
		http.Error(w, "This manifest isn't locked yet — change the customer's order or add an override instead.", http.StatusConflict)
		return
	}

	// Store EVERY product, zeros included — matching how the lock sweeper
	// writes a plan, and for the same reason.
	//
	// Omitting a zero would make GenerateManifest fall through to the
	// customer's subscription for that product, so setting milk to zero here
	// would silently restore their usual milk on the next refresh. An admin
	// cancelling one item is the main reason to edit a locked plan at all, so
	// this is the case that must work.
	clean := map[string]int{}
	for _, p := range lockProducts {
		v := req.Items[p]
		if v < 0 {
			v = 0
		}
		clean[p] = v
	}
	payload, err := json.Marshal(clean)
	if err != nil {
		http.Error(w, "Could not encode the order", http.StatusInternalServerError)
		return
	}

	if _, err := rr.DB.Exec(r.Context(), `
		UPDATE delivery_logs
		SET planned_order = $4::jsonb, plan_edited_at = NOW(), updated_at = NOW()
		WHERE customer_id = $1::uuid AND delivery_date = $2::date AND slot = $3
	`, req.CustomerID, req.Date, req.Slot, string(payload)); err != nil {
		http.Error(w, "Could not save the change: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Manifest updated. The driver will see the change on their next refresh.",
	})
}
