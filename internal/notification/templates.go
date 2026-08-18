package notification

// Business-initiated WhatsApp templates.
//
// WHY THESE EXIST SEPARATELY FROM SendDeliveryUpdate
//
// WhatsApp permits free-form text only within 24 hours of the customer's last
// inbound message. Outside that window Twilio rejects the send with error
// 63016 and nothing reaches the customer.
//
// Every message in this file is one we need to deliver to someone who has NOT
// just messaged us — a bill, a payment reminder, a suspension notice, a
// delivery confirmation, a receipt. Those are exactly the sends that fail
// silently under the free-form rule, and the failure is invisible during
// development because a developer testing the bot is always inside the window.
//
// SIDs are read from the environment rather than hardcoded so that a template
// can be re-approved or swapped without a redeploy, and so staging and
// production can point at different templates. Missing SIDs are reported by
// TemplateHealth at boot rather than discovered during a month-end run.
//
// VARIABLE CONTRACTS
//
// Meta rejects any parameter containing a newline, tab, or a run of four or
// more spaces. Every value below goes through sanitizeTemplateVar. This is
// why itemised bills live on the /pay page instead of in the message: a
// multi-line breakdown can never be a template variable. A single
// comma-separated line, however, is fine — which is how the delivery
// confirmation keeps its item list.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// TemplateSIDs holds every content template the system sends. Populated from
// the environment at construction.
type TemplateSIDs struct {
	Bill            string // bill ready:      {{1}} name {{2}} month {{3}} total {{4}} invoiceID
	PaymentReminder string // unpaid reminder: {{1}} name {{2}} month {{3}} total {{4}} invoiceID
	Suspension      string // account paused:  {{1}} name {{2}} month {{3}} total {{4}} invoiceID
	DeliveryDone    string // delivered:       {{1}} slot {{2}} items (single line)
	PaymentReceipt  string // payment landed:  {{1}} name {{2}} amount {{3}} month
}

// LoadTemplateSIDs reads the SIDs from the environment.
//
//	WHATSAPP_TEMPLATE_BILL
//	WHATSAPP_TEMPLATE_REMINDER
//	WHATSAPP_TEMPLATE_SUSPENSION
//	WHATSAPP_TEMPLATE_DELIVERY
//	WHATSAPP_TEMPLATE_RECEIPT
func LoadTemplateSIDs() TemplateSIDs {
	return TemplateSIDs{
		Bill:            os.Getenv("WHATSAPP_TEMPLATE_BILL"),
		PaymentReminder: os.Getenv("WHATSAPP_TEMPLATE_REMINDER"),
		Suspension:      os.Getenv("WHATSAPP_TEMPLATE_SUSPENSION"),
		DeliveryDone:    os.Getenv("WHATSAPP_TEMPLATE_DELIVERY"),
		PaymentReceipt:  os.Getenv("WHATSAPP_TEMPLATE_RECEIPT"),
	}
}

// TemplateHealth returns the names of any templates that aren't configured.
// Call at boot and log the result: an unset SID means that whole category of
// message silently stops reaching quiet customers, which is the kind of
// failure that surfaces weeks later as "nobody paid this month".
func (t TemplateSIDs) TemplateHealth() []string {
	var missing []string
	for name, sid := range map[string]string{
		"bill":       t.Bill,
		"reminder":   t.PaymentReminder,
		"suspension": t.Suspension,
		"delivery":   t.DeliveryDone,
		"receipt":    t.PaymentReceipt,
	} {
		if sid == "" || strings.HasPrefix(sid, "REPLACE_WITH") {
			missing = append(missing, name)
		}
	}
	return missing
}

// -------------------------------------------------------------------------
// Senders
// -------------------------------------------------------------------------

// SendBill — the month-end bill.
//
// total is pre-formatted with Indian digit grouping and no rupee symbol; the
// symbol is baked into the approved template body. Passing "₹5,120" here
// would render "₹₹5,120".
func (s *WhatsAppService) SendBill(toPhone, sid, name, month, total, invoiceID string) error {
	return s.sendBillingTemplate(toPhone, sid, "bill", name, month, total, invoiceID)
}

// SendPaymentReminder — daily nudge between the 1st and the 5th.
func (s *WhatsAppService) SendPaymentReminder(toPhone, sid, name, month, total, invoiceID string) error {
	return s.sendBillingTemplate(toPhone, sid, "reminder", name, month, total, invoiceID)
}

// SendSuspensionNotice — deliveries paused for non-payment. Carries the same
// pay link, because paying is what lifts the suspension.
func (s *WhatsAppService) SendSuspensionNotice(toPhone, sid, name, month, total, invoiceID string) error {
	return s.sendBillingTemplate(toPhone, sid, "suspension", name, month, total, invoiceID)
}

// sendBillingTemplate is shared by the three templates above — they differ
// only in approved copy, not in shape. Keeping one implementation means the
// variable contract can't drift between them, which matters because a
// mismatch produces an opaque Twilio error rather than a wrong message.
func (s *WhatsAppService) sendBillingTemplate(toPhone, sid, kind, name, month, total, invoiceID string) error {
	if !templateConfigured(sid) {
		return fmt.Errorf("%s template SID not configured", kind)
	}
	vars, err := json.Marshal(map[string]string{
		"1": sanitizeTemplateVar(firstName(name)),
		"2": sanitizeTemplateVar(month),
		"3": sanitizeTemplateVar(total),
		"4": sanitizeTemplateVar(invoiceID),
	})
	if err != nil {
		return fmt.Errorf("marshalling %s template variables: %w", kind, err)
	}
	return s.SendContentTemplate(toPhone, sid, string(vars))
}

// SendDeliveryComplete — post-delivery confirmation.
//
// items must be a SINGLE line, comma separated: "Milk 2 L, Curd 500 g".
// Multi-line breaks Meta's parameter rules. Build it with JoinItems.
//
// slot should already be capitalised ("Morning" / "Evening").
func (s *WhatsAppService) SendDeliveryComplete(toPhone, sid, slot, items string) error {
	if !templateConfigured(sid) {
		return fmt.Errorf("delivery template SID not configured")
	}
	vars, err := json.Marshal(map[string]string{
		"1": sanitizeTemplateVar(slot),
		"2": sanitizeTemplateVar(items),
	})
	if err != nil {
		return fmt.Errorf("marshalling delivery template variables: %w", err)
	}
	return s.SendContentTemplate(toPhone, sid, string(vars))
}

// SendPaymentReceipt — confirmation that money landed.
//
// amount is pre-formatted without the rupee symbol, same as the bill.
func (s *WhatsAppService) SendPaymentReceipt(toPhone, sid, name, amount, month string) error {
	if !templateConfigured(sid) {
		return fmt.Errorf("receipt template SID not configured")
	}
	vars, err := json.Marshal(map[string]string{
		"1": sanitizeTemplateVar(firstName(name)),
		"2": sanitizeTemplateVar(amount),
		"3": sanitizeTemplateVar(month),
	})
	if err != nil {
		return fmt.Errorf("marshalling receipt template variables: %w", err)
	}
	return s.SendContentTemplate(toPhone, sid, string(vars))
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

// JoinItems renders a product list as one comma-separated line suitable for a
// template variable: "Milk 2 L, Curd 500 g, Desi Khand 250 g".
//
// Deliberately not newline-separated. The bulleted multi-line format used by
// the old free-form message cannot be a template parameter — Meta rejects it.
func JoinItems(parts []string) string {
	return strings.Join(parts, ", ")
}

// firstName trims a full name down to what a greeting should use. "Aryaman
// Sharma" -> "Aryaman". Keeps messages from reading like form letters, and
// shortens a variable that Meta counts toward template length limits.
func firstName(full string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		return "there"
	}
	if i := strings.IndexByte(full, ' '); i > 0 {
		return full[:i]
	}
	return full
}

func templateConfigured(sid string) bool {
	return sid != "" && !strings.HasPrefix(sid, "REPLACE_WITH")
}

// sanitizeTemplateVar strips the characters Meta rejects in template
// parameters. A rejected send returns an opaque Twilio error, so flattening
// the value beats discovering the problem during a month-end run.
func sanitizeTemplateVar(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\t", " ")
	for strings.Contains(v, "  ") {
		v = strings.ReplaceAll(v, "  ", " ")
	}
	return strings.TrimSpace(v)
}
