package payment

// Razorpay webhook receiver.
//
// This is the only place in the system where an external party can change an
// invoice's payment status, so it's deliberately paranoid:
//
//   1. Signature verified against the RAW body before anything is parsed.
//   2. Every event recorded by its x-razorpay-event-id, which is the primary
//      key — duplicates conflict rather than double-applying.
//   3. An already-paid invoice is never overwritten; that case is logged for
//      a human instead.
//   4. A mismatched amount is applied but flagged, never silently accepted.
//
// Razorpay documents that duplicate deliveries are expected and that events
// may arrive out of order, so none of the above is theoretical.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Service) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/razorpay", s.HandleWebhook)
	return r
}

// -------------------------------------------------------------------------
// Payload
// -------------------------------------------------------------------------

type webhookEnvelope struct {
	Event   string `json:"event"`
	Payload struct {
		PaymentLink struct {
			Entity struct {
				Id          string            `json:"id"`
				Status      string            `json:"status"`
				Amount      int64             `json:"amount"`
				AmountPaid  int64             `json:"amount_paid"`
				ReferenceId string            `json:"reference_id"`
				Notes       map[string]string `json:"notes"`
			} `json:"entity"`
		} `json:"payment_link"`
		Payment struct {
			Entity struct {
				Id     string `json:"id"`
				Amount int64  `json:"amount"`
				Method string `json:"method"`
				Status string `json:"status"`
			} `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

// -------------------------------------------------------------------------
// Handler
// -------------------------------------------------------------------------

func (s *Service) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	// Read the raw body first and never re-read it. Razorpay's signature is
	// computed over the exact bytes sent; decoding into a struct and
	// re-encoding would change whitespace and key order, and the signature
	// would never match again.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Razorpay-Signature")
	if !s.validSignature(raw, signature) {
		// Deliberately vague to the caller, loud in the logs. A failure here
		// is either misconfiguration or someone probing the endpoint.
		fmt.Printf("🚨 razorpay webhook: signature verification FAILED (len=%d)\n", len(raw))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventID := r.Header.Get("X-Razorpay-Event-Id")
	if eventID == "" {
		// Shouldn't happen, but without it we have no idempotency key, and
		// silently processing an unidentifiable money event is worse than
		// rejecting it.
		http.Error(w, "missing event id", http.StatusBadRequest)
		return
	}

	var evt webhookEnvelope
	if err := json.Unmarshal(raw, &evt); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	// Claim the event. The primary key on event_id means a redelivery loses
	// this race and exits without touching the invoice. Doing the claim
	// before the work — rather than checking-then-writing — closes the
	// window where two concurrent deliveries both see "not processed yet".
	claimed, err := s.claimEvent(r.Context(), eventID, evt.Event, raw)
	if err != nil {
		// Storage failure. Return 500 so Razorpay retries rather than
		// treating an unprocessed payment as delivered.
		fmt.Printf("⚠️ razorpay webhook: could not record event %s: %v\n", eventID, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !claimed {
		// Already seen. 200 so Razorpay stops retrying.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"duplicate"}`))
		return
	}

	// Acknowledge fast, then process. Razorpay retries on non-2xx, and a slow
	// handler risks a timeout that triggers a redelivery we've already begun
	// working on. The event row is claimed, so the work is safe to finish
	// after the response.
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))

	go s.process(eventID, evt)
}

// validSignature is HMAC-SHA256 of the raw body keyed by the webhook secret,
// compared in constant time.
func (s *Service) validSignature(body []byte, signature string) bool {
	if s.WebhookSecret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal, not ==, to avoid leaking timing information about how much
	// of a guessed signature was correct.
	return hmac.Equal([]byte(expected), []byte(signature))
}

// claimEvent inserts the event row. Returns false if this event was already
// recorded, which is how duplicate deliveries are rejected.
func (s *Service) claimEvent(ctx context.Context, eventID, eventType string, raw []byte) (bool, error) {
	tag, err := s.DB.Exec(ctx, `
		INSERT INTO payment_events (event_id, event_type, payload, outcome)
		VALUES ($1, $2, $3, 'IGNORED')
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, eventType, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// -------------------------------------------------------------------------
// Processing
// -------------------------------------------------------------------------

func (s *Service) process(eventID string, evt webhookEnvelope) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// We only act on a link being fully paid. payment.captured and friends
	// describe the same money from a different angle; acting on both would
	// mean handling the same rupees twice.
	if evt.Event != "payment_link.paid" {
		s.finish(ctx, eventID, nil, "IGNORED", "event type not actioned: "+evt.Event)
		return
	}

	link := evt.Payload.PaymentLink.Entity
	payment := evt.Payload.Payment.Entity

	// invoice_id travels in notes, set at link creation. Falling back to
	// reference_id and then payment_link_id means a link created before this
	// code shipped, or one created manually in the dashboard, still resolves.
	invoiceID := link.Notes["invoice_id"]
	if invoiceID == "" {
		invoiceID = link.ReferenceId
	}

	var (
		resolvedID string
		status     string
		total      float64
	)
	err := s.DB.QueryRow(ctx, `
		SELECT id::text, status, total_amount
		FROM invoices
		WHERE id::text = $1 OR payment_link_id = $2
		LIMIT 1
	`, invoiceID, link.Id).Scan(&resolvedID, &status, &total)
	if err != nil {
		s.finish(ctx, eventID, nil, "UNMATCHED",
			fmt.Sprintf("no invoice for notes.invoice_id=%q link=%s: %v", invoiceID, link.Id, err))
		fmt.Printf("🚨 razorpay: payment received but no matching invoice — link=%s payment=%s\n", link.Id, payment.Id)
		return
	}

	// Already settled. Do NOT overwrite: someone may have paid cash on the
	// 3rd and then tapped an old link on the 5th, which is real money that
	// needs refunding, not a status to flip.
	if strings.HasPrefix(status, "PAID") {
		s.finish(ctx, eventID, &resolvedID, "ALREADY_PAID",
			fmt.Sprintf("invoice already %s; payment %s of %d paise arrived anyway", status, payment.Id, payment.Amount))
		fmt.Printf("🚨 razorpay: POSSIBLE DOUBLE PAYMENT — invoice %s already %s, payment %s\n", resolvedID, status, payment.Id)
		return
	}

	paidPaise := link.AmountPaid
	if paidPaise == 0 {
		paidPaise = payment.Amount
	}
	expectedPaise := rupeesToPaise(total)

	outcome := "APPLIED"
	note := fmt.Sprintf("payment %s, method %s, %d paise", payment.Id, payment.Method, paidPaise)

	// Mark paid either way — the money is real and withholding the status
	// would leave the customer chased for a bill they've settled. But flag a
	// discrepancy so it reaches a human. The usual cause is an invoice
	// recalculated after the link went out.
	if paidPaise != expectedPaise {
		outcome = "AMOUNT_MISMATCH"
		note = fmt.Sprintf("%s — expected %d paise, invoice may have been recalculated after the link was sent",
			note, expectedPaise)
		fmt.Printf("⚠️ razorpay: amount mismatch on invoice %s — paid %d, expected %d\n",
			resolvedID, paidPaise, expectedPaise)
	}

	if _, err := s.DB.Exec(ctx, `
		UPDATE invoices
		SET status = 'PAID_ONLINE',
		    paid_at = NOW(),
		    payment_reference = $1,
		    updated_at = NOW()
		WHERE id = $2::uuid AND status NOT LIKE 'PAID%'
	`, payment.Id, resolvedID); err != nil {
		s.finish(ctx, eventID, &resolvedID, "UNMATCHED", "update failed: "+err.Error())
		fmt.Printf("🚨 razorpay: failed marking invoice %s paid: %v\n", resolvedID, err)
		return
	}

	s.finish(ctx, eventID, &resolvedID, outcome, note)
	fmt.Printf("💰 razorpay: invoice %s marked paid (%s)\n", resolvedID, outcome)

	if s.OnPaid != nil {
		s.OnPaid(resolvedID, float64(paidPaise)/100)
	}
}

// OnPaid, if set, is called after an invoice is successfully marked paid.
// Wired in main.go to send the customer a WhatsApp receipt — kept as a
// callback so this package doesn't need to know about notifications.
type PaidCallback func(invoiceID string, amount float64)

func (s *Service) finish(ctx context.Context, eventID string, invoiceID *string, outcome, note string) {
	if _, err := s.DB.Exec(ctx, `
		UPDATE payment_events SET invoice_id = $1::uuid, outcome = $2, note = $3
		WHERE event_id = $4
	`, invoiceID, outcome, note, eventID); err != nil {
		fmt.Printf("⚠️ razorpay: could not finalise event %s: %v\n", eventID, err)
	}
}
