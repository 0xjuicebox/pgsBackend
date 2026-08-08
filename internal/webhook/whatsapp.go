package webhook

import (
	"context"
	"encoding/json"
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
		// 1. CATCH FLOW SUBMISSIONS
		flowPayload := interactiveData
		if flowPayload == "" && strings.HasPrefix(body, "{") {
			flowPayload = body
		}
		if flowPayload != "" {
			wr.handleFlowSubmissions(rawSender, cleanPhone, flowPayload)
			return
		}

		// 2. THE GATEKEEPER (Now checks status!)
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

		// 3. REGISTERED USER ROUTING (Active & Disabled)
		switch {
		case buttonPayload == "1" || strings.Contains(message, "main menu"):
			err := wr.WhatsApp.SendInteractiveMenu(rawSender)
			if err != nil {
				fmt.Printf("❌ Failed to send menu: %v\n", err)
			}

		case message == "test" || message == "3" || strings.Contains(message, "report issue"):
			wr.handleIssueRequest(rawSender, cleanPhone)

		case message == "1" || strings.Contains(message, "override"):
			wr.handleOverrideRequest(rawSender, cleanPhone)

		case message == "2" || strings.Contains(message, "update_info") || strings.Contains(message, "update_info"):
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

// --- FLOW ROUTING (issue reporting only now) ---

// flowPage / flowItem mirror the nested shape a native WhatsApp Flow submission
// actually sends: {"pages":[{"pageId":"...","items":[{"label":"...","value":"..."}]}]}
type flowPage struct {
	PageID string     `json:"pageId"`
	Items  []flowItem `json:"items"`
}
type flowItem struct {
	Label string      `json:"label"`
	Value interface{} `json:"value"`
}
type flowSubmission struct {
	Pages []flowPage `json:"pages"`
}

func (wr *WhatsAppResource) handleFlowSubmissions(rawSender, cleanPhone, jsonBody string) {
	var submission flowSubmission
	if err := json.Unmarshal([]byte(jsonBody), &submission); err != nil {
		fmt.Printf("❌ Failed to parse Flow JSON: %v\n", err)
		return
	}

	// 🕵️ DEBUG: This prints the parsed structure Meta/Twilio sent back.
	fmt.Printf("📦 Flow Payload Received: %+v\n", submission)

	// Walk the nested pages/items structure into a clean, readable summary —
	// e.g. "Issue Type: damaged_item | Describe the Issue: dansger" instead
	// of a raw Go-map dump.
	var feedbackBuilder strings.Builder
	for _, page := range submission.Pages {
		for _, item := range page.Items {
			label := strings.ReplaceAll(item.Label, "_", " ")
			feedbackBuilder.WriteString(fmt.Sprintf("%s: %v | ", label, item.Value))
		}
	}
	complaintText := strings.TrimSuffix(feedbackBuilder.String(), " | ")

	if complaintText == "" {
		fmt.Printf("⚠️ Flow submission from %s had no parseable items — raw body: %s\n", cleanPhone, jsonBody)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		UPDATE delivery_logs
		SET is_flagged = true, customer_feedback = $1, updated_at = NOW()
		WHERE customer_id = (SELECT id FROM customers WHERE phone_number = $2)
		AND delivery_date = CURRENT_DATE;
	`
	cmdTag, err := wr.DB.Exec(ctx, query, complaintText, cleanPhone)

	if err != nil {
		fmt.Printf("❌ DB Error logging complaint for %s: %v\n", cleanPhone, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't log your issue right now. Please try again or contact support.")
		wr.WhatsApp.SendInteractiveMenu(rawSender)
		return
	}

	if cmdTag.RowsAffected() == 0 {
		fmt.Printf("⚠️ DB Warning: Complaint received for %s but no delivery log found today. Feedback: %q\n", cleanPhone, complaintText)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't find a delivery record for you today. Our team will look into this manually.")
		wr.WhatsApp.SendInteractiveMenu(rawSender)
		return
	}

	// ✅ Explicit success log for your terminal
	fmt.Printf("✅ DB Success: Issue logged for %s. Form Data: %q\n", cleanPhone, complaintText)

	// Send the issue confirmation template — it has its own "Main Menu" button (id "1"),
	// so we no longer need to separately fire SendDeliveryUpdate + SendInteractiveMenu here.
	err = wr.WhatsApp.SendIssueConfirmation(rawSender)
	if err != nil {
		fmt.Printf("❌ Failed to send issue confirmation: %v\n", err)
		// Fall back to the old text + menu combo if the template send fails for any reason.
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "✅ We have recorded your feedback and our management team will take action promptly.")
		wr.WhatsApp.SendInteractiveMenu(rawSender)
	}
}

// --- ACCOUNT MANAGEMENT ---

func (wr *WhatsAppResource) handleDisableAccount(rawSender, phone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := wr.DB.Exec(ctx, "UPDATE customers SET is_active = false, status = 'disabled' WHERE phone_number = $1", phone)
	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ We couldn't pause your account right now. Please try again later.")
		return
	}

	wr.WhatsApp.SendDeliveryUpdate(rawSender, "⏸️ Your account is now disabled. Deliveries will stop tomorrow.\n\nNote: You will still receive a final bill at the end of the month for deliveries already made.")
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
		wr.WhatsApp.SendDeliveryUpdate(rawSender, fmt.Sprintf("❌ We cannot delete your account because you have %d unbilled deliveries this month.\n\nPlease disable your account (Reply 'Pause account') to stop future deliveries. Once your final bill is settled, you can delete your account.", unbilledDeliveries))
		fmt.Printf("🚨 ADMIN ALERT: Customer %s tried to delete account but has pending payments.\n", phone)
		return
	}

	_, err = wr.DB.Exec(ctx, "DELETE FROM customers WHERE id = $1", customerID)
	if err != nil {
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Deletion failed. Please contact support.")
		return
	}

	wr.WhatsApp.SendDeliveryUpdate(rawSender, "✅ Your account and all associated data have been permanently deleted. Goodbye!")
}

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
