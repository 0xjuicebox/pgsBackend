package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var Products = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

type InvoiceBreakdown struct {
	Quantities map[string]int     `json:"quantities"`
	Prices     map[string]float64 `json:"prices"`
	LineTotals map[string]float64 `json:"lineTotals"`
}

type CustomerTally struct {
	CustomerId    uuid.UUID        `json:"customerId"`
	CustomerName  string           `json:"customerName"`
	PhoneNumber   string           `json:"phoneNumber"`
	DeliveryCount int              `json:"deliveryCount"`
	TotalAmount   float64          `json:"totalAmount"`
	Breakdown     InvoiceBreakdown `json:"breakdown"`
	InvoiceId     *uuid.UUID       `json:"invoiceId"`
	InvoiceStatus *string          `json:"invoiceStatus"`
	IsFinalized   bool             `json:"isFinalized"`
}

type MonthSummary struct {
	Month           string          `json:"month"`
	CustomerCount   int             `json:"customerCount"`
	TotalRevenue    float64         `json:"totalRevenue"`
	FinalizedCount  int             `json:"finalizedCount"`
	PendingAmount   float64         `json:"pendingAmount"`
	CollectedAmount float64         `json:"collectedAmount"`
	Tallies         []CustomerTally `json:"tallies"`
}

type GenerateRequest struct {
	Month string `json:"month"`
}
type MarkPaidRequest struct {
	Status string `json:"status"`
}

// BillingResource now carries a WhatsApp handle so Generate can notify each
// customer whose invoice was just created. main.go needs to construct it
// with both fields — see the note at the bottom of this file.
type BillingResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
}

func (br BillingResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/live", br.GetLiveTallies)
	r.Get("/customer/{id}", br.GetCustomerBill)
	r.Post("/generate", br.Generate)
	r.With(middleware.Paginate).Get("/invoices", br.ListInvoices)
	r.Put("/invoices/{id}/status", br.MarkPaid)
	r.Post("/final/{customerId}", br.FinalInvoice)
	r.Post("/invoices/{id}/regenerate", br.RegenerateInvoice)
	return r
}

func (br BillingResource) aggregate(ctx context.Context, month string) ([]CustomerTally, error) {
	sums := make([]string, 0, len(Products)*2)
	for _, p := range Products {
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(d.delivered_%s_qty), 0)", p))
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(d.delivered_%s_qty * d.unit_price_%s), 0)", p, p))
	}

	query := fmt.Sprintf(`
		SELECT c.id, c.name, c.phone_number, COUNT(d.id), %s
		FROM customers c
		JOIN delivery_logs d ON d.customer_id = c.id
		WHERE d.status = 'DELIVERED' AND TO_CHAR(d.delivery_date, 'YYYY-MM') = $1
		GROUP BY c.id, c.name, c.phone_number
		ORDER BY c.name ASC
	`, strings.Join(sums, ", "))

	rows, err := br.DB.Query(ctx, query, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	n := len(Products)
	tallies := []CustomerTally{}
	for rows.Next() {
		var t CustomerTally
		qty := make([]int, n)
		revenue := make([]float64, n)
		dest := []any{&t.CustomerId, &t.CustomerName, &t.PhoneNumber, &t.DeliveryCount}
		for i := 0; i < n; i++ {
			dest = append(dest, &qty[i], &revenue[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		t.Breakdown = InvoiceBreakdown{
			Quantities: map[string]int{}, Prices: map[string]float64{}, LineTotals: map[string]float64{},
		}
		for i, p := range Products {
			t.Breakdown.Quantities[p] = qty[i]
			if qty[i] > 0 {
				t.Breakdown.Prices[p] = revenue[i] / float64(qty[i])
				t.Breakdown.LineTotals[p] = revenue[i]
			}
			t.TotalAmount += revenue[i]
		}
		tallies = append(tallies, t)
	}
	return tallies, rows.Err()
}

func (br BillingResource) mergeInvoices(ctx context.Context, month string, tallies []CustomerTally) error {
	rows, err := br.DB.Query(ctx, `SELECT customer_id, id, status, total_amount, breakdown FROM invoices WHERE billing_month = $1`, month)
	if err != nil {
		return err
	}
	defer rows.Close()
	type frozen struct {
		id     uuid.UUID
		status string
		amount float64
		bd     InvoiceBreakdown
	}
	byCustomer := map[uuid.UUID]frozen{}
	for rows.Next() {
		var custID uuid.UUID
		var f frozen
		var raw []byte
		if err := rows.Scan(&custID, &f.id, &f.status, &f.amount, &raw); err != nil {
			return err
		}
		_ = json.Unmarshal(raw, &f.bd)
		byCustomer[custID] = f
	}
	for i := range tallies {
		if f, ok := byCustomer[tallies[i].CustomerId]; ok {
			tallies[i].InvoiceId = &f.id
			s := f.status
			tallies[i].InvoiceStatus = &s
			tallies[i].IsFinalized = true
			tallies[i].TotalAmount = f.amount
			tallies[i].Breakdown = f.bd
		}
	}
	return rows.Err()
}

func (br BillingResource) GetLiveTallies(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	if !validMonth(month) {
		http.Error(w, "month must be YYYY-MM", http.StatusBadRequest)
		return
	}
	tallies, err := br.aggregate(r.Context(), month)
	if err != nil {
		http.Error(w, "Calculation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := br.mergeInvoices(r.Context(), month, tallies); err != nil {
		http.Error(w, "Invoice load error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	summary := MonthSummary{Month: month, Tallies: tallies, CustomerCount: len(tallies)}
	for _, t := range tallies {
		summary.TotalRevenue += t.TotalAmount
		if t.IsFinalized {
			summary.FinalizedCount++
			if t.InvoiceStatus != nil && strings.HasPrefix(*t.InvoiceStatus, "PAID") {
				summary.CollectedAmount += t.TotalAmount
			} else {
				summary.PendingAmount += t.TotalAmount
			}
		}
	}
	writeJSON(w, http.StatusOK, summary)
}

type BillDay struct {
	Date       string         `json:"date"`
	Slot       string         `json:"slot"`
	Status     string         `json:"status"`
	Quantities map[string]int `json:"quantities"`
	DayTotal   float64        `json:"dayTotal"`
	IsFlagged  bool           `json:"isFlagged"`
	LogId      uuid.UUID      `json:"logId"`
}

func (br BillingResource) GetCustomerBill(w http.ResponseWriter, r *http.Request) {
	customerID := chi.URLParam(r, "id")
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	qtyCols := make([]string, 0, len(Products))
	priceCols := make([]string, 0, len(Products))
	for _, p := range Products {
		qtyCols = append(qtyCols, "d.delivered_"+p+"_qty")
		priceCols = append(priceCols, "d.unit_price_"+p)
	}
	query := fmt.Sprintf(`
		SELECT d.id, TO_CHAR(d.delivery_date, 'YYYY-MM-DD'), d.slot, d.status, COALESCE(d.is_flagged, false), %s, %s
		FROM delivery_logs d
		WHERE d.customer_id = $1 AND TO_CHAR(d.delivery_date, 'YYYY-MM') = $2
		ORDER BY d.delivery_date ASC, d.slot ASC
	`, strings.Join(qtyCols, ", "), strings.Join(priceCols, ", "))
	rows, err := br.DB.Query(r.Context(), query, customerID, month)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	n := len(Products)
	days := []BillDay{}
	for rows.Next() {
		var d BillDay
		qty := make([]int, n)
		price := make([]float64, n)
		dest := []any{&d.LogId, &d.Date, &d.Slot, &d.Status, &d.IsFlagged}
		for i := range qty {
			dest = append(dest, &qty[i])
		}
		for i := range price {
			dest = append(dest, &price[i])
		}
		if err := rows.Scan(dest...); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		d.Quantities = map[string]int{}
		for i, p := range Products {
			if qty[i] > 0 {
				d.Quantities[p] = qty[i]
				if d.Status == "DELIVERED" {
					d.DayTotal += float64(qty[i]) * price[i]
				}
			}
		}
		days = append(days, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"customerId": customerID, "month": month, "days": days})
}

// Generate creates invoices for a completed month and — new in this version —
// fires off a WhatsApp bill to each customer whose invoice was just created.
// Pre-existing invoices are not re-messaged (see notifyFreshInvoices below).
func (br BillingResource) Generate(w http.ResponseWriter, r *http.Request) {
	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if !validMonth(req.Month) {
		http.Error(w, "month must be YYYY-MM", http.StatusBadRequest)
		return
	}
	if req.Month >= time.Now().Format("2006-01") {
		http.Error(w, "Cannot generate for current or future month", http.StatusBadRequest)
		return
	}
	tallies, err := br.aggregate(r.Context(), req.Month)
	if err != nil {
		http.Error(w, "Calculation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tx, err := br.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())
	generated, skipped := 0, 0
	for _, t := range tallies {
		if t.TotalAmount <= 0 {
			continue
		}
		invID, _ := uuid.NewV7()
		breakdown, _ := json.Marshal(t.Breakdown)
		tag, err := tx.Exec(r.Context(), `INSERT INTO invoices (id, customer_id, billing_month, total_amount, breakdown) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (customer_id, billing_month) DO NOTHING`, invID, t.CustomerId, req.Month, t.TotalAmount, breakdown)
		if err != nil {
			http.Error(w, "Invoice save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() > 0 {
			generated++
		} else {
			skipped++
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit failed", http.StatusInternalServerError)
		return
	}

	// Fire-and-forget notifications for the invoices we just inserted. Serial
	// Twilio calls at ~500ms each × N customers can take a long time, so we
	// don't want to hold the admin's HTTP request open while it runs. Any
	// failures get logged; admin can re-notify a specific customer manually
	// via the billing drill-down.
	if br.WhatsApp != nil && generated > 0 {
		go br.notifyFreshInvoices(req.Month, tallies)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"message": "Billing cycle processed", "month": req.Month, "invoicesGenerated": generated, "alreadyExisted": skipped})
}

// notifyFreshInvoices sends the month-end bill to customers whose invoice
// was created within the last minute — i.e. by the Generate call that just
// finished. Filtering by created_at avoids re-messaging customers whose
// invoices already existed from a previous Generate run.
func (br BillingResource) notifyFreshInvoices(month string, tallies []CustomerTally) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rows, err := br.DB.Query(ctx, `
		SELECT customer_id FROM invoices
		WHERE billing_month = $1
		  AND created_at > NOW() - INTERVAL '1 minute'
	`, month)
	if err != nil {
		fmt.Printf("⚠️ notifyFreshInvoices: %v\n", err)
		return
	}
	defer rows.Close()

	fresh := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		rows.Scan(&id)
		fresh[id] = true
	}

	for _, t := range tallies {
		if t.TotalAmount <= 0 || !fresh[t.CustomerId] {
			continue
		}
		br.notifyBill(t, month)
	}
}

// notifyBill sends the month-end bill total to a single customer over
// WhatsApp. Uses a free-form text message via SendDeliveryUpdate rather than
// a Twilio Content Template — templates would need Meta approval for every
// tweak to the wording, and this message body changes with the product mix
// each month. Once the format is stable we can switch to a template.
//
// UPI reconciliation is deliberately out of scope for v1. The message ends
// with a manual "Reply PAID once transferred" nudge; auto-reconciliation
// via a payment gateway comes in v1.1.
func (br BillingResource) notifyBill(t CustomerTally, month string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var phone string
	if err := br.DB.QueryRow(ctx, `SELECT phone_number FROM customers WHERE id = $1`, t.CustomerId).Scan(&phone); err != nil {
		fmt.Printf("⚠️ notifyBill: couldn't find phone for %s: %v\n", t.CustomerId, err)
		return
	}

	// Pretty product breakdown, only for lines that actually contributed.
	// Same LABEL and unit conventions used by the customer-facing HTML pages.
	labels := map[string]string{
		"milk": "Milk", "curd": "Curd", "butter": "Butter", "ghee": "Ghee",
		"lassi": "Buttermilk", "paneer": "Paneer", "jaggery": "Jaggery",
		"khand": "Desi Khand", "oil": "Mustard Oil", "atta": "Atta", "burfi": "Burfi",
	}
	litre := map[string]bool{"milk": true, "curd": true, "lassi": true, "oil": true}

	var lines []string
	for _, p := range Products {
		total := t.Breakdown.LineTotals[p]
		qty := t.Breakdown.Quantities[p]
		if total <= 0 {
			continue
		}
		unit := "g"
		display := float64(qty)
		if qty >= 1000 {
			display = float64(qty) / 1000
			if litre[p] {
				unit = "L"
			} else {
				unit = "kg"
			}
		} else if litre[p] {
			unit = "ml"
		}
		lines = append(lines, fmt.Sprintf("• %s %g%s — ₹%.0f", labels[p], display, unit, total))
	}

	msg := fmt.Sprintf(
		"🧾 *Your PGS Direct bill — %s*\n\n"+
			"Hello %s,\n\n"+
			"%s\n\n"+
			"*Total: ₹%.0f*\n\n"+
			"To pay, please transfer to our UPI ID and reply PAID once done. "+
			"Our team will confirm and mark your bill as settled.",
		labelForMonth(month), t.CustomerName, strings.Join(lines, "\n"), t.TotalAmount,
	)

	if err := br.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
		fmt.Printf("⚠️ notifyBill: WhatsApp send failed for %s: %v\n", phone, err)
	}
}

// labelForMonth turns "2026-07" into "July 2026" for human-friendly headers.
func labelForMonth(m string) string {
	if t, err := time.Parse("2006-01", m); err == nil {
		return t.Format("January 2006")
	}
	return m
}

type InvoiceRow struct {
	Id           uuid.UUID `json:"id"`
	CustomerId   uuid.UUID `json:"customerId"`
	CustomerName string    `json:"customerName"`
	PhoneNumber  string    `json:"phoneNumber"`
	BillingMonth string    `json:"billingMonth"`
	TotalAmount  float64   `json:"totalAmount"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (br BillingResource) ListInvoices(w http.ResponseWriter, r *http.Request) {
	page, _ := r.Context().Value(middleware.PageKey).(int)
	limit, _ := r.Context().Value(middleware.LimitKey).(int)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	conds := []string{"1=1"}
	args := []any{}
	add := func(clause string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}
	if m := r.URL.Query().Get("month"); m != "" {
		add("i.billing_month = $%d", m)
	}
	if s := r.URL.Query().Get("status"); s != "" {
		add("i.status = $%d", s)
	}
	if c := r.URL.Query().Get("customerId"); c != "" {
		add("i.customer_id = $%d", c)
	}
	args = append(args, limit, (page-1)*limit)
	query := fmt.Sprintf(`SELECT i.id, i.customer_id, c.name, c.phone_number, i.billing_month, i.total_amount, i.status, i.created_at FROM invoices i JOIN customers c ON c.id = i.customer_id WHERE %s ORDER BY i.billing_month DESC, c.name ASC LIMIT $%d OFFSET $%d`, strings.Join(conds, " AND "), len(args)-1, len(args))
	rows, err := br.DB.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	invoices := []InvoiceRow{}
	for rows.Next() {
		var inv InvoiceRow
		if err := rows.Scan(&inv.Id, &inv.CustomerId, &inv.CustomerName, &inv.PhoneNumber, &inv.BillingMonth, &inv.TotalAmount, &inv.Status, &inv.CreatedAt); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		invoices = append(invoices, inv)
	}
	writeJSON(w, http.StatusOK, invoices)
}

var allowedInvoiceStatus = map[string]bool{"PENDING": true, "PAID_ONLINE": true, "PAID_CASH": true}

func (br BillingResource) MarkPaid(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req MarkPaidRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if !allowedInvoiceStatus[req.Status] {
		http.Error(w, "status must be PENDING, PAID_ONLINE or PAID_CASH", http.StatusBadRequest)
		return
	}
	var updated uuid.UUID
	err := br.DB.QueryRow(r.Context(), `UPDATE invoices SET status = $1, updated_at = NOW() WHERE id = $2 RETURNING id`, req.Status, id).Scan(&updated)
	if err == pgx.ErrNoRows {
		http.Error(w, "Invoice not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Invoice status updated", "status": req.Status})
}

func validMonth(m string) bool { _, err := time.Parse("2006-01", m); return err == nil }
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// -------------------------------------------------------------------------
// main.go change required:
//
// Where you currently have:
//     br := billing.BillingResource{DB: pool}
//
// Change to:
//     br := billing.BillingResource{DB: pool, WhatsApp: whatsappService}
//
// Use whatever variable name your Twilio/WhatsApp service is bound to in
// main.go — it's the same one you already pass to DeliveryResource.
// -------------------------------------------------------------------------
