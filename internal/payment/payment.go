package payment

// Razorpay Payment Links.
//
// Flow: month-end billing creates a link per invoice, the link URL goes out
// in the WhatsApp bill, the customer taps and pays, and Razorpay posts a
// payment_link.paid webhook back to us (see webhook.go).
//
// Why Payment Links rather than Orders + Checkout: Checkout expects a
// browser session you control. We only ever hand the customer a URL over
// WhatsApp, so a hosted link is the whole requirement — no frontend, no SDK,
// no session to manage.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const razorpayBase = "https://api.razorpay.com/v1"

type Service struct {
	DB        *pgxpool.Pool
	KeyID     string
	KeySecret string
	// WebhookSecret is separate from KeySecret — it's set when you create the
	// webhook in the dashboard, and it's what signs incoming payloads.
	WebhookSecret string

	HTTP *http.Client

	// OnPaid fires after an invoice is successfully marked paid. Wired in
	// main.go to send a WhatsApp receipt — a callback so this package stays
	// unaware of notifications.
	OnPaid PaidCallback
}

func New(db *pgxpool.Pool, keyID, keySecret, webhookSecret string) *Service {
	return &Service{
		DB: db, KeyID: keyID, KeySecret: keySecret, WebhookSecret: webhookSecret,
		// Razorpay is generally quick, but a hung request during month-end
		// billing would stall the whole batch. Fail fast and retry later.
		HTTP: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Service) Enabled() bool {
	return s != nil && s.KeyID != "" && s.KeySecret != ""
}

// -------------------------------------------------------------------------
// Razorpay wire types
// -------------------------------------------------------------------------

type linkCustomer struct {
	Name    string `json:"name,omitempty"`
	Contact string `json:"contact,omitempty"`
	Email   string `json:"email,omitempty"`
}

type linkNotify struct {
	SMS   bool `json:"sms"`
	Email bool `json:"email"`
}

type createLinkRequest struct {
	// Razorpay takes the smallest currency unit — paise, not rupees.
	// ₹4,200.00 is 420000. Getting this wrong by a factor of 100 is the
	// classic first bug with this API.
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Description    string            `json:"description"`
	Customer       linkCustomer      `json:"customer"`
	Notify         linkNotify        `json:"notify"`
	ReminderEnable bool              `json:"reminder_enable"`
	Notes          map[string]string `json:"notes,omitempty"`
	// Partial payment is off deliberately. A half-paid invoice has no state
	// in our schema, and inventing one to support a feature nobody asked for
	// would be the wrong trade.
	PartialPayment bool   `json:"accept_partial"`
	ExpireBy       int64  `json:"expire_by,omitempty"`
	ReferenceID    string `json:"reference_id,omitempty"`
}

type createLinkResponse struct {
	Id       string `json:"id"`
	ShortURL string `json:"short_url"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
}

type razorpayError struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
		Reason      string `json:"reason"`
	} `json:"error"`
}

// -------------------------------------------------------------------------
// HTTP plumbing
// -------------------------------------------------------------------------

func (s *Service) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, razorpayBase+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.KeyID, s.KeySecret)
	req.Header.Set("Content-Type", "application/json")

	res, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay unreachable: %w", err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode >= 400 {
		var e razorpayError
		if json.Unmarshal(raw, &e) == nil && e.Error.Description != "" {
			return fmt.Errorf("razorpay %d [%s]: %s", res.StatusCode, e.Error.Code, e.Error.Description)
		}
		return fmt.Errorf("razorpay %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// rupeesToPaise converts our NUMERIC(10,2) rupee amounts to Razorpay's
// integer paise. math.Round rather than truncation — float64 can represent
// 4200.00 as 4199.9999999, and truncating would quietly undercharge.
func rupeesToPaise(rupees float64) int64 {
	return int64(math.Round(rupees * 100))
}

// -------------------------------------------------------------------------
// Create
// -------------------------------------------------------------------------

// CreateLinkForInvoice creates (or reuses) a payment link for one invoice and
// returns the short URL to put in the customer's WhatsApp bill.
//
// Idempotent by design: if a link already exists and still matches the
// invoice amount, the existing URL comes straight back. Re-running month-end
// billing therefore doesn't litter a customer's Razorpay history with
// duplicate links for the same bill.
func (s *Service) CreateLinkForInvoice(ctx context.Context, invoiceID string) (string, error) {
	var (
		total       float64
		status      string
		month       string
		custName    string
		custPhone   string
		existingID  *string
		existingURL *string
		existingAmt *float64
	)

	err := s.DB.QueryRow(ctx, `
		SELECT i.total_amount, i.status, i.billing_month, c.name, c.phone_number,
		       i.payment_link_id, i.payment_link_url, i.payment_link_amount
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1
	`, invoiceID).Scan(&total, &status, &month, &custName, &custPhone,
		&existingID, &existingURL, &existingAmt)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("invoice not found")
	}
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(status, "PAID") {
		return "", fmt.Errorf("invoice is already marked paid")
	}
	if total <= 0 {
		return "", fmt.Errorf("invoice total is zero — nothing to collect")
	}

	// Reuse only when the amount still matches. If the invoice was
	// recalculated after a delivery correction, the old link would charge
	// the wrong figure, so we cancel it and issue a fresh one.
	if existingID != nil && existingURL != nil && existingAmt != nil {
		if rupeesToPaise(*existingAmt) == rupeesToPaise(total) {
			return *existingURL, nil
		}
		if err := s.CancelLink(ctx, *existingID); err != nil {
			// Non-fatal: a stale link that can't be cancelled is untidy but
			// the new one is what the customer will be sent. Worth logging.
			fmt.Printf("⚠️ couldn't cancel stale payment link %s: %v\n", *existingID, err)
		}
	}

	// Phone must be in a form Razorpay accepts. Ours are stored with the
	// country code from WhatsApp; strip anything that isn't a digit or '+'.
	contact := sanitizePhone(custPhone)

	req := createLinkRequest{
		Amount:      rupeesToPaise(total),
		Currency:    "INR",
		Description: fmt.Sprintf("PGS Direct — %s milk delivery bill", prettyMonth(month)),
		Customer:    linkCustomer{Name: custName, Contact: contact},
		// We send the bill over WhatsApp ourselves with the full itemised
		// breakdown. Letting Razorpay also SMS them would be a second,
		// worse-looking message about the same money.
		Notify:         linkNotify{SMS: false, Email: false},
		ReminderEnable: false,
		PartialPayment: false,
		Notes: map[string]string{
			"invoice_id":    invoiceID,
			"billing_month": month,
		},
		// Razorpay caps expiry at six months. Thirty days is plenty for a
		// monthly bill and means a forgotten link can't be paid against a
		// long-settled invoice.
		ExpireBy:    time.Now().AddDate(0, 0, 30).Unix(),
		ReferenceID: invoiceID,
	}

	var out createLinkResponse
	if err := s.do(ctx, http.MethodPost, "/payment_links", req, &out); err != nil {
		return "", err
	}
	if out.ShortURL == "" {
		return "", fmt.Errorf("razorpay returned no short_url")
	}

	if _, err := s.DB.Exec(ctx, `
		UPDATE invoices
		SET payment_link_id = $1, payment_link_url = $2,
		    payment_link_amount = $3, payment_link_sent_at = NOW(), updated_at = NOW()
		WHERE id = $4
	`, out.Id, out.ShortURL, total, invoiceID); err != nil {
		// The link exists at Razorpay but we failed to record it. Return the
		// URL anyway — the customer can still pay, and the webhook carries
		// invoice_id in notes, so reconciliation survives this.
		fmt.Printf("⚠️ payment link %s created but not saved to invoice %s: %v\n", out.Id, invoiceID, err)
		return out.ShortURL, nil
	}

	return out.ShortURL, nil
}

// CancelLink voids a link so a stale amount can't be paid.
func (s *Service) CancelLink(ctx context.Context, linkID string) error {
	return s.do(ctx, http.MethodPost, "/payment_links/"+linkID+"/cancel", nil, nil)
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

func sanitizePhone(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == '+' && b.Len() == 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func prettyMonth(m string) string {
	if t, err := time.Parse("2006-01", m); err == nil {
		return t.Format("January 2006")
	}
	return m
}
