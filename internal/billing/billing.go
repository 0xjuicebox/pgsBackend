package billing

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type GenerateRequest struct {
	Month string `json:"month"` // Format: "YYYY-MM" (e.g., "2026-07")
}

// InvoiceBreakdown represents the frozen JSON state of the bill
type InvoiceBreakdown struct {
	Quantities map[string]int     `json:"quantities"`
	Prices     map[string]float64 `json:"prices"`
}

// LiveTally represents what the Admin sees mid-month
type LiveTally struct {
	CustomerId  uuid.UUID        `json:"customerId"`
	TotalAmount float64          `json:"totalAmount"`
	Breakdown   InvoiceBreakdown `json:"breakdown"`
}

type BillingResource struct {
	DB *pgxpool.Pool
}

func (br BillingResource) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/live", br.GetLiveTallies) // Admin Dashboard Preview
	r.Post("/generate", br.Generate)  // The Big End-of-Month Button

	return r
}

// getAggregatedData is a helper that runs the massive aggregation query
// used by BOTH the Live Tally and the Generator endpoints.
func (br BillingResource) getAggregatedData(r *http.Request, month string) ([]LiveTally, error) {
	// We CROSS JOIN with the system_config table (Limit 1) to get the live prices,
	// and aggregate all DELIVERED quantities for the requested month.
	query := `
		SELECT
			d.customer_id,
			SUM(d.delivered_milk_qty), SUM(d.delivered_curd_qty), SUM(d.delivered_butter_qty),
			SUM(d.delivered_ghee_qty), SUM(d.delivered_lassi_qty), SUM(d.delivered_paneer_qty),
			SUM(d.delivered_jaggery_qty), SUM(d.delivered_khand_qty), SUM(d.delivered_oil_qty),
			SUM(d.delivered_atta_qty), SUM(d.delivered_burfi_qty),
			c.milk_price, c.curd_price, c.butter_price, c.ghee_price, c.lassi_price,
			c.paneer_price, c.jaggery_price, c.khand_price, c.oil_price, c.atta_price, c.burfi_price
		FROM delivery_logs d
		CROSS JOIN (
			SELECT milk_price, curd_price, butter_price, ghee_price, lassi_price,
			       paneer_price, jaggery_price, khand_price, oil_price, atta_price, burfi_price
			FROM system_config LIMIT 1
		) c
		WHERE d.status = 'DELIVERED' AND TO_CHAR(d.delivery_date, 'YYYY-MM') = $1
		GROUP BY
			d.customer_id,
			c.milk_price, c.curd_price, c.butter_price, c.ghee_price, c.lassi_price,
			c.paneer_price, c.jaggery_price, c.khand_price, c.oil_price, c.atta_price, c.burfi_price
	`

	rows, err := br.DB.Query(r.Context(), query, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tallies []LiveTally

	for rows.Next() {
		var t LiveTally
		var mq, cq, bq, gq, lq, pq, jq, kq, oq, aq, burq int
		var mp, cp, bp, gp, lp, pp, jp, kp, op, ap, burp float64

		err := rows.Scan(
			&t.CustomerId,
			&mq, &cq, &bq, &gq, &lq, &pq, &jq, &kq, &oq, &aq, &burq,
			&mp, &cp, &bp, &gp, &lp, &pp, &jp, &kp, &op, &ap, &burp,
		)
		if err != nil {
			return nil, err
		}

		t.Breakdown = InvoiceBreakdown{
			Quantities: map[string]int{
				"milk": mq, "curd": cq, "butter": bq, "ghee": gq, "lassi": lq,
				"paneer": pq, "jaggery": jq, "khand": kq, "oil": oq, "atta": aq, "burfi": burq,
			},
			Prices: map[string]float64{
				"milk": mp, "curd": cp, "butter": bp, "ghee": gp, "lassi": lp,
				"paneer": pp, "jaggery": jp, "khand": kp, "oil": op, "atta": ap, "burfi": burp,
			},
		}

		// Calculate total amount dynamically
		t.TotalAmount = (float64(mq) * mp) + (float64(cq) * cp) + (float64(bq) * bp) +
			(float64(gq) * gp) + (float64(lq) * lp) + (float64(pq) * pp) +
			(float64(jq) * jp) + (float64(kq) * kp) + (float64(oq) * op) +
			(float64(aq) * ap) + (float64(burq) * burp)

		tallies = append(tallies, t)
	}

	return tallies, nil
}

// GetLiveTallies powers the Admin Dashboard preview
func (br BillingResource) GetLiveTallies(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		http.Error(w, "Missing 'month' query parameter (e.g., ?month=2026-07)", http.StatusBadRequest)
		return
	}

	tallies, err := br.getAggregatedData(r, month)
	if err != nil {
		http.Error(w, "Failed to calculate live tallies: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tallies)
}

// Generate locks in the final invoices at the end of the month
func (br BillingResource) Generate(w http.ResponseWriter, r *http.Request) {
	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	// 1. Calculate the final math
	tallies, err := br.getAggregatedData(r, req.Month)
	if err != nil {
		http.Error(w, "Failed to calculate invoices: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. Start a transaction to save all invoices safely
	tx, err := br.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction start failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	// Insert query using ON CONFLICT DO NOTHING.
	// This makes the button completely safe to double-click! It won't duplicate invoices.
	insertQuery := `
		INSERT INTO invoices (id, customer_id, billing_month, total_amount, breakdown)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (customer_id, billing_month) DO NOTHING
	`

	invoicesGenerated := 0

	for _, tally := range tallies {
		// Only bill customers who actually owe money > 0
		if tally.TotalAmount <= 0 {
			continue
		}

		invId, _ := uuid.NewV7()
		breakdownJSON, _ := json.Marshal(tally.Breakdown)

		tag, err := tx.Exec(r.Context(), insertQuery, invId, tally.CustomerId, req.Month, tally.TotalAmount, breakdownJSON)
		if err != nil {
			http.Error(w, "Failed to save invoice for customer "+tally.CustomerId.String(), http.StatusInternalServerError)
			return
		}

		// tag.RowsAffected() tells us if it actually inserted or if it was blocked by the duplicate rule
		if tag.RowsAffected() > 0 {
			invoicesGenerated++
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Transaction commit failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Billing cycle processed successfully",
		"month":             req.Month,
		"invoicesGenerated": invoicesGenerated,
	})
}
