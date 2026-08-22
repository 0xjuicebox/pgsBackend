package webhook

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/registration"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type WhatsAppResource struct {
	WhatsApp *notification.WhatsAppService
	DB       *pgxpool.Pool
}

func (wr WhatsAppResource) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", wr.HandleIncomingMessage)
	return r
}

func (wr WhatsAppResource) HandleIncomingMessage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	rawSender := r.FormValue("From")
	cleanPhone := strings.TrimPrefix(rawSender, "whatsapp:")
	body := strings.TrimSpace(r.FormValue("Body"))
	buttonPayload := strings.TrimSpace(r.FormValue("ButtonPayload"))
	interactiveData := strings.TrimSpace(r.FormValue("InteractiveData"))
	message := strings.ToLower(body)

	fmt.Printf("📩 From %s | Body=%q | InteractiveData=%q | ButtonPayload=%q\n", cleanPhone, body, interactiveData, buttonPayload)
	w.WriteHeader(http.StatusOK)

	go func() {
		// NOTE: the old Flow-submission intercept used to sit here and
		// short-circuit before the gatekeeper. It was removed when issue
		// reporting moved to the token-gated HTML page (/delivery/issue).
		// Keeping it meant any InteractiveData — including ordinary menu
		// taps — bypassed routing entirely and got silently dropped when
		// the JSON didn't parse.

		// --- THE GATEKEEPER ---
		var customerID, status string
		var isActive bool
		err := wr.DB.QueryRow(context.Background(), "SELECT id, is_active, status FROM customers WHERE phone_number = $1", cleanPhone).Scan(&customerID, &isActive, &status)

		if err != nil {
			if err == pgx.ErrNoRows {
				wr.handleUnregisteredUser(rawSender, cleanPhone, message, buttonPayload)
				return
			}
			fmt.Printf("❌ Database error checking user: %v\n", err)
			return
		}

		// 🚀 INTERCEPT PENDING & REJECTED USERS
		if status == "pending" {
			wr.WhatsApp.SendPendingReviewMessage(rawSender)
			return
		}
		if status == "rejected" {
			wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Your subscription request was previously reviewed and could not be fulfilled. Please contact support if you believe this is an error.")
			return
		}

		// --- SUSPENDED USERS: unpaid bill, only payment lifts this ---
		//
		// Deliberately handled BEFORE the disabled branch, and deliberately
		// not sharing its RESUME path. A suspension exists because money is
		// owed; letting the customer type RESUME to lift it would make the
		// whole dunning cycle decorative — paused on the 6th, back on the
		// 7th, still unpaid.
		//
		// Payment is what resumes them, automatically, via
		// billing.ResumeIfSuspended on the OnPaid callback. So the only
		// useful thing to send here is the bill and a way to pay it.
		if status == "suspended" {
			wr.handleSuspendedMessage(rawSender, customerID)
			return
		}

		// --- DISABLED USERS: only reactivation or deletion make sense ---
		//
		// Previously a paused customer could still reach the override and
		// update flows, which quietly did nothing useful — their deliveries
		// were stopped either way. And there was no way back at all: once
		// paused, only an admin could restore the account. Now the resume
		// path is the default response for this state.
		if status == "disabled" {
			switch {
			case strings.Contains(message, "resume") || strings.Contains(message, "reactivate") ||
				strings.Contains(message, "restart") || strings.Contains(message, "unpause"):
				wr.handleReactivateAccount(rawSender, cleanPhone)
			case strings.Contains(message, "delete account"):
				wr.handleDeleteAccount(rawSender, customerID, cleanPhone)
			default:
				wr.WhatsApp.SendDeliveryUpdate(rawSender,
					"⏸️ Your deliveries are currently paused.\n\nReply *RESUME* to start them again, or *DELETE ACCOUNT* to close your account permanently.")
			}
			return
		}

		// --- ACTIVE USER ROUTING ---
		switch {
		case buttonPayload == "1" || strings.Contains(message, "main menu"):
			err := wr.WhatsApp.SendInteractiveMenu(rawSender)
			if err != nil {
				fmt.Printf("❌ Failed to send menu: %v\n", err)
			}

		case message == "3" || strings.Contains(message, "report issue"):
			wr.handleIssueRequest(rawSender, cleanPhone)

		case message == "1" || strings.Contains(message, "override"):
			wr.handleOverrideRequest(rawSender, cleanPhone)

		case message == "2" || strings.Contains(message, "update_info"):
			wr.handleUpdateRequest(rawSender, cleanPhone)

		case strings.Contains(message, "disable") || strings.Contains(message, "pause"):
			wr.handleDisableAccount(rawSender, cleanPhone)

		case strings.Contains(message, "delete account"):
			wr.handleDeleteAccount(rawSender, customerID, cleanPhone)

		default:
			err := wr.WhatsApp.SendInteractiveMenu(rawSender)
			if err != nil {
				fmt.Printf("❌ Failed to send interactive menu: %v\n", err)
			}
		}
	}()
}

// --- UNREGISTERED USER ROUTING ---

// handleUnregisteredUser is the ONLY logic path for phone numbers with no
// matching customer row. Every message gets the same registration prompt
// template, EXCEPT tapping/typing "register_and_subscribe", which generates
// a single-use registration link and sends it via a Call-to-Action template.
func (wr WhatsAppResource) handleUnregisteredUser(rawSender, cleanPhone, message, buttonPayload string) {
	if buttonPayload == "register_and_subscribe" || message == "register_and_subscribe" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		token, err := registration.GenerateToken(ctx, wr.DB, cleanPhone)
		if err != nil {
			fmt.Printf("❌ Failed to generate registration token for %s: %v\n", cleanPhone, err)
			wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Something went wrong generating your registration link. Please try again in a moment.")
			return
		}

		// The Twilio template's Website URL field already has the domain/path
		// baked in (https://.../register?token={{1}}) — we only pass the token.
		fmt.Printf("🚀 Sending registration link to %s (token=%s)\n", cleanPhone, token)
		err = wr.WhatsApp.SendRegistrationLink(rawSender, token)
		if err != nil {
			fmt.Printf("❌ Failed to send registration link: %v\n", err)
		}
		return
	}

	err := wr.WhatsApp.SendRegistrationPrompt(rawSender)
	if err != nil {
		fmt.Printf("❌ Failed to send registration prompt: %v\n", err)
	}
}

// --- ACCOUNT STATE CHANGES ---

func (wr *WhatsAppResource) handleDisableAccount(rawSender, phone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := wr.DB.Exec(ctx, "UPDATE customers SET is_active = false, status = 'disabled' WHERE phone_number = $1", phone)
	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't pause your account right now. Please try again later.")
		return
	}

	// "Starting tomorrow" was wrong: is_active is cleared here and the
	// manifest is generated live, so a customer pausing at 2pm is already off
	// that evening's van. Someone expecting one more delivery wouldn't get it.
	//
	// RESUME is described as a request, not a switch — it routes back through
	// admin approval, and promising an instant restart sets up the same
	// disappointment in the other direction.
	wr.WhatsApp.SendDeliveryUpdate(rawSender, "⏸️ Your deliveries are paused from now.\n\nYou'll still receive a bill at the end of the month for deliveries already made.\n\nReply *RESUME* when you'd like to start again — we'll confirm your first delivery date before deliveries restart.")
}

// handleReactivateAccount is the return path for a customer who paused
// themselves. Deliberately routes back through admin approval rather than
// flipping straight to active: while they were paused their route slot may
// have been reassigned, so someone has to confirm capacity before deliveries
// restart.
func (wr *WhatsAppResource) handleReactivateAccount(rawSender, phone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var subCount int
	if err := wr.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM subscriptions s
		JOIN customers c ON c.id = s.customer_id
		WHERE c.phone_number = $1
	`, phone).Scan(&subCount); err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't restart your deliveries right now. Please try again later.")
		return
	}

	if subCount == 0 {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We don't have an order on file for you any more. Reply *2* to set up your delivery again.")
		return
	}

	_, err := wr.DB.Exec(ctx,
		"UPDATE customers SET status = 'pending', is_active = false WHERE phone_number = $1", phone)
	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't restart your deliveries right now. Please try again later.")
		return
	}

	fmt.Printf("🔄 Reactivation requested by %s — now pending admin approval\n", phone)
	wr.WhatsApp.SendDeliveryUpdate(rawSender, "🔄 Welcome back! Your request to resume deliveries has been sent to our team.\n\nWe'll confirm as soon as your route is assigned. 🥛")
}

func (wr *WhatsAppResource) handleDeleteAccount(rawSender, customerID, phone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var unbilledDeliveries int
	query := `
		SELECT COUNT(*) FROM delivery_logs
		WHERE customer_id = $1
		AND status = 'DELIVERED'
		AND delivery_date >= date_trunc('month', CURRENT_DATE)
	`
	err := wr.DB.QueryRow(ctx, query, customerID).Scan(&unbilledDeliveries)

	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't process your request right now. Please try again later.")
		return
	}

	if unbilledDeliveries > 0 {
		// Stop the deliveries even though we're refusing the deletion.
		//
		// Previously this only said no. The customer had clearly asked to
		// leave, was told they couldn't, and kept receiving milk — accruing
		// more charges against the very balance blocking them. Telling
		// someone to reply PAUSE after they've already said DELETE ACCOUNT
		// asks them to act twice to achieve one thing.
		//
		// The debt is a reason to hold the account open, not a reason to keep
		// selling to them.
		wr.pauseForExit(ctx, customerID)

		wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf("⏸️ Your deliveries have been stopped.\n\nWe can't close your account yet — you have %d deliveries this month that haven't been billed. We'll send your final bill at the start of next month.\n\nOnce it's settled, reply *DELETE ACCOUNT* again and we'll close your account for good.", unbilledDeliveries))
		fmt.Printf("🚨 ADMIN ALERT: Customer %s asked to delete with %d unbilled deliveries — deliveries stopped, account held.\n", phone, unbilledDeliveries)
		return
	}

	// Also block on unpaid invoices from previous months — the same guard the
	// admin Delete endpoint applies. Without this a customer could settle
	// nothing and vanish between the 1st and the invoice run.
	var unpaidInvoices int
	if err := wr.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM invoices WHERE customer_id = $1 AND status = 'PENDING'`,
		customerID,
	).Scan(&unpaidInvoices); err == nil && unpaidInvoices > 0 {
		// Same reasoning as above: stop delivering, hold the account open for
		// the balance.
		wr.pauseForExit(ctx, customerID)

		wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf("⏸️ Your deliveries have been stopped.\n\nWe can't close your account yet — you have %d unpaid bill(s). Once they're settled, reply *DELETE ACCOUNT* again and we'll close your account for good.", unpaidInvoices))
		return
	}

	// order_overrides is the one foreign key into customers that does NOT
	// cascade (confdeltype 'a'). A bare DELETE therefore fails with a 23503
	// for any customer who has ever changed a single day's order — which the
	// customer experiences as "Deletion failed. Please contact support."
	//
	// Everything else — subscriptions, delivery_logs, invoices,
	// pending_subscription_changes — cascades.
	if _, err := wr.DB.Exec(ctx, `DELETE FROM order_overrides WHERE customer_id = $1`, customerID); err != nil {
		fmt.Printf("❌ delete: clearing overrides failed for %s: %v\n", customerID, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Deletion failed. Please reply here and our team will help.")
		return
	}

	_, err = wr.DB.Exec(ctx, "DELETE FROM customers WHERE id = $1", customerID)
	if err != nil {
		fmt.Printf("❌ delete: failed for %s: %v\n", customerID, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Deletion failed. Please reply here and our team will help.")
		return
	}

	wr.WhatsApp.SendDeliveryUpdate(rawSender, "✅ Your account and all associated data have been permanently deleted. Goodbye!")
}

// --- TOKEN-GATED WEB LINK HANDLERS ---

// handleOverrideRequest generates a secure token and sends the override web link
func (wr *WhatsAppResource) handleOverrideRequest(rawSender, cleanPhone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	token, err := registration.GenerateToken(ctx, wr.DB, cleanPhone)
	if err != nil {
		fmt.Printf("❌ Failed to generate override token for %s: %v\n", cleanPhone, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Something went wrong generating your link. Please try again in a moment.")
		return
	}

	fmt.Printf("🚀 Sending override link to %s (token=%s)\n", cleanPhone, token)
	err = wr.WhatsApp.SendOverrideLink(rawSender, token)
	if err != nil {
		fmt.Printf("❌ Failed to send override link: %v\n", err)
	}
}

// handleUpdateRequest generates a secure token and sends the update web link
func (wr *WhatsAppResource) handleUpdateRequest(rawSender, cleanPhone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	token, err := registration.GenerateToken(ctx, wr.DB, cleanPhone)
	if err != nil {
		fmt.Printf("❌ Failed to generate update token for %s: %v\n", cleanPhone, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Something went wrong generating your link. Please try again in a moment.")
		return
	}

	fmt.Printf("🚀 Sending update link to %s (token=%s)\n", cleanPhone, token)
	err = wr.WhatsApp.SendUpdateLink(rawSender, token)
	if err != nil {
		fmt.Printf("❌ Failed to send update link: %v\n", err)
	}
}

// NOTE: handleIssueRequest lives in internal/webhook/issue.go — it isn't
// duplicated here.

// handleSuspendedMessage replies to any message from a customer suspended for
// non-payment.
//
// Every inbound message gets the same answer regardless of content: here is
// what you owe, here is where to pay. There is no menu branch worth offering —
// overrides, order changes and issue reports are all meaningless while
// deliveries are stopped, and offering them would imply the account is
// functioning.
//
// The suspension lifts automatically when the invoice is paid (see
// billing.ResumeIfSuspended), so no admin step is mentioned and none is
// needed.
func (wr WhatsAppResource) handleSuspendedMessage(rawSender, customerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var (
		invoiceID string
		month     string
		total     float64
	)
	err := wr.DB.QueryRow(ctx, `
		SELECT i.id::text, i.billing_month, i.total_amount
		FROM customers c
		JOIN invoices i ON i.id = c.suspended_for_invoice_id
		WHERE c.id = $1::uuid
	`, customerID).Scan(&invoiceID, &month, &total)

	if err != nil {
		// Suspended but the invoice link is missing or broken. Rather than
		// leave the customer with no path forward, hand them to a human —
		// this is a state only an admin can untangle.
		fmt.Printf("⚠️ suspended customer %s has no resolvable invoice: %v\n", customerID, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender,
			"⏸️ Your deliveries are currently on hold because of an unpaid bill.\n\nPlease reply here and our team will help you sort it out.")
		return
	}

	base := os.Getenv("PUBLIC_BASE_URL")
	if base == "" {
		base = "https://pgsbackend-e4hiw.ondigitalocean.app"
	}

	wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf(
		"⏸️ Your deliveries are on hold.\n\nYour %s bill of *₹%.0f* is still unpaid.\n\n"+
			"Pay here and your deliveries resume automatically — nothing else needed:\n%s/pay/%s\n\n"+
			"If you'd like to discuss this, just reply and our team will help.",
		prettyMonthLabel(month), total, strings.TrimRight(base, "/"), invoiceID,
	))
}

// prettyMonthLabel turns "2026-07" into "July 2026".
func prettyMonthLabel(m string) string {
	if t, err := time.Parse("2006-01", m); err == nil {
		return t.Format("January 2006")
	}
	return m
}

// pauseForExit stops deliveries for a customer who has asked to leave but
// can't be deleted yet because they owe money.
//
// Only touches an account that's currently active. A customer already
// suspended for non-payment must stay suspended — downgrading them to
// 'disabled' would let them lift it themselves with RESUME, which is exactly
// what the suspension exists to prevent.
func (wr *WhatsAppResource) pauseForExit(ctx context.Context, customerID string) {
	if _, err := wr.DB.Exec(ctx, `
		UPDATE customers
		SET is_active = false, status = 'disabled'
		WHERE id = $1 AND status = 'active'
	`, customerID); err != nil {
		fmt.Printf("⚠️ pauseForExit: couldn't stop deliveries for %s: %v\n", customerID, err)
	}
}
