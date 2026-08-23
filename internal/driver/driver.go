package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/route"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Driver struct {
	Id          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	PhoneNumber string    `json:"phoneNumber"`
	IsActive    bool      `json:"isActive"`
	CreatedAt   time.Time `json:"createdAt"`
}

type DriverResource struct {
	DB *pgxpool.Pool
}

func (dr DriverResource) Routes() chi.Router {
	r := chi.NewRouter()

	// Admin routes
	r.With(middleware.Paginate).Get("/", dr.List)
	r.Post("/", dr.Create)

	// Admin: sync-failure review. Outside the Supabase group — the admin
	// app authenticates locally and holds no Supabase JWT.
	r.Get("/sync-failures", dr.ListSyncFailures)
	r.Post("/sync-failures/{id}/resolve", dr.ResolveSyncFailure)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", dr.Get)
		r.Put("/", dr.Update)
		r.Delete("/", dr.Delete)
	})

	// Mobile routes (protected by Supabase JWT)
	r.Group(func(r chi.Router) {
		r.Use(middleware.SupabaseAuth)
		r.Post("/sync", dr.Sync)
		r.Get("/manifest", dr.GetMobileManifest)
		r.Post("/route/close", dr.CloseRoute)
		// Standing route assignments, independent of the current slot.
		r.Get("/routes", dr.GetMyRoutes)

		// Shift lifecycle
		r.Get("/shift/today", dr.GetTodayShifts)
		r.Post("/shift/start", dr.StartShift)
		r.Post("/shift/end", dr.EndShift)

		// Offline-queue sync failure reporting (driver-side)
		r.Post("/sync-failures", dr.ReportSyncFailure)
	})

	return r
}

// -------------------------------------------------------------------------
// Admin CRUD — unchanged from original except we never touch routes.driver_id
// -------------------------------------------------------------------------

func (dr DriverResource) Create(w http.ResponseWriter, r *http.Request) {
	var d Driver
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	u, _ := uuid.NewV7()
	d.Id = u

	_, err := dr.DB.Exec(r.Context(), `INSERT INTO drivers (id, name, phone_number) VALUES ($1, $2, $3)`, d.Id, d.Name, d.PhoneNumber)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": d.Id.String(), "message": "Driver registered successfully"})
}

func (dr DriverResource) List(w http.ResponseWriter, r *http.Request) {
	page, ok := r.Context().Value(middleware.PageKey).(int)
	if !ok {
		page = 1
	}
	limit, ok := r.Context().Value(middleware.LimitKey).(int)
	if !ok {
		limit = 10
	}
	offset := (page - 1) * limit

	rows, err := dr.DB.Query(r.Context(),
		`SELECT id, name, phone_number, is_active, created_at FROM drivers ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	drivers := []Driver{}
	for rows.Next() {
		var d Driver
		if err := rows.Scan(&d.Id, &d.Name, &d.PhoneNumber, &d.IsActive, &d.CreatedAt); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		drivers = append(drivers, d)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(drivers)
}

func (dr DriverResource) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var d Driver
	err := dr.DB.QueryRow(r.Context(), `SELECT id, name, phone_number, is_active, created_at FROM drivers WHERE id = $1`, id).
		Scan(&d.Id, &d.Name, &d.PhoneNumber, &d.IsActive, &d.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "Driver not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}

func (dr DriverResource) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var d Driver
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	_, err := dr.DB.Exec(r.Context(),
		`UPDATE drivers SET name = $1, phone_number = $2, is_active = $3 WHERE id = $4`,
		d.Name, d.PhoneNumber, d.IsActive, id)
	if err != nil {
		http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver profile updated successfully"})
}

func (dr DriverResource) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	_, err := dr.DB.Exec(r.Context(), `UPDATE drivers SET is_active = FALSE WHERE id = $1`, id)
	if err != nil {
		http.Error(w, "Deactivation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Driver profile deactivated"})
}

// -------------------------------------------------------------------------
// Mobile — Sync, Manifest, CloseRoute
// -------------------------------------------------------------------------

func (dr *DriverResource) Sync(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(middleware.UserIDKey).(string)

	var phone string
	if p, ok := r.Context().Value(middleware.PhoneKey).(string); ok {
		phone = p
	}

	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = "Unknown Driver"
	}

	_, err := dr.DB.Exec(r.Context(), `
		INSERT INTO drivers (id, name, phone_number) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET phone_number = EXCLUDED.phone_number
	`, userID, req.Name, phone)

	if err != nil {
		fmt.Printf("\n🚨 DB SYNC ERROR: %v\n\n", err)
		http.Error(w, `{"error": "Sync failed"}`, http.StatusInternalServerError)
		return
	}

	fmt.Println("✅ Driver synced. UUID:", userID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "synced"}`))
}

// GetMobileManifest looks up the route assigned to this driver via
// route_slot_drivers instead of the old routes.driver_id column.
//
// Slot detection: if a ?slot query param is given, use it. Otherwise infer
// from the current time against system_config cutoffs. If the driver is
// assigned to multiple routes (morning on A, evening on B), the slot
// determines which route they see.
func (dr *DriverResource) GetMobileManifest(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	targetDate := r.URL.Query().Get("date")
	if targetDate == "" {
		targetDate = time.Now().Format("2006-01-02")
	}

	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = inferSlot(r.Context(), dr.DB)
	}

	// Find which route this driver runs for the given slot.
	var routeID string
	err := dr.DB.QueryRow(r.Context(), `
		SELECT route_id FROM route_slot_drivers
		WHERE driver_id = $1 AND slot = $2
		LIMIT 1
	`, driverID, slot).Scan(&routeID)

	if err != nil {
		if err == pgx.ErrNoRows {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "no_route_for_slot",
				"slot":    slot,
				"message": "No route assigned to this driver for the " + slot + " slot",
			})
			return
		}
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	manifest, err := route.GenerateManifest(dr.DB, r.Context(), routeID, targetDate, slot)
	if err != nil {
		http.Error(w, "Error generating manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

// effectiveOrderTotal builds the SQL expression for "how much is actually
// owed on this stop" — the override quantity if one exists, otherwise the
// subscription default, summed across all products.
//
// NOTE: this predicate is now the third place expressing "is a delivery due
// here" (the others being route.GenerateManifest and stats.dueTodayCTE).
// Worth consolidating when one of them next needs to change.
const effectiveOrderTotal = `(
	  COALESCE(o.new_milk_qty,    s.default_milk_qty,    0)
	+ COALESCE(o.new_curd_qty,    s.default_curd_qty,    0)
	+ COALESCE(o.new_butter_qty,  s.default_butter_qty,  0)
	+ COALESCE(o.new_ghee_qty,    s.default_ghee_qty,    0)
	+ COALESCE(o.new_lassi_qty,   s.default_lassi_qty,   0)
	+ COALESCE(o.new_paneer_qty,  s.default_paneer_qty,  0)
	+ COALESCE(o.new_jaggery_qty, s.default_jaggery_qty, 0)
	+ COALESCE(o.new_khand_qty,   s.default_khand_qty,   0)
	+ COALESCE(o.new_oil_qty,     s.default_oil_qty,     0)
	+ COALESCE(o.new_atta_qty,    s.default_atta_qty,    0)
	+ COALESCE(o.new_burfi_qty,   s.default_burfi_qty,   0)
)`

// CloseRoute marks undelivered stops as UNATTEMPTED for the driver's current
// slot.
//
// Kept as an endpoint for manual use, but the important callers are now
// EndShift and the auto-end sweeper — see CloseRouteForSlot below for why.
func (dr *DriverResource) CloseRoute(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = inferSlot(r.Context(), dr.DB)
	}

	n, err := CloseRouteForSlot(r.Context(), dr.DB, driverID, slot, time.Now().Format("2006-01-02"))
	if err != nil {
		if err == errNoRouteForSlot {
			http.Error(w, `{"error": "No active route for this slot"}`, http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to close route: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"message": "Route closed successfully", "closed": %d}`, n)
}

var errNoRouteForSlot = errors.New("no route assigned for this driver and slot")

// CloseRouteForSlot ends a round: every stop the driver didn't reach becomes
// UNATTEMPTED.
//
// # WHY THIS IS NOW A SHARED FUNCTION RATHER THAN JUST AN ENDPOINT
//
// POST /driver/route/close was never called by anything. Not by the driver
// app, not by EndShift, not by the auto-end sweeper. It sat orphaned.
//
// Before manifest locking that was survivable: an unvisited stop simply had no
// delivery_logs row. Missing data, but inert — billing counts only DELIVERED
// and nothing else looked.
//
// Locking changed that. Every stop now gets a PENDING row at the slot's
// cutoff, and with nothing closing the round those rows stay PENDING forever:
// the dashboard reports a run still in progress days later, a stop nobody
// reached never becomes a recorded miss, and completion figures are quietly
// wrong.
//
// So closing is wired to the two moments a round actually ends — the driver
// tapping "end shift", and the sweeper auto-ending a shift they forgot to
// close. A driver who never opens the app at all is covered by the second.
//
// Returns how many stops were closed. Idempotent: a second call finds nothing
// PENDING and nothing missing, and changes nothing.
func CloseRouteForSlot(ctx context.Context, db *pgxpool.Pool, driverID, slot, targetDate string) (int, error) {
	var routeID string
	err := db.QueryRow(ctx, `
		SELECT route_id FROM route_slot_drivers
		WHERE driver_id = $1 AND slot = $2 LIMIT 1
	`, driverID, slot).Scan(&routeID)
	if err != nil {
		return 0, errNoRouteForSlot
	}

	// Insert UNATTEMPTED logs for every scheduled customer on this route+slot
	// who doesn't already have a delivery log for today.
	//
	// On a locked round this finds nothing — the lock sweeper already wrote a
	// row per stop. It still matters for dates before locking existed, and for
	// a round whose cutoff hasn't passed but whose driver has finished early.
	//
	// The final condition excludes stops with nothing owed — a customer who
	// paused today via a zero-quantity override. GenerateManifest already
	// filters those out, so the driver never saw them; without this they'd be
	// recorded as a missed delivery the driver was never asked to make. That
	// polluted the admin's log view with false failures and dragged the
	// dashboard's completion rate below 100% on any day someone skipped.
	insertQuery := `
		INSERT INTO delivery_logs (customer_id, route_id, driver_id, delivery_date, status, slot)
		SELECT
			s.customer_id, s.route_id, $4, $2::date, 'UNATTEMPTED', $3
		FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		LEFT JOIN order_overrides o ON o.customer_id = c.id AND o.target_date = $2::date AND o.slot = $3
		LEFT JOIN delivery_logs dl ON dl.customer_id = c.id AND dl.delivery_date = $2::date AND dl.slot = $3
		WHERE s.route_id = $1 AND s.slot = $3
		  AND c.is_active = TRUE AND s.stop_order > 0
		  AND dl.id IS NULL
		  AND (
			  s.schedule_type = 'daily'
			  OR (s.schedule_type = 'custom' AND EXTRACT(DOW FROM $2::date)::int = ANY(s.active_days))
			  OR (s.schedule_type = 'alternate' AND ($2::date - s.anchor_date) % 2 = 0)
			  OR o.id IS NOT NULL
		  )
		  AND ` + effectiveOrderTotal + ` > 0
	`

	inserted, err := db.Exec(ctx, insertQuery, routeID, targetDate, slot, driverID)
	if err != nil {
		return 0, fmt.Errorf("inserting unattempted logs: %w", err)
	}

	// Convert leftover PENDING stops to UNATTEMPTED.
	//
	// A stop still PENDING when the round closes is exactly what UNATTEMPTED
	// means: it was on the list and nobody reached it.
	updated, err := db.Exec(ctx, `
		UPDATE delivery_logs
		SET status = 'UNATTEMPTED', driver_id = COALESCE(driver_id, $4), updated_at = NOW()
		WHERE route_id = $1 AND delivery_date = $2::date AND slot = $3
		  AND status = 'PENDING'
	`, routeID, targetDate, slot, driverID)
	if err != nil {
		return 0, fmt.Errorf("closing pending stops: %w", err)
	}

	return int(inserted.RowsAffected() + updated.RowsAffected()), nil
}

// inferSlot picks "morning" or "evening" based on the current time and the
// cutoff stored in system_config. If it's before the evening cutoff, the
// driver is probably still on their morning run; after it, they're on evening.
// Falls back to "morning" if anything goes wrong.
func inferSlot(ctx context.Context, db *pgxpool.Pool) string {
	var eveningCutoff string
	err := db.QueryRow(ctx, `SELECT evening_cutoff_time FROM system_config LIMIT 1`).Scan(&eveningCutoff)
	if err != nil {
		return "morning"
	}
	now := time.Now().Format("15:04")
	if now >= eveningCutoff {
		return "evening"
	}
	return "morning"
}

// -------------------------------------------------------------------------
// Mobile: which routes am I on?
// -------------------------------------------------------------------------

// MyRoute is one of the driver's standing route assignments.
type MyRoute struct {
	RouteID   string `json:"routeId"`
	RouteName string `json:"routeName"`
	Slot      string `json:"slot"`
	StopCount int    `json:"stopCount"`
}

// GetMyRoutes returns every route the driver is assigned to, across both slots.
//
// GET /driver/routes
//
// # WHY THIS EXISTS
//
// The driver profile screen used to answer "what route am I on?" by calling
// /driver/manifest and reading routeName off the response. That endpoint
// infers the slot from the current time, so a driver assigned to mornings who
// opened their profile in the afternoon got a 404 and the screen told them
// "You are not assigned to a delivery route today."
//
// They were assigned. The question was just being asked about the wrong slot.
//
// Assignment is a standing fact about the driver, not a property of the
// current hour, so it needs its own query. This also answers it for both
// slots at once, which the manifest never could.
func (dr *DriverResource) GetMyRoutes(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	rows, err := dr.DB.Query(r.Context(), `
		SELECT r.id::text, r.name, rsd.slot,
		       (SELECT COUNT(*) FROM subscriptions s
		         WHERE s.route_id = r.id AND s.slot = rsd.slot AND s.stop_order > 0)
		FROM route_slot_drivers rsd
		JOIN routes r ON r.id = rsd.route_id
		WHERE rsd.driver_id = $1::uuid
		ORDER BY rsd.slot
	`, driverID)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []MyRoute{}
	for rows.Next() {
		var m MyRoute
		if err := rows.Scan(&m.RouteID, &m.RouteName, &m.Slot, &m.StopCount); err != nil {
			continue
		}
		out = append(out, m)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
