package webhook

import (
	"context"
	"fmt"
	"net/http"
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

	wr.WhatsApp.SendDeliveryUpdate(rawSender, "⏸️ Your deliveries are now paused starting tomorrow.\n\nYou'll still receive a final bill at the end of the month for deliveries already made.\n\nReply *RESUME* any time to start again.")
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
		wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf("❌ We cannot delete your account because you have %d unbilled deliveries this month.\n\nPlease pause your account (reply *PAUSE*) to stop future deliveries. Once your final bill is settled, you can delete your account.", unbilledDeliveries))
		fmt.Printf("🚨 ADMIN ALERT: Customer %s tried to delete account but has pending payments.\n", phone)
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
		wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf("❌ We cannot delete your account — you have %d unpaid bill(s) outstanding.\n\nPlease settle them first, then try again.", unpaidInvoices))
		return
	}

	_, err = wr.DB.Exec(ctx, "DELETE FROM customers WHERE id = $1", customerID)
	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Deletion failed. Please contact support.")
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
