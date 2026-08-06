package config

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SystemConfig struct {
	MorningCutoffTime string `json:"morningCutoffTime"`
	EveningCutoffTime string `json:"eveningCutoffTime"`
}

type ConfigResource struct{ DB *pgxpool.Pool }

func (cr ConfigResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", cr.Get)
	r.Put("/", cr.Update)
	return r
}

func (cr ConfigResource) ensureRow(ctx interface{ Deadline() (interface{}, bool) }) error {
	return nil
}

func (cr ConfigResource) Get(w http.ResponseWriter, r *http.Request) {
	_, _ = cr.DB.Exec(r.Context(), `INSERT INTO system_config (morning_cutoff_time) SELECT '03:00' WHERE NOT EXISTS (SELECT 1 FROM system_config)`)

	var cfg SystemConfig
	err := cr.DB.QueryRow(r.Context(), `SELECT morning_cutoff_time, evening_cutoff_time FROM system_config LIMIT 1`).Scan(&cfg.MorningCutoffTime, &cfg.EveningCutoffTime)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

type UpdateRequest struct {
	MorningCutoffTime *string `json:"morningCutoffTime"`
	EveningCutoffTime *string `json:"eveningCutoffTime"`
}

func (cr ConfigResource) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	_, _ = cr.DB.Exec(r.Context(), `INSERT INTO system_config (morning_cutoff_time) SELECT '03:00' WHERE NOT EXISTS (SELECT 1 FROM system_config)`)

	if req.MorningCutoffTime != nil {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET morning_cutoff_time = $1, updated_at = NOW()`, *req.MorningCutoffTime)
	}
	if req.EveningCutoffTime != nil {
		cr.DB.Exec(r.Context(), `UPDATE system_config SET evening_cutoff_time = $1, updated_at = NOW()`, *req.EveningCutoffTime)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Configuration updated"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
