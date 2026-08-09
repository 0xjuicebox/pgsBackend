package route

// Deferred pricing.
//
// Rule: on an established route, a price change takes effect from the 1st of
// the following month. Nothing charged mid-month ever changes.
//
// The one exception is a brand-new route whose prices are all still zero —
// that isn't a "change", it's the initial setup, and making a new route wait
// up to a month before it can charge anything would be absurd.
//
// Mechanics: the proposed prices sit in routes.pending_price_* with
// pending_effective_from set to the 1st of next month. A sweeper promotes
// them into price_* once that date arrives. Because the sweeper matches
// `pending_effective_from <= CURRENT_DATE` rather than `=`, a server that was
// down on the 1st catches up the moment it comes back.
//
// LogDelivery keeps reading price_* and is untouched by any of this.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingPriceInfo is surfaced on the route detail screen so an admin can see
// what's queued and when it lands.
type PendingPriceInfo struct {
	Prices        map[string]float64 `json:"prices"`
	EffectiveFrom string             `json:"effectiveFrom"`
}

// firstOfNextMonth returns the date a change queued today should land on,
// in IST — the business's calendar, not the server's.
func firstOfNextMonth() time.Time {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
}

// SetPrices replaces UpdatePrices. It decides on its own whether the change
// applies now or is queued, and reports which it did so the UI can say so.
//
// Wire in Routes(): r.Put("/prices", rr.SetPrices)
func (rr RouteResource) SetPrices(w http.ResponseWriter, r *http.Request) {
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
	for p, val := range payload.Prices {
		if val < 0 {
			http.Error(w, "Price for "+p+" cannot be negative", http.StatusBadRequest)
			return
		}
	}

	// Is this route still unpriced? Summing is enough — any non-zero price
	// means the route has been live and its customers have been quoted.
	var priceSum float64
	sumCols := make([]string, 0, len(Products))
	for _, p := range Products {
		sumCols = append(sumCols, "COALESCE(price_"+p+", 0)")
	}
	err := rr.DB.QueryRow(r.Context(),
		"SELECT "+strings.Join(sumCols, " + ")+" FROM routes WHERE id = $1", id,
	).Scan(&priceSum)
	if err != nil {
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}

	immediate := priceSum == 0

	sets := []string{}
	args := []any{}
	prefix := "pending_price_"
	if immediate {
		prefix = "price_"
	}
	for _, p := range Products {
		val, ok := payload.Prices[p]
		if !ok {
			continue
		}
		args = append(args, val)
		sets = append(sets, prefix+p+" = $"+itoa(len(args)))
	}
	if len(sets) == 0 {
		http.Error(w, "No recognised products in payload", http.StatusBadRequest)
		return
	}

	var effective string
	if immediate {
		// Setting live prices also clears anything queued — otherwise a
		// stale pending row from before the route went live would fire
		// later and silently overwrite what was just set.
		sets = append(sets, "pending_effective_from = NULL")
		for _, p := range Products {
			sets = append(sets, "pending_price_"+p+" = NULL")
		}
	} else {
		eff := firstOfNextMonth()
		args = append(args, eff)
		sets = append(sets, "pending_effective_from = $"+itoa(len(args)))
		effective = eff.Format("2006-01-02")
	}

	args = append(args, id)
	query := "UPDATE routes SET " + strings.Join(sets, ", ") + " WHERE id = $" + itoa(len(args))
	if _, err := rr.DB.Exec(r.Context(), query, args...); err != nil {
		http.Error(w, "Failed to save prices: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if immediate {
		json.NewEncoder(w).Encode(map[string]any{
			"applied": "immediately",
			"message": "Prices set. This route had no prices yet, so they're active right away.",
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"applied":       "scheduled",
		"effectiveFrom": effective,
		"message":       "Prices queued. They'll take effect on " + effective + ". This month's deliveries are unaffected.",
	})
}

// CancelPendingPrices discards a queued change before it lands.
//
// Wire in Routes(): r.Delete("/prices/pending", rr.CancelPendingPrices)
func (rr RouteResource) CancelPendingPrices(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	sets := []string{"pending_effective_from = NULL"}
	for _, p := range Products {
		sets = append(sets, "pending_price_"+p+" = NULL")
	}
	tag, err := rr.DB.Exec(r.Context(),
		"UPDATE routes SET "+strings.Join(sets, ", ")+" WHERE id = $1 AND pending_effective_from IS NOT NULL", id)
	if err != nil {
		http.Error(w, "Failed to cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "Nothing queued for this route", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Queued price change cancelled"})
}

// GetPendingPrices returns what's queued, or 204 if nothing is.
//
// Wire in Routes(): r.Get("/prices/pending", rr.GetPendingPrices)
func (rr RouteResource) GetPendingPrices(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	cols := make([]string, 0, len(Products))
	for _, p := range Products {
		cols = append(cols, "pending_price_"+p)
	}

	dest := make([]*float64, len(Products))
	scan := make([]any, 0, len(Products)+1)
	var eff *time.Time
	scan = append(scan, &eff)
	for i := range dest {
		scan = append(scan, &dest[i])
	}

	err := rr.DB.QueryRow(r.Context(),
		"SELECT pending_effective_from, "+strings.Join(cols, ", ")+" FROM routes WHERE id = $1", id,
	).Scan(scan...)
	if err != nil {
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}
	if eff == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	info := PendingPriceInfo{
		Prices:        map[string]float64{},
		EffectiveFrom: eff.Format("2006-01-02"),
	}
	for i, p := range Products {
		if dest[i] != nil {
			info.Prices[p] = *dest[i]
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// -------------------------------------------------------------------------
// Promotion sweeper
// -------------------------------------------------------------------------

// StartPricePromotionSweeper promotes queued prices once their effective date
// arrives. Runs hourly, and once immediately at boot.
//
// Hourly rather than daily because a daily timer anchored to process start
// would drift — restart the server at 23:00 and the "daily" tick lands at
// 23:00, delaying a 1st-of-month promotion by nearly a full day. Hourly caps
// the worst case at 60 minutes for a change that only matters at day
// granularity.
//
// Start from main.go, next to the shift sweeper:
//
//	go route.StartPricePromotionSweeper(pool)
func StartPricePromotionSweeper(db *pgxpool.Pool) {
	promotePendingPrices(db)

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		promotePendingPrices(db)
	}
}

func promotePendingPrices(db *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// COALESCE so a partial change (admin only queued a new milk price)
	// leaves the other products alone instead of nulling them.
	sets := make([]string, 0, len(Products)*2+1)
	for _, p := range Products {
		sets = append(sets, fmt.Sprintf("price_%s = COALESCE(pending_price_%s, price_%s)", p, p, p))
	}
	for _, p := range Products {
		sets = append(sets, "pending_price_"+p+" = NULL")
	}
	sets = append(sets, "pending_effective_from = NULL")

	// `<=` not `=`: catches up if the server was down on the 1st.
	q := "UPDATE routes SET " + strings.Join(sets, ", ") +
		" WHERE pending_effective_from IS NOT NULL AND pending_effective_from <= CURRENT_DATE"

	tag, err := db.Exec(ctx, q)
	if err != nil {
		fmt.Printf("⚠️ promotePendingPrices: %v\n", err)
		return
	}
	if tag.RowsAffected() > 0 {
		fmt.Printf("💰 promoted queued prices on %d route(s)\n", tag.RowsAffected())
	}
}
