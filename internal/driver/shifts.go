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

// ShiftCutoffs surfaces the auto-end times so the client can display
// "auto-ends at 22:00" hints without a separate config fetch.
//
// These are the SHIFT END times, not the customer order cutoffs. The two
// were the same column until the shift-end split; naming them
// morningCutoff/eveningCutoff while they carried order deadlines is what
// made the sweeper bug hard to see. Named for what they are now.
type ShiftCutoffs struct {
	// When a round may START.
	//
	// These are the customers' ORDER cutoffs, not a separate setting. The
	// manifest locks at the cutoff, so before it the driver's list is still
	// live and a customer can change tomorrow's order out from under a van
	// that has already been loaded. After it, the list is frozen and safe to
	// work from.
	//
	// Reusing the order cutoff means there is one deadline per slot rather
	// than two that could drift apart.
	MorningStart string `json:"morningStart"` // HH:MM:SS — morning_cutoff_time
	EveningStart string `json:"eveningStart"` // HH:MM:SS — evening_cutoff_time

	// When a round is considered over. An ACTIVE shift past this is
	// auto-ended by the sweeper within five minutes, so starting one after
	// this time produces a shift that ends itself — which reads as the app
	// malfunctioning.
	MorningShiftEnd string `json:"morningShiftEnd"` // HH:MM:SS
	EveningShiftEnd string `json:"eveningShiftEnd"` // HH:MM:SS
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

	// Best-effort; a missing row shouldn't break the response. The client
	// treats empty strings as "no window known" and leaves the buttons
	// enabled rather than locking a driver out on a config read failure.
	dr.DB.QueryRow(r.Context(), `
		SELECT morning_cutoff_time::text, evening_cutoff_time::text,
		       morning_shift_end_time::text, evening_shift_end_time::text
		FROM system_config LIMIT 1
	`).Scan(
		&state.Cutoffs.MorningStart, &state.Cutoffs.EveningStart,
		&state.Cutoffs.MorningShiftEnd, &state.Cutoffs.EveningShiftEnd,
	)

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

	// Starting reopens an ended shift rather than doing nothing.
	//
	// This used to be ON CONFLICT DO NOTHING, which stranded any driver who
	// ended their shift early. Thumb the End button at stop 8 of 20 and the
	// round closes: stops 9-20 flip to UNATTEMPTED. Tap Start again and the
	// shift stayed ENDED, the manifest returned UNATTEMPTED for every
	// remaining stop, and the app — which treats anything other than PENDING
	// as finished — rendered them greyed out with no deliver button.
	//
	// Twelve customers left and no way to record any of them. The same thing
	// happened, without anyone touching anything, when the auto-end sweeper
	// closed a driver still out on a late round.
	//
	// ended_at is cleared so the shift reads as genuinely in progress again.
	_, err := dr.DB.Exec(r.Context(), `
		INSERT INTO shifts (id, driver_id, shift_date, slot, status)
		VALUES ($1, $2, $3::date, $4, 'ACTIVE')
		ON CONFLICT (driver_id, shift_date, slot) DO UPDATE
		SET status = 'ACTIVE',
		    ended_at = NULL,
		    -- Only stamped when reopening something that had ended, so a
		    -- driver simply double-tapping Start doesn't look like a restart.
		    reopened_at = CASE
		        WHEN shifts.status IN ('ENDED', 'AUTO_ENDED') THEN NOW()
		        ELSE shifts.reopened_at
		    END,
		    updated_at = NOW()
	`, shiftID, driverID, today, req.Slot)
	if err != nil {
		http.Error(w, "Failed to start shift: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reopen the round: stops closed as UNATTEMPTED go back to PENDING.
	//
	// Only today's, only this slot, and only stops nobody has actually
	// recorded — a DELIVERED, SKIPPED or FAILED stop is a decision the driver
	// made and must not be undone by restarting.
	//
	// Without this, reopening the shift would fix the label while leaving
	// every remaining stop un-deliverable, which is the worse failure: it
	// looks resolved.
	if n, rerr := reopenRound(r.Context(), dr.DB, driverID, req.Slot, today); rerr != nil {
		// Logged, not returned. The driver has asked to start and the shift
		// is already ACTIVE; refusing here would leave them on a screen they
		// can't get past for a bookkeeping failure.
		fmt.Printf("⚠️ StartShift: reopening %s round for driver %s failed: %v\n", req.Slot, driverID, rerr)
	} else if n > 0 {
		fmt.Printf("↩️  reopened %s round for driver %s — %d stop(s) back to pending\n", req.Slot, driverID, n)
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

	// Close the round: anything the driver didn't reach becomes UNATTEMPTED.
	//
	// This is the moment a round actually ends, and nothing was doing it. The
	// close endpoint existed but no caller ever hit it, which since manifest
	// locking means every unvisited stop stayed PENDING indefinitely — showing
	// on the dashboard as a run still in progress days later, and never
	// counting as a missed delivery.
	//
	// Failure here is logged, not returned. The driver has finished and told
	// us so; refusing to end their shift because the bookkeeping failed would
	// leave them stuck on a screen they can't get past, and the auto-end
	// sweeper will close the round anyway.
	if n, cerr := CloseRouteForSlot(r.Context(), dr.DB, driverID, req.Slot, today); cerr != nil {
		if cerr != errNoRouteForSlot {
			fmt.Printf("⚠️ EndShift: closing route for driver %s (%s) failed: %v\n", driverID, req.Slot, cerr)
		}
	} else if n > 0 {
		fmt.Printf("🏁 closed %s round for driver %s — %d stop(s) marked unattempted\n", req.Slot, driverID, n)
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
// The rule: each slot has its own shift end time in system_config, and an
// ACTIVE shift for that slot is flipped to AUTO_ENDED once today's clock
// passes it. Runs every 5 minutes, so worst-case delay is 5 min past the
// time. That precision is fine — this is a dashboard housekeeping tick, not
// a payment settlement.
//
// This used to read evening_cutoff_time and apply it to BOTH slots. That
// column is the customer's deadline to change an evening order (14:00), not
// a shift boundary, so an evening driver starting their run at 16:00 was
// auto-ended on the next sweep. The dashboard reported zero runs in progress
// while vans were still out — during the evening run, which is precisely
// when someone is watching that number.
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

	var morningEndStr, eveningEndStr string
	err := db.QueryRow(ctx,
		`SELECT morning_shift_end_time::text, evening_shift_end_time::text FROM system_config LIMIT 1`,
	).Scan(&morningEndStr, &eveningEndStr)
	if err != nil {
		fmt.Printf("⚠️ sweepAutoEnd: no system_config row: %v\n", err)
		return
	}

	// Each slot closes on its own clock. Morning vans are back by noon;
	// evening vans run until ten. Sweeping them together would either end
	// evening shifts mid-round or leave morning shifts open all afternoon.
	for _, s := range []struct {
		slot   string
		endStr string
	}{
		{"morning", morningEndStr},
		{"evening", eveningEndStr},
	} {
		if !cutoffPassed(now, s.endStr, loc) {
			continue
		}

		// Collect the drivers first: once the status flips to AUTO_ENDED we
		// can no longer tell which shifts this pass ended, and their rounds
		// still need closing.
		// reopened_at IS NULL excludes drivers who deliberately restarted
		// after an auto-end. Without it the sweeper and the driver fight:
		// ended at 11:30, restarted at 11:35, ended again at 11:40, forever.
		//
		// This sweeper exists to catch a driver who FORGOT to end their
		// shift. One who has explicitly restarted is telling us they are
		// still out, and that is not a case to correct.
		var driverIDs []string
		rows, qerr := db.Query(ctx, `
			SELECT driver_id::text FROM shifts
			WHERE shift_date = $1::date AND slot = $2 AND status = 'ACTIVE'
			  AND reopened_at IS NULL
		`, today, s.slot)
		if qerr != nil {
			fmt.Printf("⚠️ sweepAutoEnd: %s listing failed: %v\n", s.slot, qerr)
			continue
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				driverIDs = append(driverIDs, id)
			}
		}
		rows.Close()

		tag, err := db.Exec(ctx, `
			UPDATE shifts
			SET status = 'AUTO_ENDED', ended_at = NOW(), updated_at = NOW()
			WHERE shift_date = $1::date AND slot = $2 AND status = 'ACTIVE'
			  AND reopened_at IS NULL
		`, today, s.slot)
		if err != nil {
			// Log and carry on to the other slot — one failing UPDATE must
			// not leave the second slot unswept for the rest of the day.
			fmt.Printf("⚠️ sweepAutoEnd: %s update failed: %v\n", s.slot, err)
			continue
		}
		if tag.RowsAffected() > 0 {
			fmt.Printf("🕘 auto-ended %d stale %s shift(s) for %s\n", tag.RowsAffected(), s.slot, today)
		}

		// Close each auto-ended driver's round.
		//
		// A driver who forgets to tap "end shift" is precisely the case this
		// sweeper exists for, and leaving their round open would keep every
		// unvisited stop PENDING forever. Ending the shift without closing
		// the round would fix the dashboard's "runs in progress" count while
		// leaving the delivery records wrong — worse than not sweeping at
		// all, because it looks resolved.
		for _, id := range driverIDs {
			if n, cerr := CloseRouteForSlot(ctx, db, id, s.slot, today); cerr != nil {
				if cerr != errNoRouteForSlot {
					fmt.Printf("⚠️ sweepAutoEnd: closing %s round for driver %s failed: %v\n", s.slot, id, cerr)
				}
			} else if n > 0 {
				fmt.Printf("🏁 auto-closed %s round for driver %s — %d stop(s) marked unattempted\n", s.slot, id, n)
			}
		}
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

// reopenRound puts a closed round back into a deliverable state.
//
// Called when a driver starts a shift that was already ended — by mistake, or
// by the auto-end sweeper catching them mid-round.
//
// Deliberately narrow: today only, this slot only, and only stops sitting at
// UNATTEMPTED. A stop the driver actually marked — DELIVERED, SKIPPED,
// FAILED — represents a decision, and restarting a shift is not a reason to
// discard it.
//
// planned_order and locked_at are untouched, so a reopened stop still carries
// the plan it was locked with. The driver sees the same quantities they were
// asked to deliver, not a fresh computation from subscriptions that may have
// changed since.
func reopenRound(ctx context.Context, db *pgxpool.Pool, driverID, slot, date string) (int, error) {
	tag, err := db.Exec(ctx, `
		UPDATE delivery_logs dl
		SET status = 'PENDING', updated_at = NOW()
		FROM route_slot_drivers rsd
		WHERE rsd.driver_id = $1::uuid AND rsd.slot = $2
		  AND dl.route_id = rsd.route_id
		  AND dl.delivery_date = $3::date
		  AND dl.slot = $2
		  AND dl.status = 'UNATTEMPTED'
	`, driverID, slot, date)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
