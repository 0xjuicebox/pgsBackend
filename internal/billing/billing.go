package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/payment"
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

// BillingResource carries a WhatsApp handle so Generate can notify each
// customer whose invoice was just created, and a Payment handle so those
// messages can carry a Razorpay link.
//
// Payment is optional. When it's nil or unconfigured, bills still go out with
// manual payment instructions — a missing gateway shouldn't stop billing.
type BillingResource struct {
	DB       *pgxpool.Pool
	WhatsApp *notification.WhatsAppService
	Payment  *payment.Service
	// Templates carries the approved Twilio content template SIDs. An empty
	// Bill SID makes notifyBill fall back to the free-form message, which
	// still works inside WhatsApp's 24-hour window.
	Templates notification.TemplateSIDs
}

func (br BillingResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/live", br.GetLiveTallies)
	r.Get("/customer/{id}", br.GetCustomerBill)
	r.Get("/customer/{id}/invoices", br.GetCustomerInvoices)
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
		// Divided by 1000: prices are per LITRE or per KILOGRAM, quantities are in
		// millilitres or grams.
		//
		// routes.price_milk = 105 means rupees 105 per litre — which is what an admin
		// types and what anyone reading the table would assume. delivery_logs stores
		// that same figure as a snapshot at delivery time.
		//
		// Without the division, 2 L of milk billed as 2000 x 105 = rupees 210,000.
		// Every invoice was out by a factor of a thousand.
		//
		// Storing a per-millilitre price instead would remove the division but round
		// rupees 105/L to 0.11 in NUMERIC(10,2) — a 4.7% overcharge baked into the
		// schema. Keeping the human-readable price and dividing at the point of use is
		// both exact and legible.
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(d.delivered_%s_qty * d.unit_price_%s) / 1000.0, 0)", p, p))
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
				// Stored per LITRE or KILOGRAM, matching routes.price_* and
				// what the customer is shown. revenue is already in rupees and
				// qty is in base units, so the ratio is per base unit — the
				// x1000 brings it back to the unit anyone actually quotes.
				//
				// Derived rather than copied from unit_price_* because a
				// month can span a price change, and the effective rate a
				// customer paid is the one worth showing on their bill.
				t.Breakdown.Prices[p] = (revenue[i] / float64(qty[i])) * 1000
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

// CustomerInvoice is one row of a customer's billing history.
type CustomerInvoice struct {
	InvoiceId     string   `json:"invoiceId"`
	BillingMonth  string   `json:"billingMonth"`
	TotalAmount   float64  `json:"totalAmount"`
	Status        string   `json:"status"`
	PaidAt        *string  `json:"paidAt"`
	PaymentRef    *string  `json:"paymentReference"`
	ReminderCount int      `json:"reminderCount"`
	SuspendedAt   *string  `json:"suspendedAt"`
	PayURL        string   `json:"payUrl"`
	LineItems     []string `json:"lineItems"`
}

// GetCustomerInvoices returns every invoice ever raised for a customer,
// newest first.
//
// Exists because the customer detail screen previously showed only identity
// and slots — an admin fielding "what did I pay in July?" had to leave the
// customer, open Billing, pick the month, and find them again. The data was
// always there; nothing surfaced it in the one place an admin looks when
// they're thinking about a specific person.
func (br BillingResource) GetCustomerInvoices(w http.ResponseWriter, r *http.Request) {
	customerID := chi.URLParam(r, "id")

	rows, err := br.DB.Query(r.Context(), `
		SELECT id::text, billing_month, total_amount, status,
		       TO_CHAR(paid_at, 'YYYY-MM-DD'), payment_reference,
		       COALESCE(reminder_count, 0),
		       TO_CHAR(suspended_at, 'YYYY-MM-DD'),
		       breakdown
		FROM invoices
		WHERE customer_id = $1
		ORDER BY billing_month DESC
	`, customerID)
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	labels := map[string]string{
		"milk": "Milk", "curd": "Curd", "butter": "Butter", "ghee": "Ghee",
		"lassi": "Buttermilk", "paneer": "Paneer", "jaggery": "Jaggery",
		"khand": "Desi Khand", "oil": "Mustard Oil", "atta": "Atta", "burfi": "Burfi",
	}
	litre := map[string]bool{"milk": true, "curd": true, "lassi": true, "oil": true}

	out := []CustomerInvoice{}
	for rows.Next() {
		var (
			inv          CustomerInvoice
			breakdownRaw []byte
		)
		if err := rows.Scan(&inv.InvoiceId, &inv.BillingMonth, &inv.TotalAmount, &inv.Status,
			&inv.PaidAt, &inv.PaymentRef, &inv.ReminderCount, &inv.SuspendedAt, &breakdownRaw); err != nil {
			http.Error(w, "Scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// A compact one-line-per-product summary so the screen can show what
		// the bill was made of without a second request per invoice.
		var bd InvoiceBreakdown
		if len(breakdownRaw) > 0 {
			_ = json.Unmarshal(breakdownRaw, &bd)
		}
		for _, p := range Products {
			qty := bd.Quantities[p]
			total := bd.LineTotals[p]
			if qty <= 0 {
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
			inv.LineItems = append(inv.LineItems, fmt.Sprintf("%s %g%s — ₹%.0f", labels[p], display, unit, total))
		}

		// Only unpaid invoices get a pay link; a settled one would be a dead
		// end for the admin and a confusing thing to forward to a customer.
		if !strings.HasPrefix(inv.Status, "PAID") {
			inv.PayURL = publicBaseURL() + "/pay/" + inv.InvoiceId
		}

		out = append(out, inv)
	}

	writeJSON(w, http.StatusOK, out)
}

// publicBaseURL is where customer-facing pages live. Read from the
// environment so a staging deploy doesn't hand out production links.
func publicBaseURL() string {
	if v := os.Getenv("PUBLIC_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://pgsbackend-e4hiw.ondigitalocean.app"
}

// Generate creates invoices for a completed month and fires off a WhatsApp
// bill to each customer whose invoice was just created. Pre-existing invoices
// are not re-messaged (see notifyFreshInvoices below).
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
	// Twilio calls at ~500ms each × N customers can take a long time, and
	// each one now also creates a Razorpay link, so we don't hold the admin's
	// HTTP request open while it runs. Failures get logged; admin can
	// re-notify a specific customer via the billing drill-down.
	if br.WhatsApp != nil && generated > 0 {
		go br.notifyFreshInvoices(req.Month, tallies)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"message": "Billing cycle processed", "month": req.Month, "invoicesGenerated": generated, "alreadyExisted": skipped})
}

// notifyFreshInvoices sends the month-end bill to customers whose invoice
// was created within the last minute — i.e. by the Generate call that just
// finished. Filtering by created_at avoids re-messaging customers whose
// invoices already existed from a previous Generate run.
// billSendConcurrency is how many bills go out at once.
//
// Five, not one and not fifty. Serial was the original bug — 700 customers at
// ~1.5s each needs seventeen minutes and the send died halfway. Going much
// wider trades that for a different failure: WhatsApp throttles per sender, so
// a burst of fifty simply queues at Twilio while consuming a pool connection
// each, and Razorpay rate-limits too.
//
// Five gets 700 bills out in roughly three and a half minutes, comfortably
// inside the budget, without either provider pushing back.
const billSendConcurrency = 5

// notifyFreshInvoices sends bills for every invoice this month that hasn't had
// one, concurrently and resumably.
//
// # RESUMABLE MATTERS MORE THAN CONCURRENT
//
// Work is selected by `bill_sent_at IS NULL`, not by creation time. That means
// a run which dies partway — timeout, deploy, crash — leaves the remaining
// invoices still marked unsent, and the next run picks up exactly those. The
// previous design keyed off "created in the last 60 seconds", so anything
// missed was missed permanently and invisibly.
//
// Marking happens AFTER a successful send. The opposite order would be safer
// against duplicates but far worse in practice: a customer who never receives
// a bill still gets chased for it daily and suspended on the 6th, which is a
// much more damaging failure than receiving the same bill twice.
func (br BillingResource) notifyFreshInvoices(month string, tallies []CustomerTally) {
	// Budget is generous but finite. The work is now resumable, so hitting it
	// costs a delay rather than a permanent gap.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rows, err := br.DB.Query(ctx, `
		SELECT customer_id FROM invoices
		WHERE billing_month = $1 AND bill_sent_at IS NULL
	`, month)
	if err != nil {
		fmt.Printf("⚠️ notifyFreshInvoices: %v\n", err)
		return
	}

	unsent := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			unsent[id] = true
		}
	}
	rows.Close()

	// Only customers who both owe something and haven't been told.
	var queue []CustomerTally
	for _, t := range tallies {
		if t.TotalAmount > 0 && unsent[t.CustomerId] {
			queue = append(queue, t)
		}
	}
	if len(queue) == 0 {
		return
	}

	fmt.Printf("📤 sending %d bill(s) for %s\n", len(queue), month)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		sent   int
		failed int
	)
	jobs := make(chan CustomerTally)

	for i := 0; i < billSendConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				// Stop cleanly if the budget ran out. Remaining invoices keep
				// bill_sent_at NULL and are picked up by the next run.
				if ctx.Err() != nil {
					return
				}
				if err := br.sendOneBill(ctx, t, month); err != nil {
					fmt.Printf("⚠️ bill send failed for %s: %v\n", t.CustomerId, err)
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				mu.Lock()
				sent++
				mu.Unlock()
			}
		}()
	}

	for _, t := range queue {
		if ctx.Err() != nil {
			break
		}
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	remaining := len(queue) - sent
	if remaining > 0 {
		// Loud, because this is the case that used to be silent. Anyone still
		// unsent will be chased by the dunning cycle for a bill they never
		// got, so it needs a human before the 2nd.
		fmt.Printf("🚨 BILLS INCOMPLETE for %s — %d sent, %d still unsent. Re-run Generate to finish; nothing will be duplicated.\n",
			month, sent, remaining)
	} else {
		fmt.Printf("✅ all %d bill(s) sent for %s\n", sent, month)
	}

	// Recorded so the dashboard can show a partial run rather than leaving it
	// to be discovered from customer complaints.
	br.recordBillRun(ctx, month, sent, remaining, failed)
}

// sendOneBill delivers a single bill and marks it, so the marker can never be
// set for a message that didn't go out.
func (br BillingResource) sendOneBill(ctx context.Context, t CustomerTally, month string) error {
	if err := br.notifyBill(t, month); err != nil {
		return err
	}
	_, err := br.DB.Exec(ctx, `
		UPDATE invoices SET bill_sent_at = NOW()
		WHERE customer_id = $1 AND billing_month = $2
	`, t.CustomerId, month)
	return err
}

// recordBillRun writes the outcome to dunning_runs so a partial send is
// visible on the dashboard. Best-effort: failing to record must not change
// what was actually sent.
func (br BillingResource) recordBillRun(ctx context.Context, month string, sent, remaining, failed int) {
	note := fmt.Sprintf("%d sent", sent)
	if remaining > 0 {
		note = fmt.Sprintf("%d sent, %d NOT SENT (%d errors) — re-run Generate", sent, remaining, failed)
	}
	runID, _ := uuid.NewV7()
	_, _ = br.DB.Exec(ctx, `
		INSERT INTO dunning_runs (id, run_date, phase, billing_month, affected, note)
		VALUES ($1, CURRENT_DATE, 'GENERATE', $2, $3, $4)
		ON CONFLICT (run_date, phase) DO UPDATE
		SET affected = EXCLUDED.affected, note = EXCLUDED.note, billing_month = EXCLUDED.billing_month
	`, runID, month, sent, note)
}

// paymentLine builds the "how to pay" part of a bill message.
//
// Returns a Razorpay link when the gateway is configured and the call
// succeeds, and falls back to manual instructions otherwise. The fallback is
// deliberate: a gateway outage at month-end must not stop bills going out —
// worst case the customer pays the old way and an admin marks it manually,
// which is exactly where this system was before payments existed.
func (br BillingResource) paymentLine(ctx context.Context, customerID uuid.UUID, month string) string {
	const manual = "To pay, please transfer to our UPI ID and reply PAID once done. " +
		"Our team will confirm and mark your bill as settled."

	if br.Payment == nil || !br.Payment.Enabled() {
		return manual
	}

	// The invoice id isn't carried on CustomerTally, and
	// (customer_id, billing_month) is unique, so looking it up here avoids
	// threading it through every caller of notifyBill.
	var invoiceID string
	if err := br.DB.QueryRow(ctx, `
		SELECT id::text FROM invoices WHERE customer_id = $1 AND billing_month = $2
	`, customerID, month).Scan(&invoiceID); err != nil {
		fmt.Printf("⚠️ paymentLine: no invoice for customer %s month %s: %v\n", customerID, month, err)
		return manual
	}

	url, err := br.Payment.CreateLinkForInvoice(ctx, invoiceID)
	if err != nil {
		fmt.Printf("⚠️ paymentLine: payment link failed for invoice %s: %v\n", invoiceID, err)
		return manual
	}

	return "Tap here to pay securely:\n" + url
}

// notifyBill sends the month-end bill total to a single customer over
// WhatsApp. Uses a free-form text message via SendDeliveryUpdate rather than
// a Twilio Content Template — templates would need Meta approval for every
// tweak to the wording, and this message body changes with the product mix
// each month. Once the format is stable we can switch to a template.
// notifyBill sends a customer their month-end bill.
//
// Uses the approved Twilio Content Template rather than free-form text.
// WhatsApp only allows free-form within 24 hours of the customer's last
// inbound message, and a month-end bill goes out to everyone regardless of
// whether they've messaged — so the free-form version silently failed with
// error 63016 for anyone who hadn't been in touch that day. In development
// that never showed up, because a developer testing the bot is always inside
// the window.
//
// The itemised breakdown moved to the hosted /pay page. Meta rejects template
// parameters containing newlines, so a multi-line product list can never be a
// variable — the template carries name, month and total, and the button links
// to the full bill.
//
// Falls back to the old free-form message when no template SID is configured,
// so a deployment without templates still bills people (it just won't reach
// quiet customers).
func (br BillingResource) notifyBill(t CustomerTally, month string) error {
	// Long enough to cover a Razorpay round trip plus the Twilio send.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var phone string
	if err := br.DB.QueryRow(ctx, `SELECT phone_number FROM customers WHERE id = $1`, t.CustomerId).Scan(&phone); err != nil {
		// Returned rather than logged-and-swallowed: the caller marks the
		// invoice as sent only on success, so this customer stays in the
		// queue for the next run instead of being silently skipped forever.
		return fmt.Errorf("no phone for customer %s: %w", t.CustomerId, err)
	}

	// The invoice id is what the template's button URL appends to /pay/.
	// Without it there's nothing to link to, so fall back rather than send a
	// bill the customer can't act on.
	var invoiceID string
	err := br.DB.QueryRow(ctx,
		`SELECT id::text FROM invoices WHERE customer_id = $1 AND billing_month = $2`,
		t.CustomerId, month).Scan(&invoiceID)
	if err != nil {
		fmt.Printf("⚠️ notifyBill: no invoice row for %s / %s: %v\n", t.CustomerId, month, err)
	}

	if br.Templates.Bill != "" && invoiceID != "" {
		if err := br.WhatsApp.SendBill(
			phone, br.Templates.Bill,
			t.CustomerName, labelForMonth(month),
			formatAmount(t.TotalAmount), invoiceID,
		); err != nil {
			// Log and fall through to free-form: a failed template send is
			// better followed by an attempt that might land than by silence.
			fmt.Printf("⚠️ notifyBill: template send failed for %s, falling back: %v\n", phone, err)
		} else {
			return nil
		}
	}

	return br.notifyBillFreeForm(ctx, phone, t, month)
}

// notifyBillFreeForm is the pre-template message, kept as a fallback for
// deployments without approved templates and for the 24-hour window where it
// still works. Retains the full itemised breakdown, which the template can't
// carry.
func (br BillingResource) notifyBillFreeForm(ctx context.Context, phone string, t CustomerTally, month string) error {
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
			"*Total: ₹%.0f*\n\n%s",
		labelForMonth(month), t.CustomerName, strings.Join(lines, "\n"), t.TotalAmount,
		br.paymentLine(ctx, t.CustomerId, month),
	)

	if err := br.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
		return fmt.Errorf("whatsapp send to %s: %w", phone, err)
	}
	return nil
}

// formatAmount renders a rupee amount with Indian digit grouping and no
// symbol — the ₹ is baked into the approved template body, so including it
// here would render "₹₹5,120".
func formatAmount(v float64) string {
	whole := int64(v + 0.5)
	s := fmt.Sprintf("%d", whole)
	if len(s) <= 3 {
		return s
	}
	last3 := s[len(s)-3:]
	rest := s[:len(s)-3]
	var parts []string
	for len(rest) > 2 {
		parts = append([]string{rest[len(rest)-2:]}, parts...)
		rest = rest[:len(rest)-2]
	}
	if rest != "" {
		parts = append([]string{rest}, parts...)
	}
	return strings.Join(parts, ",") + "," + last3
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

// MarkPaid is the manual path — cash, or an admin correcting a status.
//
// Marking anything PAID also cancels an outstanding Razorpay link. Without
// that, a customer who paid cash could still tap the link they were sent and
// pay a second time. The webhook would catch it as ALREADY_PAID, but issuing
// a refund is worse than a link that quietly stops working.
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
	var linkID *string
	err := br.DB.QueryRow(r.Context(),
		`UPDATE invoices SET status = $1, updated_at = NOW() WHERE id = $2 RETURNING id, payment_link_id`,
		req.Status, id,
	).Scan(&updated, &linkID)
	if err == pgx.ErrNoRows {
		http.Error(w, "Invoice not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if strings.HasPrefix(req.Status, "PAID") && linkID != nil && br.Payment != nil && br.Payment.Enabled() {
		go func(lid string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := br.Payment.CancelLink(ctx, lid); err != nil {
				fmt.Printf("⚠️ MarkPaid: couldn't cancel link %s: %v\n", lid, err)
			}
		}(*linkID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Invoice status updated", "status": req.Status})
}

func validMonth(m string) bool { _, err := time.Parse("2006-01", m); return err == nil }
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
