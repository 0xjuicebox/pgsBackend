package config

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemConfig holds all admin-editable global settings. Every field is a
// wall-clock time interpreted as IST.
//
// Four distinct times, two concerns:
//
//   - ORDER CUTOFFS (morning/evening) are the customer's deadline to change
//     TODAY's order for that slot. Enforced in override.Submit.
//   - SHIFT END TIMES (morning/evening) are when an ACTIVE driver shift stops
//     counting as a run in progress. Enforced by driver.sweepAutoEnd.
//
// These were one setting until the shift-end split migration, which meant an
// evening driver starting at 16:00 had their shift auto-ended at 15:00's
// sweep — dashboard showed zero runs in progress while vans were out.
//
// NULL semantics are per-field: complaint_cutoff_time NULL disables the
// complaint window entirely, while the four cutoffs are effectively required.
type SystemConfig struct {
	MorningCutoffTime   string  `json:"morningCutoffTime"`
	EveningCutoffTime   string  `json:"eveningCutoffTime"`
	MorningShiftEndTime string  `json:"morningShiftEndTime"`
	EveningShiftEndTime string  `json:"eveningShiftEndTime"`
	ComplaintCutoffTime *string `json:"complaintCutoffTime"`
}

type ConfigResource struct{ DB *pgxpool.Pool }

func (cr ConfigResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", cr.Get)
	r.Put("/", cr.Update)
	return r
}

// seedIfEmpty guarantees the singleton row exists. The migration seeds it
// too, but this covers a database restored from a backup taken before that
// migration, and keeps Get/Update independent of migration ordering.
func (cr ConfigResource) seedIfEmpty(r *http.Request) {
	_, _ = cr.DB.Exec(r.Context(),
		`INSERT INTO system_config (morning_cutoff_time) SELECT '03:00' WHERE NOT EXISTS (SELECT 1 FROM system_config)`)
}

func (cr ConfigResource) Get(w http.ResponseWriter, r *http.Request) {
	cr.seedIfEmpty(r)

	var cfg SystemConfig
	err := cr.DB.QueryRow(r.Context(), `
		SELECT
			morning_cutoff_time::text,
			evening_cutoff_time::text,
			morning_shift_end_time::text,
			evening_shift_end_time::text,
			complaint_cutoff_time::text
		FROM system_config LIMIT 1
	`).Scan(
		&cfg.MorningCutoffTime, &cfg.EveningCutoffTime,
		&cfg.MorningShiftEndTime, &cfg.EveningShiftEndTime,
		&cfg.ComplaintCutoffTime,
	)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// UpdateRequest uses pointers so callers can PATCH-style send only the fields
// they want to change. Explicitly sending complaintCutoffTime: null clears
// the window (allowed by the schema; means "no cutoff, forever").
type UpdateRequest struct {
	MorningCutoffTime   *string `json:"morningCutoffTime"`
	EveningCutoffTime   *string `json:"eveningCutoffTime"`
	MorningShiftEndTime *string `json:"morningShiftEndTime"`
	EveningShiftEndTime *string `json:"eveningShiftEndTime"`
	ComplaintCutoffTime *string `json:"complaintCutoffTime"`
	// ClearComplaintCutoff, when true, deletes the complaint window (sets
	// column to NULL). This is separate from ComplaintCutoffTime = nil in
	// JSON, which cannot be distinguished from "field omitted". Frontend
	// sends { clearComplaintCutoff: true } when the toggle is off.
	ClearComplaintCutoff bool `json:"clearComplaintCutoff"`
}

func (cr ConfigResource) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	cr.seedIfEmpty(r)

	// One statement per supplied field, in a transaction.
	//
	// Previously each UPDATE ran standalone with its error discarded, so a
	// value rejected by the database — a malformed time, or now a CHECK
	// violation from setting an order cutoff after its shift end — was
	// silently dropped while the response still said "Configuration
	// updated". The admin would set a cutoff, see success, and find the old
	// value still in place on the next load.
	tx, err := cr.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	type field struct {
		column string
		value  *string
	}
	fields := []field{
		{"morning_cutoff_time", req.MorningCutoffTime},
		{"evening_cutoff_time", req.EveningCutoffTime},
		{"morning_shift_end_time", req.MorningShiftEndTime},
		{"evening_shift_end_time", req.EveningShiftEndTime},
	}

	for _, f := range fields {
		if f.value == nil {
			continue
		}
		// Column name is from this fixed list, never from the request body.
		q := `UPDATE system_config SET ` + f.column + ` = $1, updated_at = NOW()`
		if _, err := tx.Exec(r.Context(), q, *f.value); err != nil {
			http.Error(w, "Could not set "+f.column+": "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Complaint cutoff: clear takes precedence over set. This means a
	// frontend that accidentally sends both a value AND clear=true will
	// end up clearing — safer default than a partial update.
	if req.ClearComplaintCutoff {
		if _, err := tx.Exec(r.Context(),
			`UPDATE system_config SET complaint_cutoff_time = NULL, updated_at = NOW()`); err != nil {
			http.Error(w, "Could not clear complaint cutoff: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if req.ComplaintCutoffTime != nil {
		if _, err := tx.Exec(r.Context(),
			`UPDATE system_config SET complaint_cutoff_time = $1, updated_at = NOW()`,
			*req.ComplaintCutoffTime); err != nil {
			http.Error(w, "Could not set complaint cutoff: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Could not save configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Configuration updated"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
