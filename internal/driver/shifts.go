package driver

// Shift lifecycle for drivers.
//
// One row per (driver_id, shift_date, slot) in the `shifts` table. Morning
// and evening are independent — a driver can end one and leave the other
// running, and the sweeper treats them separately.
//
// Design decisions worth remembering:
//   - Backend is the source of truth. The mobile app mirrors this in
//     AsyncStorage for instant cold-start rendering and offline resilience,
//     but on reconnect the backend always wins.
//   - Start/end are idempotent via the unique index. The client can safely
//     retry a Start request without creating a second row.
//   - The auto-end sweeper is intentionally soft: it flips ACTIVE →
//     AUTO_ENDED past the evening cutoff so the admin dashboard's "runs in
//     progress" count doesn't stay elevated overnight, but the driver's app
//     stays fully functional. A delivery logged after AUTO_ENDED writes
//     normally — the shift row is a housekeeping marker, not a wall.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -------------------------------------------------------------------------
// Types
// -------------------------------------------------------------------------

// ShiftState is what the mobile app reads on cold-start. Both slots are
// returned every time — this is one round-trip instead of two, and the app
// can render both toggle buttons at once.
type ShiftState struct {
	Date    string       `json:"date"`
	Morning *ShiftRow    `json:"morning"`
	Evening *ShiftRow    `json:"evening"`
	Cutoffs ShiftCutoffs `json:"cutoffs"`
}

type ShiftRow struct {
	Id        uuid.UUID  `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	Status    string     `json:"status"`
}

// Cutoffs surfaced so the client can display "auto-ends at 21:00" hints
// without a separate config fetch.
type ShiftCutoffs struct {
	MorningCutoff string `json:"morningCutoff"` // HH:MM:SS
	EveningCutoff string `json:"eveningCutoff"` // HH:MM:SS
}

type shiftMutationRequest struct {
	Slot string `json:"slot"`
}

// -------------------------------------------------------------------------
// Routes — wired from driver.go
// -------------------------------------------------------------------------
//
// In driver.go's Routes() method, add these three lines inside the mobile
// group (the r.Group that uses SupabaseAuth):
//
//     r.Post("/shift/start", dr.StartShift)
//     r.Post("/shift/end",   dr.EndShift)
//     r.Get("/shift/today",  dr.GetTodayShifts)
//
// -------------------------------------------------------------------------

// GetTodayShifts returns both slots' current state for the calling driver.
// A nil slot means "no shift started today for this slot" — the client
// renders a Start button in that case.
func (dr DriverResource) GetTodayShifts(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)
	today := indianToday()

	state := ShiftState{Date: today}

	rows, err := dr.DB.Query(r.Context(), `
		SELECT id, slot, started_at, ended_at, status
		FROM shifts
		WHERE driver_id = $1 AND shift_date = $2::date
	`, driverID, today)
	if err != nil {
		http.Error(w, "Failed loading shifts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var s ShiftRow
		var slot string
		if err := rows.Scan(&s.Id, &slot, &s.StartedAt, &s.EndedAt, &s.Status); err != nil {
			continue
		}
		row := s
		if slot == "morning" {
			state.Morning = &row
		} else {
			state.Evening = &row
		}
	}

	// Cutoffs are best-effort; a missing row shouldn't break the response.
	dr.DB.QueryRow(r.Context(),
		`SELECT morning_cutoff_time::text, evening_cutoff_time::text FROM system_config LIMIT 1`,
	).Scan(&state.Cutoffs.MorningCutoff, &state.Cutoffs.EveningCutoff)

	writeJSON(w, http.StatusOK, state)
}

// StartShift creates an ACTIVE row for the caller for today's given slot.
// Idempotent: a second call with the same slot on the same day returns the
// existing row rather than erroring, so the client can safely retry.
func (dr DriverResource) StartShift(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var req shiftMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Slot != "morning" && req.Slot != "evening" {
		http.Error(w, "slot must be 'morning' or 'evening'", http.StatusBadRequest)
		return
	}

	today := indianToday()
	shiftID, _ := uuid.NewV7()

	// ON CONFLICT DO NOTHING gives us idempotency; the follow-up SELECT
	// returns whichever row now owns (driver, date, slot) — the newly
	// inserted one or the pre-existing one. Both are equally valid answers.
	_, err := dr.DB.Exec(r.Context(), `
		INSERT INTO shifts (id, driver_id, shift_date, slot, status)
		VALUES ($1, $2, $3::date, $4, 'ACTIVE')
		ON CONFLICT (driver_id, shift_date, slot) DO NOTHING
	`, shiftID, driverID, today, req.Slot)
	if err != nil {
		http.Error(w, "Failed to start shift: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var row ShiftRow
	err = dr.DB.QueryRow(r.Context(), `
		SELECT id, started_at, ended_at, status
		FROM shifts WHERE driver_id = $1 AND shift_date = $2::date AND slot = $3
	`, driverID, today, req.Slot).Scan(&row.Id, &row.StartedAt, &row.EndedAt, &row.Status)
	if err != nil {
		http.Error(w, "Shift saved but reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, row)
}

// EndShift closes the ACTIVE shift for this driver/slot/today. If the shift
// is already ENDED or AUTO_ENDED, this is a no-op and returns the existing
// row (idempotent, same reasoning as StartShift).
func (dr DriverResource) EndShift(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var req shiftMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if req.Slot != "morning" && req.Slot != "evening" {
		http.Error(w, "slot must be 'morning' or 'evening'", http.StatusBadRequest)
		return
	}

	today := indianToday()

	// Only flip ACTIVE → ENDED; if the row is already terminal, leave it.
	// This means an auto-ended shift stays labeled AUTO_ENDED even if the
	// driver taps End afterward — reflecting what actually happened.
	_, err := dr.DB.Exec(r.Context(), `
		UPDATE shifts
		SET status = 'ENDED', ended_at = NOW(), updated_at = NOW()
		WHERE driver_id = $1 AND shift_date = $2::date AND slot = $3 AND status = 'ACTIVE'
	`, driverID, today, req.Slot)
	if err != nil {
		http.Error(w, "Failed to end shift: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var row ShiftRow
	err = dr.DB.QueryRow(r.Context(), `
		SELECT id, started_at, ended_at, status
		FROM shifts WHERE driver_id = $1 AND shift_date = $2::date AND slot = $3
	`, driverID, today, req.Slot).Scan(&row.Id, &row.StartedAt, &row.EndedAt, &row.Status)
	if err != nil {
		http.Error(w, "Shift saved but reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, row)
}

// -------------------------------------------------------------------------
// Sync failures endpoint
// -------------------------------------------------------------------------
//
// The mobile client posts here when it decides to permanently give up on a
// queued delivery — either because the backend keeps 400ing it (bad data,
// e.g. the customer got deleted mid-shift) or because 48h of retries with
// exponential backoff haven't succeeded.
//
// Also wire this route in driver.go's mobile group:
//     r.Post("/sync-failures", dr.ReportSyncFailure)

type syncFailureReq struct {
	Payload      map[string]any `json:"payload"`
	ErrorMessage string         `json:"errorMessage"`
	Reason       string         `json:"reason"` // REJECTED_400 | TIMED_OUT_48H | OTHER
}

var allowedFailureReason = map[string]bool{
	"REJECTED_400": true, "TIMED_OUT_48H": true, "OTHER": true,
}

func (dr DriverResource) ReportSyncFailure(w http.ResponseWriter, r *http.Request) {
	driverID := r.Context().Value(middleware.UserIDKey).(string)

	var req syncFailureReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if !allowedFailureReason[req.Reason] {
		http.Error(w, "reason must be REJECTED_400, TIMED_OUT_48H, or OTHER", http.StatusBadRequest)
		return
	}
	if len(req.Payload) == 0 {
		http.Error(w, "payload is required", http.StatusBadRequest)
		return
	}

	// Cap error text length — the client might send stack traces or long
	// server responses, and there's no reason to store more than ~2kB.
	if len(req.ErrorMessage) > 2000 {
		req.ErrorMessage = req.ErrorMessage[:2000]
	}

	payloadJSON, err := json.Marshal(req.Payload)
	if err != nil {
		http.Error(w, "Cannot serialize payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	failureID, _ := uuid.NewV7()
	_, err = dr.DB.Exec(r.Context(), `
		INSERT INTO delivery_sync_failures (id, driver_id, payload, error_message, reason)
		VALUES ($1, $2, $3, $4, $5)
	`, failureID, driverID, payloadJSON, req.ErrorMessage, req.Reason)
	if err != nil {
		http.Error(w, "Failed to record: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Printf("⚠️ sync failure recorded — driver=%s reason=%s error=%q\n", driverID, req.Reason, req.ErrorMessage)
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded", "id": failureID.String()})
}

// -------------------------------------------------------------------------
// Auto-end sweeper — background goroutine
// -------------------------------------------------------------------------

// StartAutoEndSweeper runs a ticker that closes stale ACTIVE shifts.
//
// The rule: at the evening cutoff (from system_config), any ACTIVE shift for
// today gets flipped to AUTO_ENDED. This runs every 5 minutes, so the worst-
// case delay is 5 min past cutoff. That precision is fine — this is a
// dashboard housekeeping tick, not a payment settlement.
//
// Start this from main.go once, after the DB pool is ready:
//
//	go driver.StartAutoEndSweeper(pool)
func StartAutoEndSweeper(db *pgxpool.Pool) {
	// One immediate sweep on startup catches any shifts we missed while the
	// server was down; then settle into the regular cadence.
	sweepAutoEnd(db)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sweepAutoEnd(db)
	}
}

func sweepAutoEnd(db *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	var morningCutoffStr, eveningCutoffStr string
	err := db.QueryRow(ctx,
		`SELECT morning_cutoff_time::text, evening_cutoff_time::text FROM system_config LIMIT 1`,
	).Scan(&morningCutoffStr, &eveningCutoffStr)
	if err != nil {
		fmt.Printf("⚠️ sweepAutoEnd: no system_config row: %v\n", err)
		return
	}

	// Morning slot rule: closes when the *evening* cutoff passes, since the
	// morning cutoff is about override deadlines, not shift end. Both slots
	// therefore auto-end at the same wall-clock moment (typically 21:00 IST).
	// Kept a single check for both to make the sweeper's behaviour obvious
	// to anyone reading it later.
	if !cutoffPassed(now, eveningCutoffStr, loc) {
		return
	}

	tag, err := db.Exec(ctx, `
		UPDATE shifts
		SET status = 'AUTO_ENDED', ended_at = NOW(), updated_at = NOW()
		WHERE shift_date = $1::date AND status = 'ACTIVE'
	`, today)
	if err != nil {
		fmt.Printf("⚠️ sweepAutoEnd: update failed: %v\n", err)
		return
	}
	if tag.RowsAffected() > 0 {
		fmt.Printf("🕘 auto-ended %d stale shift(s) for %s\n", tag.RowsAffected(), today)
	}
}

// cutoffPassed returns true if `now` (already in IST) is at or after the
// wall-clock cutoff time. Handles the HH:MM:SS format Postgres returns.
func cutoffPassed(now time.Time, cutoffStr string, loc *time.Location) bool {
	cutoff, err := time.ParseInLocation("15:04:05", cutoffStr, loc)
	if err != nil {
		return false
	}
	cutoffToday := time.Date(now.Year(), now.Month(), now.Day(),
		cutoff.Hour(), cutoff.Minute(), cutoff.Second(), 0, loc)
	return !now.Before(cutoffToday)
}

// -------------------------------------------------------------------------
// Local helpers
// -------------------------------------------------------------------------

// indianToday returns today's date string in IST. Kept as a helper because
// server clock may be UTC and "today" for the driver means IST calendar day.
func indianToday() string {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	return time.Now().In(loc).Format("2006-01-02")
}

// writeJSON — small local helper. driver.go doesn't currently have one and
// I'd rather not add a package-level utility that lives in a random file.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
