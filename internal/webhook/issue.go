package webhook

// The customer-facing "Report an issue" flow. Called from the message router
// in whatsapp.go when the user taps option 3 / says "report issue".
//
// Same pattern as handleOverrideRequest and handleUpdateRequest: generate a
// short-lived token bound to the caller's phone, then send them a link to
// the token-gated HTML page (served by delivery.ShowIssueForm at
// /delivery/issue?token=...).

import (
	"context"
	"fmt"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/registration"
)

func (wr *WhatsAppResource) handleIssueRequest(rawSender, cleanPhone string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	token, err := registration.GenerateToken(ctx, wr.DB, cleanPhone)
	if err != nil {
		fmt.Printf("❌ Failed to generate issue token for %s: %v\n", cleanPhone, err)
		wr.WhatsApp.SendDeliveryUpdate(rawSender, "⚠️ Something went wrong generating your link. Please try again in a moment.")
		return
	}

	fmt.Printf("🚀 Sending issue-report link to %s (token=%s)\n", cleanPhone, token)
	if err := wr.WhatsApp.SendIssueLink(rawSender, token); err != nil {
		fmt.Printf("❌ Failed to send issue link: %v\n", err)
	}
}
