package notification

import (
	"fmt"
	"os"
	"strings"

	"github.com/twilio/twilio-go"
	openapi "github.com/twilio/twilio-go/rest/api/v2010"
)

type WhatsAppService struct {
	client    *twilio.RestClient
	fromPhone string
}

func NewWhatsAppService() *WhatsAppService {
	return &WhatsAppService{
		client:    twilio.NewRestClient(),
		fromPhone: os.Getenv("TWILIO_WHATSAPP_NUMBER"),
	}
}

// SendUpdateLink sends a Call-to-Action template for the update-details page.
func (s *WhatsAppService) SendUpdateLink(toPhone, token string) error {
	variables := fmt.Sprintf(`{"1":"%s"}`, token)
	// ⚠️ Replace with your actual Update Link Template SID from Twilio!
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_UPDATE_LINK", "HX306ea9cbf65027086120b77ccdfae302"), variables)
}

// formatPhone ensures the number is perfectly formatted for Twilio's WhatsApp API
func (s *WhatsAppService) formatPhone(phone string) string {
	phone = strings.ReplaceAll(phone, " ", "")
	phone = strings.TrimPrefix(phone, "whatsapp:")
	if !strings.HasPrefix(phone, "+") {
		phone = "+91" + phone
	}
	return "whatsapp:" + phone
}

// SendDeliveryUpdate is used to send standard text alerts to customers via Twilio
func (s *WhatsAppService) SendDeliveryUpdate(toPhone string, messageBody string) error {
	toPhone = s.formatPhone(toPhone)
	fromPhone := s.formatPhone(s.fromPhone)

	params := &openapi.CreateMessageParams{}
	params.SetTo(toPhone)
	params.SetFrom(fromPhone)
	params.SetBody(messageBody)

	resp, err := s.client.Api.CreateMessage(params)
	if err != nil {
		return fmt.Errorf("twilio text failure: %w", err)
	}

	if resp.Sid != nil {
		fmt.Printf("✅ WhatsApp text sent! SID: %s\n", *resp.Sid)
	}
	return nil
}

// SendPendingReviewMessage loops a waiting message if a user texts before approval
func (s *WhatsAppService) SendPendingReviewMessage(toPhone string) error {
	msg := "⏳ Thanks for registering! Our team is currently reviewing your details. We will notify you right here as soon as your delivery route is confirmed."
	return s.SendDeliveryUpdate(toPhone, msg)
}

// SendApprovalNotification tells a newly-approved customer that admin has
// reviewed their registration, assigned them to a route, and that deliveries
// will begin soon. Called automatically by CustomerResource.Approve.
func (s *WhatsAppService) SendApprovalNotification(toPhone, customerName string) error {
	msg := fmt.Sprintf(
		"🎉 Great news, %s! Your PGS Direct registration has been approved and you've been added to our delivery route.\n\nWe'll notify you once your first delivery is scheduled. Welcome aboard! 🥛",
		customerName,
	)
	return s.SendDeliveryUpdate(toPhone, msg)
}

// SendRejectionNotification tells a customer their registration was not
// approved. reason is optional — if empty, the message omits that line.
func (s *WhatsAppService) SendRejectionNotification(toPhone, customerName, reason string) error {
	msg := fmt.Sprintf(
		"We're sorry, %s — we're unable to onboard you as a PGS Direct customer at this time.",
		customerName,
	)
	if reason != "" {
		msg += fmt.Sprintf("\n\nReason: %s", reason)
	}
	msg += "\n\nIf you believe this is a mistake, please contact our support team directly."
	return s.SendDeliveryUpdate(toPhone, msg)
}

// SendOverrideLink sends a "Visit Website" (Call-to-Action) template for the
// order-override page. Same pattern as SendRegistrationLink: the template's
// Website URL field has the domain/path baked in with {{1}} as the token
// suffix (e.g. https://yourdomain.com/override?token={{1}}) — configure this
// as a separate Twilio template from the registration one.
func (s *WhatsAppService) SendOverrideLink(toPhone, token string) error {
	variables := fmt.Sprintf(`{"1":"%s"}`, token)
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_OVERRIDE_LINK", "HX40cbc193ae82c16ffcc1d47a31dda48c"), variables)
}

func (s *WhatsAppService) SendIssueLink(toPhone, token string) error {
	variables := fmt.Sprintf(`{"1":"%s"}`, token)
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_ISSUE_LINK", "HX08111bb54337d6ef74dadf404e9f2402"), variables)
}

// -------------------------------------------------------------------------
// Twilio Template functions
// -------------------------------------------------------------------------

func (s *WhatsAppService) SendIssueFlow(toPhone string) error {
	// Pass an empty string because there are no variables
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_ISSUE_FLOW", "HX043303525ae32a481156c55240783249"), "")
}

// SendIssueConfirmation sends the "issue received" confirmation template,
// which includes a quick-reply button (id "1") that routes back to the main menu.
func (s *WhatsAppService) SendIssueConfirmation(toPhone string) error {
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_ISSUE_CONFIRM", "HX7bad95c3f5cf029fbf1124d634c6d2f7"), "")
}

// SendRegistrationPrompt is sent to ANY unregistered number, regardless of what
// they typed. It contains a "Register" button.
func (s *WhatsAppService) SendRegistrationPrompt(toPhone string) error {
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_REG_PROMPT", "HX27a35ca1c882c0a76966bd774935b79a"), "")
}

// SendRegistrationLink sends the registration link as plain text.
//
// # WHY NOT A TEMPLATE WITH A BUTTON
//
// This used to be a Call-to-Action template. In practice the button often
// arrived rendered as static text — WhatsApp paints the message before the
// interactive layer has hydrated, and it only becomes tappable after the
// customer closes and reopens the chat.
//
// That is a client-side quirk, not something the server can fix. But it lands
// on the very first thing a new customer ever does, and a signup link that
// does nothing when tapped is a customer lost before they started.
//
// A bare URL in a message body has no interactive layer to fail. WhatsApp
// auto-links it and it works on first tap, every time.
//
// The 24-hour window doesn't apply: this is always a reply, sent seconds after
// the customer messaged us or tapped Register, so free-form is permitted.
//
// The base URL comes from PUBLIC_BASE_URL — the old template had an ngrok
// domain baked into it, which stops working the moment that tunnel dies.
func (s *WhatsAppService) SendRegistrationLink(toPhone, token string) error {
	link := fmt.Sprintf("%s/register?token=%s", publicBaseURL(), token)

	body := fmt.Sprintf(
		"🥛 *Welcome to PGS Direct!*\n\n"+
			"Tap the link below to set up your deliveries — it takes about a minute.\n\n"+
			"%s\n\n"+
			"_This link works for the next 30 minutes. Reply here if you need a new one._",
		link,
	)

	return s.SendDeliveryUpdate(toPhone, body)
}

// publicBaseURL is where the customer-facing pages live.
//
// Read from the environment so a staging deploy doesn't hand out production
// links, and so this can never again be a tunnel URL frozen into an approved
// Meta template that takes two days to change.
func publicBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://pgsbackend-e4hiw.ondigitalocean.app"
}

func (s *WhatsAppService) SendContentTemplate(toPhone string, templateSid string, variables string) error {
	toPhone = s.formatPhone(toPhone)
	fromPhone := s.formatPhone(s.fromPhone)

	params := &openapi.CreateMessageParams{}
	params.SetTo(toPhone)
	params.SetFrom(fromPhone)
	params.SetContentSid(templateSid)

	// Keep your original check: only set variables if they actually exist
	if variables != "" && variables != "{}" {
		params.SetContentVariables(variables)
	}

	resp, err := s.client.Api.CreateMessage(params)
	if err != nil {
		return fmt.Errorf("twilio template failure: %w", err)
	}

	if resp.Sid != nil {
		fmt.Printf("✅ WhatsApp Template sent! SID: %s\n", *resp.Sid)
	}
	return nil
}

func (s *WhatsAppService) SendInteractiveMenu(toPhone string) error {
	return s.SendContentTemplate(toPhone, templateSID("WHATSAPP_TEMPLATE_MENU", "HXd4d41a71bcc7f287b2a8dfffa83f649e"), "")
}

// templateSID resolves a content template SID from the environment, falling
// back to the value the code shipped with.
//
// # WHY THE FALLBACK
//
// These eight SIDs were hardcoded. Swapping a template — because Meta made you
// re-approve it, or because you built a better version — meant a code change,
// a commit and a redeploy, for what is really a configuration value.
//
// The fallback matters as much as the lookup: a deployment that forgets one of
// these env vars keeps working on the old template rather than silently
// sending nothing. Losing a message is worse than sending a slightly stale
// one, particularly for registration, which is the first thing a customer
// ever receives.
func templateSID(envKey, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return fallback
}
