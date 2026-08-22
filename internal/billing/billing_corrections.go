package billing

// Two admin escape hatches that the regular month-end cycle doesn't cover.
//
//   FinalInvoice — a customer discontinuing mid-month. The normal Generate
//   refuses the current month (correctly: more deliveries are still coming),
//   but the delete guard requires no unbilled deliveries. Together they trap
//   a leaver for up to 30 days. This closes their month early.
//
//   RegenerateInvoice — an invoice is a frozen snapshot, and Generate uses
//   ON CONFLICT DO NOTHING so it never overwrites one. When a delivery log is
//   corrected after invoicing, the bill silently stays wrong. This recomputes
//   a single invoice from the current logs.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"
)

// aggregateOne is aggregate() scoped to a single customer. Kept separate
// rather than adding an optional filter to aggregate(), because that function
// is on the month-end hot path and shouldn't grow branches.
func (br BillingResource) aggregateOne(ctx context.Context, customerID, month string) (*CustomerTally, error) {
	sums := make([]string, 0, len(Products)*2)
	for _, p := range Products {
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(d.delivered_%s_qty), 0)", p))
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(d.delivered_%s_qty * d.unit_price_%s), 0)", p, p))
	}

	query := fmt.Sprintf(`
		SELECT c.id, c.name, c.phone_number, COUNT(d.id), %s
		FROM customers c
		JOIN delivery_logs d ON d.customer_id = c.id
		WHERE c.id = $1
		  AND d.status = 'DELIVERED'
		  AND TO_CHAR(d.delivery_date, 'YYYY-MM') = $2
		GROUP BY c.id, c.name, c.phone_number
	`, strings.Join(sums, ", "))

	n := len(Products)
	var t CustomerTally
	qty := make([]int, n)
	revenue := make([]float64, n)
	dest := []any{&t.CustomerId, &t.CustomerName, &t.PhoneNumber, &t.DeliveryCount}
	for i := 0; i < n; i++ {
		dest = append(dest, &qty[i], &revenue[i])
	}

	if err := br.DB.QueryRow(ctx, query, customerID, month).Scan(dest...); err != nil {
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
	return &t, nil
}

// FinalInvoice closes out one customer's current month early.
//
// Deliberately does NOT change status or is_active — admin deactivates
// separately once payment lands, so they keep a clear "billed, awaiting
// payment" state to work from.
//
// It DOES set stop_order = 0 on every subscription, which drops the customer
// off all manifests immediately. Without that, deliveries could continue
// after the invoice was frozen and would never be billed: month-end Generate
// skips anyone who already has an invoice for that month, so those deliveries
// would fall through the gap permanently.
func (br BillingResource) FinalInvoice(w http.ResponseWriter, r *http.Request) {
	customerID := chi.URLParam(r, "customerId")
	month := time.Now().Format("2006-01")

	tally, err := br.aggregateOne(r.Context(), customerID, month)
	if err == pgx.ErrNoRows {
		http.Error(w, "This customer has no delivered orders this month — nothing to bill. You can deactivate or delete them directly.", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "Calculation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tally.TotalAmount <= 0 {
		http.Error(w, "This customer's deliveries this month total zero. Nothing to bill.", http.StatusBadRequest)
		return
	}

	tx, err := br.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "Transaction error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	invID, _ := uuid.NewV7()
	breakdown, _ := json.Marshal(tally.Breakdown)
	tag, err := tx.Exec(r.Context(), `
		INSERT INTO invoices (id, customer_id, billing_month, total_amount, breakdown)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (customer_id, billing_month) DO NOTHING
	`, invID, tally.CustomerId, month, tally.TotalAmount, breakdown)
	if err != nil {
		http.Error(w, "Invoice save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "This customer already has an invoice for this month. Use Regenerate if the amount needs correcting.", http.StatusConflict)
		return
	}

	// Drop off every manifest, both slots.
	if _, err := tx.Exec(r.Context(),
		`UPDATE subscriptions SET stop_order = 0, updated_at = NOW() WHERE customer_id = $1`,
		customerID,
	); err != nil {
		http.Error(w, "Failed to remove from routes: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Mark the account as paused too, not just un-routed.
	//
	// stop_order = 0 stops the deliveries, but status stayed 'active' — so on
	// WhatsApp the customer still saw the ordinary menu and could request
	// overrides and order changes for deliveries that were never coming.
	// Everything looked like it was working while nothing was being
	// delivered, and they'd have had no way to tell.
	//
	// Deliberately 'disabled' rather than deactivating outright: the admin
	// still closes the account manually once payment lands, and 'disabled'
	// leaves the customer able to reply RESUME if they change their mind.
	if _, err := tx.Exec(r.Context(),
		`UPDATE customers SET is_active = false, status = 'disabled' WHERE id = $1 AND status = 'active'`,
		customerID,
	); err != nil {
		http.Error(w, "Failed to pause the account: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "Commit failed", http.StatusInternalServerError)
		return
	}

	// notifyBill builds its own payment link, so the final invoice arrives
	// with the same tap-to-pay flow as a regular month-end bill.
	if br.WhatsApp != nil {
		go br.notifyBill(*tally, month)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message":     "Final invoice created. Deliveries stopped. Deactivate the customer once payment is settled.",
		"invoiceId":   invID.String(),
		"month":       month,
		"totalAmount": tally.TotalAmount,
	})
}

// RegenerateInvoice recomputes a frozen invoice from the current delivery
// logs. Used after an admin corrects a log that had already been invoiced.
//
// Refuses to touch invoices already marked paid: rewriting the amount on
// something the customer has settled would silently create or erase a
// balance with no audit trail. Admin has to reset it to PENDING first, which
// is a deliberate act.
func (br BillingResource) RegenerateInvoice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var customerID uuid.UUID
	var month, status string
	var oldAmount float64
	err := br.DB.QueryRow(r.Context(),
		`SELECT customer_id, billing_month, status, total_amount FROM invoices WHERE id = $1`, id,
	).Scan(&customerID, &month, &status, &oldAmount)
	if err == pgx.ErrNoRows {
		http.Error(w, "Invoice not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Database error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if strings.HasPrefix(status, "PAID") {
		http.Error(w, "This invoice is already marked paid. Reset it to Pending first if the amount genuinely needs to change.", http.StatusConflict)
		return
	}

	tally, err := br.aggregateOne(r.Context(), customerID.String(), month)
	if err == pgx.ErrNoRows {
		http.Error(w, "No delivered orders remain for this month — delete the invoice instead of regenerating it.", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "Calculation error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	breakdown, _ := json.Marshal(tally.Breakdown)
	if _, err := br.DB.Exec(r.Context(), `
		UPDATE invoices SET total_amount = $1, breakdown = $2, updated_at = NOW() WHERE id = $3
	`, tally.TotalAmount, breakdown, id); err != nil {
		http.Error(w, "Failed to update invoice: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Only re-message the customer when the number actually moved. A
	// regenerate that changes nothing shouldn't generate a WhatsApp.
	changed := tally.TotalAmount != oldAmount
	if changed && br.WhatsApp != nil {
		go br.notifyRevisedBill(*tally, month, oldAmount, id)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message":       "Invoice recalculated",
		"previousTotal": oldAmount,
		"newTotal":      tally.TotalAmount,
		"changed":       changed,
	})
}

// notifyRevisedBill tells the customer their bill moved and by how much.
// Leads with the direction of change, since that's the only thing they
// actually care about on opening the message.
//
// Takes the invoice id so it can reissue the payment link. The old link is
// locked to the old amount — CreateLinkForInvoice notices the mismatch,
// cancels it, and issues a fresh one. Without that, a customer could still
// tap the original and pay the wrong figure.
func (br BillingResource) notifyRevisedBill(t CustomerTally, month string, oldAmount float64, invoiceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var phone string
	if err := br.DB.QueryRow(ctx, `SELECT phone_number FROM customers WHERE id = $1`, t.CustomerId).Scan(&phone); err != nil {
		fmt.Printf("⚠️ notifyRevisedBill: no phone for %s: %v\n", t.CustomerId, err)
		return
	}

	direction := "reduced"
	if t.TotalAmount > oldAmount {
		direction = "increased"
	}
	diff := t.TotalAmount - oldAmount
	if diff < 0 {
		diff = -diff
	}

	payLine := "To pay, please transfer to our UPI ID and reply PAID once done."
	if br.Payment != nil && br.Payment.Enabled() {
		if url, err := br.Payment.CreateLinkForInvoice(ctx, invoiceID); err == nil {
			payLine = "Tap here to pay the updated amount:\n" + url
		} else {
			fmt.Printf("⚠️ notifyRevisedBill: link reissue failed for invoice %s: %v\n", invoiceID, err)
		}
	}

	msg := fmt.Sprintf(
		"🧾 *Your %s bill has been updated*\n\n"+
			"Hello %s,\n\n"+
			"We've corrected your bill after reviewing your deliveries. "+
			"Your total has %s by ₹%.0f.\n\n"+
			"Previous: ₹%.0f\n*Updated total: ₹%.0f*\n\n%s\n\n"+
			"Sorry for the confusion, and thank you for letting us know.",
		labelForMonth(month), t.CustomerName, direction, diff, oldAmount, t.TotalAmount, payLine,
	)

	// Template first: a bill correction follows an admin action that may
	// happen days after the original complaint, so the customer has usually
	// not messaged us and the free-form version below would be dropped.
	//
	// The template carries the pay button, which the free-form message can
	// only approximate with a bare URL — and without it the customer has to
	// scroll back through WhatsApp hunting for the original bill.
	if br.Templates.BillCorrected != "" {
		if err := br.WhatsApp.SendBillCorrected(
			phone, br.Templates.BillCorrected,
			t.CustomerName, labelForMonth(month), formatAmount(t.TotalAmount), invoiceID,
		); err == nil {
			return
		} else {
			fmt.Printf("⚠️ bill-corrected template failed for %s, falling back: %v\n", phone, err)
		}
	}

	if err := br.WhatsApp.SendDeliveryUpdate(phone, msg); err != nil {
		fmt.Printf("⚠️ notifyRevisedBill: send failed for %s: %v\n", phone, err)
	}
}
