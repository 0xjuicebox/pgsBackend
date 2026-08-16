package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	testPhone      = "+919911121912"
	eventIDPrefix  = "evt_webhooktest_"
	pollTimeout    = 15 * time.Second
	pollInterval   = 150 * time.Millisecond
	fixtureCustNam = "WEBHOOK TEST FIXTURE — safe to delete"
)

var (
	flagBase = flag.String("base", "", "server base URL (default http://localhost:$PORT, or :3000)")
	flagKeep = flag.Bool("keep", false, "skip cleanup so fixture rows can be inspected")
)

// -------------------------------------------------------------------------
// Harness plumbing
// -------------------------------------------------------------------------

type harness struct {
	db      *pgxpool.Pool
	base    string
	secret  string
	custID  uuid.UUID
	results []result
}

type result struct {
	name   string
	passed bool
	detail string
}

func (h *harness) pass(name, detail string) {
	h.results = append(h.results, result{name, true, detail})
	fmt.Printf("  \033[32mPASS\033[0m  %-24s %s\n", name, detail)
}

func (h *harness) fail(name, detail string) {
	h.results = append(h.results, result{name, false, detail})
	fmt.Printf("  \033[31mFAIL\033[0m  %-24s %s\n", name, detail)
}

// expect is the single assertion primitive: pass when cond holds, fail with
// got/want otherwise.
func (h *harness) expect(name string, cond bool, got, want string) {
	if cond {
		h.pass(name, got)
		return
	}
	h.fail(name, fmt.Sprintf("got %s, want %s", got, want))
}

// -------------------------------------------------------------------------
// Payload construction and signing
// -------------------------------------------------------------------------

// linkPayload builds a payment_link.paid envelope shaped to match
// webhookEnvelope in internal/payment/webhook.go. Amounts are in paise, which
// is what Razorpay sends and what rupeesToPaise produces.
//
// invoiceID lands in notes.invoice_id, which is where process() looks first.
// Passing "" exercises the reference_id / payment_link_id fallback path.
func linkPayload(event, linkID, invoiceID, paymentID string, amountPaise, paidPaise int64) map[string]any {
	notes := map[string]string{}
	if invoiceID != "" {
		notes["invoice_id"] = invoiceID
	}
	return map[string]any{
		"event":      event,
		"account_id": "acc_webhooktest",
		"created_at": time.Now().Unix(),
		"payload": map[string]any{
			"payment_link": map[string]any{
				"entity": map[string]any{
					"id":           linkID,
					"status":       "paid",
					"amount":       amountPaise,
					"amount_paid":  paidPaise,
					"reference_id": "",
					"notes":        notes,
				},
			},
			"payment": map[string]any{
				"entity": map[string]any{
					"id":     paymentID,
					"amount": paidPaise,
					"method": "upi",
					"status": "captured",
				},
			},
		},
	}
}

// sign returns the hex HMAC-SHA256 that validSignature recomputes. It must be
// taken over the exact bytes transmitted — hence the caller marshals once and
// reuses the buffer for both signing and the request body. Marshalling twice
// would be fine in Go today but is precisely the mistake the handler's comment
// warns against.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// post sends a payload. Pass signature="" for a correct signature, or any other
// string to send that value verbatim (used by the forged-signature case).
// Pass eventID="" to omit the header entirely.
func (h *harness) post(payload map[string]any, eventID, signature string) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	if signature == "" {
		signature = sign(h.secret, body)
	}

	req, err := http.NewRequest(http.MethodPost, h.base+"/payment/razorpay", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", signature)
	if eventID != "" {
		req.Header.Set("X-Razorpay-Event-Id", eventID)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, strings.TrimSpace(string(respBody)), nil
}

// -------------------------------------------------------------------------
// Database helpers
// -------------------------------------------------------------------------

// awaitOutcome blocks until Service.finish has written a note for this event,
// then returns the recorded outcome. See the package comment on why note is the
// completion signal.
func (h *harness) awaitOutcome(eventID string) (outcome, note string, err error) {
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		var o string
		var n *string
		err = h.db.QueryRow(context.Background(),
			`SELECT outcome, note FROM payment_events WHERE event_id = $1`, eventID).Scan(&o, &n)
		if err == nil && n != nil {
			return o, *n, nil
		}
		time.Sleep(pollInterval)
	}
	if err != nil {
		return "", "", fmt.Errorf("event %s never appeared: %w", eventID, err)
	}
	return "", "", fmt.Errorf("event %s claimed but never finished within %s", eventID, pollTimeout)
}

func (h *harness) eventCount(eventID string) int {
	var n int
	_ = h.db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM payment_events WHERE event_id = $1`, eventID).Scan(&n)
	return n
}

type invoiceState struct {
	status string
	paidAt *time.Time
	ref    *string
}

func (h *harness) invoice(id string) (invoiceState, error) {
	var st invoiceState
	err := h.db.QueryRow(context.Background(),
		`SELECT status, paid_at, payment_reference FROM invoices WHERE id = $1::uuid`, id).
		Scan(&st.status, &st.paidAt, &st.ref)
	return st, err
}

// newInvoice creates a fixture invoice. billingMonth must be unique per
// customer — invoices carries UNIQUE(customer_id, billing_month) — so each
// case gets its own 2099 month.
func (h *harness) newInvoice(billingMonth string, rupees float64, status, linkID string) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	_, err = h.db.Exec(context.Background(), `
		INSERT INTO invoices (id, customer_id, billing_month, total_amount, status, breakdown,
		                      payment_link_id, payment_link_amount)
		VALUES ($1, $2, $3, $4, $5, '{"fixture":true}'::jsonb, $6, $4)
	`, id, h.custID, billingMonth, rupees, status, linkID)
	return id.String(), err
}

// -------------------------------------------------------------------------
// Fixtures
// -------------------------------------------------------------------------

func (h *harness) setup() error {
	// Clear any residue from a previous -keep run so months don't collide.
	if err := h.cleanup(); err != nil {
		return err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	h.custID = id

	_, err = h.db.Exec(context.Background(), `
		INSERT INTO customers (id, name, phone_number, house_address, geo_latitude, geo_longitude, is_active, status)
		VALUES ($1, $2, $3, 'Fixture address, do not deliver', '0', '0', false, 'pending')
	`, h.custID, fixtureCustNam, testPhone)
	return err
}

func (h *harness) cleanup() error {
	ctx := context.Background()
	// payment_events.invoice_id is ON DELETE SET NULL, so those rows survive
	// the customer cascade and must go explicitly.
	if _, err := h.db.Exec(ctx,
		`DELETE FROM payment_events WHERE event_id LIKE $1`, eventIDPrefix+"%"); err != nil {
		return err
	}
	// invoices and subscriptions cascade from customers.
	_, err := h.db.Exec(ctx, `DELETE FROM customers WHERE phone_number = $1`, testPhone)
	return err
}

// -------------------------------------------------------------------------
// Cases
// -------------------------------------------------------------------------

// caseForgedSignature — a wrong signature must be rejected before anything is
// parsed or recorded. This is the only thing standing between a stranger with
// the URL and the ability to mark any invoice paid.
func (h *harness) caseForgedSignature() {
	const name = "forged_signature"
	eventID := eventIDPrefix + "forged"

	payload := linkPayload("payment_link.paid", "plink_forged", uuid.Must(uuid.NewV7()).String(), "pay_forged", 100000, 100000)
	code, _, err := h.post(payload, eventID, "deadbeef")
	if err != nil {
		h.fail(name, err.Error())
		return
	}

	h.expect(name+"/status", code == http.StatusUnauthorized, fmt.Sprintf("HTTP %d", code), "HTTP 401")
	// Nothing may be recorded — a rejected payload must not consume its own
	// event id, or a later legitimate redelivery would be swallowed as a
	// duplicate.
	h.expect(name+"/no_row", h.eventCount(eventID) == 0,
		fmt.Sprintf("%d payment_events rows", h.eventCount(eventID)), "0 rows")
}

// caseMissingEventID — without the header there is no idempotency key, so the
// handler must refuse rather than process an unidentifiable money event.
func (h *harness) caseMissingEventID() {
	const name = "missing_event_id"
	payload := linkPayload("payment_link.paid", "plink_noid", uuid.Must(uuid.NewV7()).String(), "pay_noid", 100000, 100000)
	code, _, err := h.post(payload, "", "")
	if err != nil {
		h.fail(name, err.Error())
		return
	}
	h.expect(name, code == http.StatusBadRequest, fmt.Sprintf("HTTP %d", code), "HTTP 400")
}

// caseHappyPath — the baseline. Returns the event id and invoice id so the
// duplicate case can replay against exactly this event.
func (h *harness) caseHappyPath() (eventID, invoiceID string) {
	const name = "happy_path"
	eventID = eventIDPrefix + "happy"

	invoiceID, err := h.newInvoice("2099-01", 4200.00, "PENDING", "plink_happy")
	if err != nil {
		h.fail(name, "fixture: "+err.Error())
		return "", ""
	}

	payload := linkPayload("payment_link.paid", "plink_happy", invoiceID, "pay_happy", 420000, 420000)
	code, _, err := h.post(payload, eventID, "")
	if err != nil {
		h.fail(name, err.Error())
		return "", ""
	}
	h.expect(name+"/status", code == http.StatusOK, fmt.Sprintf("HTTP %d", code), "HTTP 200")

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return eventID, invoiceID
	}
	h.expect(name+"/outcome", outcome == "APPLIED", outcome+" ("+note+")", "APPLIED")

	st, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/invoice", err.Error())
		return eventID, invoiceID
	}
	h.expect(name+"/invoice", st.status == "PAID_ONLINE", st.status, "PAID_ONLINE")
	h.expect(name+"/paid_at", st.paidAt != nil, fmt.Sprintf("paid_at=%v", st.paidAt), "non-null")
	h.expect(name+"/reference", st.ref != nil && *st.ref == "pay_happy",
		fmt.Sprintf("payment_reference=%v", derefOr(st.ref, "<nil>")), "pay_happy")

	return eventID, invoiceID
}

// caseDuplicateReplay — Razorpay documents that redelivery is expected. The
// same event id arriving twice must not touch the invoice a second time.
// Depends on caseHappyPath having already consumed this event id.
func (h *harness) caseDuplicateReplay(eventID, invoiceID string) {
	const name = "duplicate_replay"
	if eventID == "" || invoiceID == "" {
		h.fail(name, "skipped — happy_path did not complete")
		return
	}

	before, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name, err.Error())
		return
	}

	payload := linkPayload("payment_link.paid", "plink_happy", invoiceID, "pay_happy_REDELIVERED", 420000, 420000)
	code, body, err := h.post(payload, eventID, "")
	if err != nil {
		h.fail(name, err.Error())
		return
	}

	// 200 so Razorpay stops retrying, but the body distinguishes it.
	h.expect(name+"/status", code == http.StatusOK, fmt.Sprintf("HTTP %d", code), "HTTP 200")
	h.expect(name+"/body", strings.Contains(body, "duplicate"), body, `{"status":"duplicate"}`)
	h.expect(name+"/single_row", h.eventCount(eventID) == 1,
		fmt.Sprintf("%d rows", h.eventCount(eventID)), "exactly 1")

	// Give the (absent) goroutine a chance to misbehave before asserting.
	time.Sleep(500 * time.Millisecond)

	after, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/unchanged", err.Error())
		return
	}
	// payment_reference is the tell: if the duplicate were processed it would
	// be overwritten with pay_happy_REDELIVERED.
	unchanged := after.status == before.status &&
		derefOr(after.ref, "") == derefOr(before.ref, "") &&
		equalTime(after.paidAt, before.paidAt)
	h.expect(name+"/unchanged", unchanged,
		fmt.Sprintf("status=%s ref=%s", after.status, derefOr(after.ref, "<nil>")),
		fmt.Sprintf("status=%s ref=%s", before.status, derefOr(before.ref, "<nil>")))
}

// caseAlreadyPaidCash — the case worth having a tunnel-free harness for at all.
// Someone pays cash on the 3rd, the invoice is marked PAID_CASH, then they tap
// an old link on the 5th. That is real money owed back, not a status to flip.
func (h *harness) caseAlreadyPaidCash() {
	const name = "already_paid_cash"
	eventID := eventIDPrefix + "alreadypaid"

	invoiceID, err := h.newInvoice("2099-02", 5000.00, "PAID_CASH", "plink_cash")
	if err != nil {
		h.fail(name, "fixture: "+err.Error())
		return
	}

	payload := linkPayload("payment_link.paid", "plink_cash", invoiceID, "pay_double", 500000, 500000)
	if _, _, err := h.post(payload, eventID, ""); err != nil {
		h.fail(name, err.Error())
		return
	}

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return
	}
	h.expect(name+"/outcome", outcome == "ALREADY_PAID", outcome+" ("+note+")", "ALREADY_PAID")

	st, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/invoice", err.Error())
		return
	}
	// Must NOT have been rewritten to PAID_ONLINE — that would erase the fact
	// that the cash payment is the one that settled the bill.
	h.expect(name+"/not_overwritten", st.status == "PAID_CASH", st.status, "PAID_CASH")
	h.expect(name+"/no_reference", st.ref == nil,
		fmt.Sprintf("payment_reference=%v", derefOr(st.ref, "<nil>")), "<nil>")
}

// caseAmountMismatch — invoice recalculated after the link went out. The money
// is real, so mark it paid; but flag it so a human reconciles the difference.
func (h *harness) caseAmountMismatch() {
	const name = "amount_mismatch"
	eventID := eventIDPrefix + "mismatch"

	// Link was created at ₹4,200; invoice has since been regenerated to ₹4,080.
	invoiceID, err := h.newInvoice("2099-03", 4080.00, "PENDING", "plink_mismatch")
	if err != nil {
		h.fail(name, "fixture: "+err.Error())
		return
	}

	payload := linkPayload("payment_link.paid", "plink_mismatch", invoiceID, "pay_mismatch", 420000, 420000)
	if _, _, err := h.post(payload, eventID, ""); err != nil {
		h.fail(name, err.Error())
		return
	}

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return
	}
	h.expect(name+"/outcome", outcome == "AMOUNT_MISMATCH", outcome+" ("+note+")", "AMOUNT_MISMATCH")

	st, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/invoice", err.Error())
		return
	}
	// Paid anyway — withholding the status would chase a customer for a bill
	// they have settled.
	h.expect(name+"/still_paid", st.status == "PAID_ONLINE", st.status, "PAID_ONLINE")
}

// caseLinkIDFallback — no invoice_id in notes. Resolution falls through to
// payment_link_id, which is what makes a link created by hand in the Razorpay
// dashboard still reconcile.
func (h *harness) caseLinkIDFallback() {
	const name = "link_id_fallback"
	eventID := eventIDPrefix + "fallback"

	invoiceID, err := h.newInvoice("2099-04", 1500.00, "PENDING", "plink_fallback")
	if err != nil {
		h.fail(name, "fixture: "+err.Error())
		return
	}

	// invoiceID deliberately omitted from notes.
	payload := linkPayload("payment_link.paid", "plink_fallback", "", "pay_fallback", 150000, 150000)
	if _, _, err := h.post(payload, eventID, ""); err != nil {
		h.fail(name, err.Error())
		return
	}

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return
	}
	h.expect(name+"/outcome", outcome == "APPLIED", outcome+" ("+note+")", "APPLIED")

	st, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/invoice", err.Error())
		return
	}
	h.expect(name+"/invoice", st.status == "PAID_ONLINE", st.status, "PAID_ONLINE")
}

// caseUnmatched — money arrived that we cannot tie to an invoice. Must be
// recorded loudly rather than dropped; this is the one that becomes a customer
// insisting they paid while the system says otherwise.
func (h *harness) caseUnmatched() {
	const name = "unmatched"
	eventID := eventIDPrefix + "unmatched"

	orphan := uuid.Must(uuid.NewV7()).String()
	payload := linkPayload("payment_link.paid", "plink_nonexistent", orphan, "pay_orphan", 999900, 999900)
	if _, _, err := h.post(payload, eventID, ""); err != nil {
		h.fail(name, err.Error())
		return
	}

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return
	}
	h.expect(name+"/outcome", outcome == "UNMATCHED", outcome+" ("+note+")", "UNMATCHED")
}

// caseIgnoredEventType — payment.captured and friends describe the same rupees
// from a different angle. Acting on both would apply the same money twice.
func (h *harness) caseIgnoredEventType() {
	const name = "ignored_event_type"
	eventID := eventIDPrefix + "ignored"

	invoiceID, err := h.newInvoice("2099-05", 2000.00, "PENDING", "plink_ignored")
	if err != nil {
		h.fail(name, "fixture: "+err.Error())
		return
	}

	payload := linkPayload("payment.captured", "plink_ignored", invoiceID, "pay_captured", 200000, 200000)
	if _, _, err := h.post(payload, eventID, ""); err != nil {
		h.fail(name, err.Error())
		return
	}

	outcome, note, err := h.awaitOutcome(eventID)
	if err != nil {
		h.fail(name+"/outcome", err.Error())
		return
	}
	h.expect(name+"/outcome", outcome == "IGNORED", outcome+" ("+note+")", "IGNORED")

	st, err := h.invoice(invoiceID)
	if err != nil {
		h.fail(name+"/invoice", err.Error())
		return
	}
	h.expect(name+"/untouched", st.status == "PENDING", st.status, "PENDING")
}

// -------------------------------------------------------------------------
// Small helpers
// -------------------------------------------------------------------------

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// -------------------------------------------------------------------------
// Entry point
// -------------------------------------------------------------------------

func main() {
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		fmt.Println("Notice: no .env found, reading from environment")
	}

	secret := os.Getenv("RAZORPAY_WEBHOOK_SECRET")
	if secret == "" {
		exit("RAZORPAY_WEBHOOK_SECRET is not set.\n" +
			"validSignature returns false on an empty secret, so every case would 401 and tell you nothing.")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		exit("DATABASE_URL is not set.")
	}

	base := *flagBase
	if base == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "3000"
		}
		base = "http://localhost:" + port
	}
	base = strings.TrimRight(base, "/")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		exit("could not connect to database: " + err.Error())
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		exit("database ping failed: " + err.Error())
	}

	// Fail early and clearly if the server is not up, rather than reporting
	// nine identical connection-refused failures.
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/healthz"); err != nil {
		exit(fmt.Sprintf("server not reachable at %s: %v\nStart it with `go run .` first.", base, err))
	} else {
		resp.Body.Close()
	}

	h := &harness{db: db, base: base, secret: secret}

	fmt.Printf("\nRazorpay webhook replay — %s\n", base)
	fmt.Printf("Signing with RAZORPAY_WEBHOOK_SECRET (%d chars)\n\n", len(secret))

	if err := h.setup(); err != nil {
		exit("fixture setup failed: " + err.Error())
	}

	// NOT deferred: this function ends in os.Exit on failure, and deferred
	// calls do not run through os.Exit. Cleanup is invoked explicitly below,
	// on both the passing and failing path.
	teardown := func() {
		if *flagKeep {
			fmt.Printf("\n-keep: fixtures left in place. Customer phone %s, events LIKE '%s%%'.\n", testPhone, eventIDPrefix)
			return
		}
		if err := h.cleanup(); err != nil {
			fmt.Printf("\n⚠️  cleanup failed: %v\n", err)
			fmt.Printf("    Remove by hand: DELETE FROM payment_events WHERE event_id LIKE '%s%%';\n", eventIDPrefix)
			fmt.Printf("                    DELETE FROM customers WHERE phone_number = '%s';\n", testPhone)
		}
	}

	fmt.Println("Rejection paths")
	h.caseForgedSignature()
	h.caseMissingEventID()

	fmt.Println("\nApplication paths")
	eventID, invoiceID := h.caseHappyPath()
	h.caseLinkIDFallback()
	h.caseAmountMismatch()

	fmt.Println("\nProtection paths")
	h.caseDuplicateReplay(eventID, invoiceID)
	h.caseAlreadyPaidCash()
	h.caseUnmatched()
	h.caseIgnoredEventType()

	// Summary
	var failed int
	for _, r := range h.results {
		if !r.passed {
			failed++
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("─", 64))
	if failed == 0 {
		fmt.Printf("\033[32m%d/%d assertions passed.\033[0m\n", len(h.results), len(h.results))
	} else {
		fmt.Printf("\033[31m%d of %d assertions FAILED:\033[0m\n", failed, len(h.results))
		for _, r := range h.results {
			if !r.passed {
				fmt.Printf("  • %-24s %s\n", r.name, r.detail)
			}
		}
	}
	teardown()
	fmt.Println()

	if failed > 0 {
		db.Close()
		os.Exit(1)
	}
}

func exit(msg string) {
	fmt.Fprintf(os.Stderr, "\n%s\n\n", msg)
	os.Exit(1)
}
