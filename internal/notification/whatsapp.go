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
	return s.SendContentTemplate(toPhone, "HX306ea9cbf65027086120b77ccdfae302", variables)
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
	return s.SendContentTemplate(toPhone, "HX40cbc193ae82c16ffcc1d47a31dda48c", variables)
}

func (s *WhatsAppService) SendIssueLink(toPhone, token string) error {
	variables := fmt.Sprintf(`{"1":"%s"}`, token)
	return s.SendContentTemplate(toPhone, "HX08111bb54337d6ef74dadf404e9f2402", variables)
}

// -------------------------------------------------------------------------
// Twilio Template functions
// -------------------------------------------------------------------------

func (s *WhatsAppService) SendIssueFlow(toPhone string) error {
	// Pass an empty string because there are no variables
	return s.SendContentTemplate(toPhone, "HX043303525ae32a481156c55240783249", "")
}

// SendIssueConfirmation sends the "issue received" confirmation template,
// which includes a quick-reply button (id "1") that routes back to the main menu.
func (s *WhatsAppService) SendIssueConfirmation(toPhone string) error {
	return s.SendContentTemplate(toPhone, "HX7bad95c3f5cf029fbf1124d634c6d2f7", "")
}

// SendRegistrationPrompt is sent to ANY unregistered number, regardless of what
// they typed. It contains a "Register" button.
func (s *WhatsAppService) SendRegistrationPrompt(toPhone string) error {
	return s.SendContentTemplate(toPhone, "HX27a35ca1c882c0a76966bd774935b79a", "")
}

// SendRegistrationLink sends a "Visit Website" (Call-to-Action) template.
// The template's Website URL field is configured in Twilio as:
//
//	https://babbling-dynasty-scrunch.ngrok-free.dev/register?token={{1}}
//
// (Twilio requires a valid domain in the field itself — {{1}} can only be a
// suffix, not the whole URL — so we pass just the raw token here, not a full link.)
func (s *WhatsAppService) SendRegistrationLink(toPhone, token string) error {
	variables := fmt.Sprintf(`{"1":"%s"}`, token)
	return s.SendContentTemplate(toPhone, "HX69072c45c623b99ebf805e81d2ef5ac9", variables)
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
	return s.SendContentTemplate(toPhone, "HXd4d41a71bcc7f287b2a8dfffa83f649e", "")
}
