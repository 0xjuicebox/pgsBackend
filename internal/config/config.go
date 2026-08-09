package config

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemConfig holds all admin-editable global settings. Every field is a
// nullable HH:MM:SS wall-clock time interpreted as IST. NULL semantics are
// per-field: complaint_cutoff_time NULL disables the complaint window
// entirely, while the two driver cutoffs are effectively required.
type SystemConfig struct {
	MorningCutoffTime   string  `json:"morningCutoffTime"`
	EveningCutoffTime   string  `json:"eveningCutoffTime"`
	ComplaintCutoffTime *string `json:"complaintCutoffTime"`
}

type ConfigResource struct{ DB *pgxpool.Pool }

func (cr ConfigResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", cr.Get)
	r.Put("/", cr.Update)
	return r
}

func (cr ConfigResource) Get(w http.ResponseWriter, r *http.Request) {
	// Seed a row if the table's empty. Only sets morning to a safe default;
	// evening and complaint will come from column defaults.
	_, _ = cr.DB.Exec(r.Context(),
		`INSERT INTO system_config (morning_cutoff_time) SELECT '03:00' WHERE NOT EXISTS (SELECT 1 FROM system_config)`)

	var cfg SystemConfig
	err := cr.DB.QueryRow(r.Context(), `
		SELECT
			morning_cutoff_time::text,
			evening_cutoff_time::text,
			complaint_cutoff_time::text
		FROM system_config LIMIT 1
	`).Scan(&cfg.MorningCutoffTime, &cfg.EveningCutoffTime, &cfg.ComplaintCutoffTime)
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

	_, _ = cr.DB.Exec(r.Context(),
		`INSERT INTO system_config (morning_cutoff_time) SELECT '03:00' WHERE NOT EXISTS (SELECT 1 FROM system_config)`)

	if req.MorningCutoffTime != nil {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET morning_cutoff_time = $1, updated_at = NOW()`, *req.MorningCutoffTime)
	}
	if req.EveningCutoffTime != nil {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET evening_cutoff_time = $1, updated_at = NOW()`, *req.EveningCutoffTime)
	}
	// Complaint cutoff: clear takes precedence over set. This means a
	// frontend that accidentally sends both a value AND clear=true will
	// end up clearing — safer default than a partial update.
	if req.ClearComplaintCutoff {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET complaint_cutoff_time = NULL, updated_at = NOW()`)
	} else if req.ComplaintCutoffTime != nil {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET complaint_cutoff_time = $1, updated_at = NOW()`, *req.ComplaintCutoffTime)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Configuration updated"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
