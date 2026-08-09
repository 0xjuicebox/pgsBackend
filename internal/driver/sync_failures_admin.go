package driver

// Admin-side of the sync-failures bucket.
//
// The mobile client posts failures to POST /driver/sync-failures (see
// shifts.go). Admin needs to read them back and mark them resolved once the
// underlying delivery has been manually re-created via the log-correction
// screen.
//
// These endpoints live in the `driver` package because the table
// (delivery_sync_failures) was created here — keeping all its
// reads/writes in one package makes the schema owner obvious.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
)

// -------------------------------------------------------------------------
// Types
// -------------------------------------------------------------------------

type SyncFailure struct {
	Id           uuid.UUID              `json:"id"`
	DriverId     *uuid.UUID             `json:"driverId"`
	DriverName   *string                `json:"driverName"`
	Payload      map[string]interface{} `json:"payload"`
	ErrorMessage string                 `json:"errorMessage"`
	Reason       string                 `json:"reason"`
	ReportedAt   time.Time              `json:"reportedAt"`
	ResolvedAt   *time.Time             `json:"resolvedAt"`
}

// -------------------------------------------------------------------------
// Routes — wired from driver.go
//
// In driver.go's Routes() method, add these OUTSIDE the mobile group (they
// don't want SupabaseAuth — admin app doesn't hold a Supabase JWT):
//
//     // Admin: sync-failure review
//     r.Get("/sync-failures", dr.ListSyncFailures)
//     r.Post("/sync-failures/{id}/resolve", dr.ResolveSyncFailure)
// -------------------------------------------------------------------------

// ListSyncFailures returns unresolved failures by default, most recent first.
// ?includeResolved=true returns everything.
func (dr DriverResource) ListSyncFailures(w http.ResponseWriter, r *http.Request) {
	includeResolved := r.URL.Query().Get("includeResolved") == "true"

	where := "WHERE sf.resolved_at IS NULL"
	if includeResolved {
		where = ""
	}

	q := `
		SELECT sf.id, sf.driver_id, d.name, sf.payload, sf.error_message,
		       sf.reason, sf.reported_at, sf.resolved_at
		FROM delivery_sync_failures sf
		LEFT JOIN drivers d ON d.id = sf.driver_id
		` + where + `
		ORDER BY sf.reported_at DESC
		LIMIT 200
	`
	rows, err := dr.DB.Query(r.Context(), q)
	if err != nil {
		http.Error(w, "Failed to load: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []SyncFailure{}
	for rows.Next() {
		var sf SyncFailure
		var payloadRaw []byte
		if err := rows.Scan(
			&sf.Id, &sf.DriverId, &sf.DriverName, &payloadRaw,
			&sf.ErrorMessage, &sf.Reason, &sf.ReportedAt, &sf.ResolvedAt,
		); err != nil {
			continue
		}
		// Unmarshal the JSONB payload so the frontend can render it as a
		// structured form instead of a raw JSON blob. If parsing fails
		// (shouldn't — we wrote it ourselves via json.Marshal), fall back
		// to an empty map rather than dropping the whole row.
		if err := json.Unmarshal(payloadRaw, &sf.Payload); err != nil {
			sf.Payload = map[string]interface{}{}
		}
		out = append(out, sf)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ResolveSyncFailure marks a failure row as handled. Does not touch
// delivery_logs — the admin is expected to have already fixed (or
// deliberately skipped) the underlying delivery via the log-correction
// screen. This endpoint is just the "I've dealt with this, hide it" button.
func (dr DriverResource) ResolveSyncFailure(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var updated uuid.UUID
	err := dr.DB.QueryRow(r.Context(), `
		UPDATE delivery_sync_failures
		SET resolved_at = NOW()
		WHERE id = $1 AND resolved_at IS NULL
		RETURNING id
	`, id).Scan(&updated)
	if err == pgx.ErrNoRows {
		http.Error(w, "Failure not found or already resolved", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Resolved"})
}
