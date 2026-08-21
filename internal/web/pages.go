package web

// Public marketing and policy pages.
//
// WHY THESE EXIST
//
// Razorpay will not enable live API keys until a website has been submitted
// and approved — the "Generate Key" button stays disabled without one. For a
// business whose customers only ever use WhatsApp, that's an odd requirement,
// but it's a hard gate.
//
// A reviewer will open the submitted URL. Before this, the root path returned
// 404, which reads as a dead service and is a likely rejection.
//
// Reviewers also look for the standard commerce policies: what you're selling,
// how refunds work, how delivery works, how to reach a human, and what you do
// with personal data. Those live here as separate URLs rather than anchors on
// one page, because that's the form the review checklist expects.
//
// CONTENT IS READ FROM THE ENVIRONMENT
//
// Legal entity name, address, phone and email must be real and must match the
// KYC submission — a mismatch between the site and the registered business is
// one of the commonest rejection reasons. They're read from env rather than
// hardcoded so the same binary can serve a staging deploy without publishing
// a home address.
//
// Unset values render as an obvious placeholder and are reported at boot, so
// a half-filled site can't be submitted by accident.

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

//go:embed templates/site.html
var siteFS embed.FS
var siteTmpl = template.Must(template.ParseFS(siteFS, "templates/site.html"))

// BusinessInfo is the real-world identity shown on every page. Everything
// here appears publicly, and must match what was submitted for KYC.
type BusinessInfo struct {
	LegalName   string // registered entity, e.g. "PGS Dairy Enterprises"
	TradingName string // what customers call it, e.g. "PGS Direct"
	Address     string // full registered address, single line
	Phone       string // support number, E.164 or local format
	Email       string // support email
	ServiceArea string // where deliveries actually run, e.g. "Karnal, Haryana"
	Since       string // year established, for the footer
}

// LoadBusinessInfo reads the public business identity from the environment.
//
//	PGS_LEGAL_NAME, PGS_TRADING_NAME, PGS_ADDRESS,
//	PGS_PHONE, PGS_EMAIL, PGS_SERVICE_AREA, PGS_SINCE
func LoadBusinessInfo() BusinessInfo {
	return BusinessInfo{
		LegalName:   env("PGS_LEGAL_NAME", ""),
		TradingName: env("PGS_TRADING_NAME", "PGS Direct"),
		Address:     env("PGS_ADDRESS", ""),
		Phone:       env("PGS_PHONE", ""),
		Email:       env("PGS_EMAIL", ""),
		ServiceArea: env("PGS_SERVICE_AREA", ""),
		Since:       env("PGS_SINCE", ""),
	}
}

// Missing reports which required fields are unset, so main.go can warn at
// boot. Submitting the site to Razorpay with "[NOT SET]" on the contact page
// wastes a 48-hour review cycle.
func (b BusinessInfo) Missing() []string {
	var out []string
	for name, v := range map[string]string{
		"PGS_LEGAL_NAME":   b.LegalName,
		"PGS_ADDRESS":      b.Address,
		"PGS_PHONE":        b.Phone,
		"PGS_EMAIL":        b.Email,
		"PGS_SERVICE_AREA": b.ServiceArea,
	} {
		if strings.TrimSpace(v) == "" {
			out = append(out, name)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// display renders an unset field visibly rather than as an empty gap, so a
// missing value is obvious on the page instead of looking like a design
// choice.
func display(v string) string {
	if strings.TrimSpace(v) == "" {
		return "[NOT SET — see .env]"
	}
	return v
}

type Resource struct{ Info BusinessInfo }

// RegisterOn attaches the public pages to the main router:
//
//	web.Resource{Info: info}.RegisterOn(r)
//
// Deliberately NOT a Mount("/"). Mounting a subrouter at the root registers a
// catch-all wildcard, which chi rejects when other routes already exist at
// that level — and would make every unmatched path render the landing page
// rather than 404. Registering the six exact paths keeps the rest of the
// routing untouched.
//
// /delivery is a policy page here. The delivery API lives under a different
// mount and is unaffected, but the collision is worth knowing about: if an
// API route is ever added at exactly /delivery, this would shadow it.
func (wr Resource) RegisterOn(r chi.Router) {
	r.Get("/", wr.page("home"))
	r.Get("/terms", wr.page("terms"))
	r.Get("/privacy", wr.page("privacy"))
	r.Get("/refunds", wr.page("refunds"))
	r.Get("/delivery-policy", wr.page("delivery"))
	r.Get("/contact", wr.page("contact"))
}

type section struct {
	Heading string
	Body    []string // each entry is a paragraph
	Bullets []string
}

type pageData struct {
	Title       string
	Subtitle    string
	Sections    []section
	Info        BusinessInfo
	DisplayInfo map[string]string
	Year        int
	IsHome      bool
}

func (wr Resource) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := wr.build(name)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Short cache: a reviewer may load these repeatedly, but the content
		// changes whenever the env does.
		w.Header().Set("Cache-Control", "public, max-age=300")

		if err := siteTmpl.Execute(w, data); err != nil {
			fmt.Printf("⚠️ site page %q render failed: %v\n", name, err)
		}
	}
}

func (wr Resource) build(name string) pageData {
	b := wr.Info
	d := pageData{
		Info: b,
		Year: time.Now().Year(),
		DisplayInfo: map[string]string{
			"LegalName":   display(b.LegalName),
			"TradingName": display(b.TradingName),
			"Address":     display(b.Address),
			"Phone":       display(b.Phone),
			"Email":       display(b.Email),
			"ServiceArea": display(b.ServiceArea),
		},
	}

	switch name {
	case "home":
		d.IsHome = true
		d.Title = b.TradingName
		d.Subtitle = "Fresh dairy, delivered to your door twice a day."
		d.Sections = []section{
			{
				Heading: "What we do",
				Body: []string{
					fmt.Sprintf("%s delivers fresh milk and dairy products to homes in %s. We run two delivery rounds a day — one in the morning and one in the evening — and you choose the one that suits your household.",
						display(b.TradingName), display(b.ServiceArea)),
					"Everything is delivered on a monthly subscription. You tell us what you want each day, we deliver it, and you pay once a month for exactly what was delivered.",
				},
			},
			{
				Heading: "What we deliver",
				Bullets: []string{
					"Fresh cow milk",
					"Curd and buttermilk",
					"Paneer, butter and ghee",
					"Desi khand and jaggery",
					"Cold-pressed mustard oil",
					"Stone-ground atta",
					"Burfi",
				},
			},
			{
				Heading: "How it works",
				Bullets: []string{
					"Message us on WhatsApp to register. We'll send you a short form.",
					"Once approved, deliveries begin and you get a WhatsApp message after every delivery.",
					"Change tomorrow's order, pause for a holiday, or report a problem — all on WhatsApp.",
					"On the 1st of each month we send an itemised bill for the previous month, with a link to pay online. Cash is also accepted.",
				},
			},
			{
				Heading: "No app to install",
				Body: []string{
					"Everything happens over WhatsApp. There's nothing to download and no account to remember. Your phone number is your account.",
				},
			},
		}

	case "terms":
		d.Title = "Terms of Service"
		d.Sections = []section{
			{
				Heading: "Agreement",
				Body: []string{
					fmt.Sprintf("These terms govern the supply of dairy products by %s (\"we\", \"us\") to customers (\"you\") in %s. By registering for deliveries you accept these terms.",
						display(b.LegalName), display(b.ServiceArea)),
				},
			},
			{
				Heading: "Registration and eligibility",
				Body: []string{
					"Registration is by WhatsApp using the mobile number you contact us from. That number identifies your account. We may decline a registration where we cannot serve your address, and we will tell you the reason.",
					"You are responsible for keeping your delivery address and contact number accurate.",
				},
			},
			{
				Heading: "Orders and changes",
				Bullets: []string{
					"Your standing order is delivered on the days you select, in your chosen slot.",
					"You may change the order for a specific date up to the cutoff time for that slot. After the cutoff, that day's order is already being prepared and cannot be changed.",
					"Changes to your standing order are reviewed by us and take effect from the following day, because production is planned a day in advance.",
					"You may pause deliveries at any time. Pausing takes effect immediately.",
				},
			},
			{
				Heading: "Pricing",
				Body: []string{
					"Prices are per litre or per kilogram and are shown on your monthly bill. Price changes take effect from the 1st of the following month and never apply retrospectively to deliveries already made.",
				},
			},
			{
				Heading: "Billing and payment",
				Bullets: []string{
					"You are billed monthly in arrears for what was actually delivered.",
					"Bills are issued on the 1st of the month for the previous month.",
					"Payment is due by the 5th of the month.",
					"If a bill remains unpaid after the 5th, deliveries may be suspended until it is settled. Deliveries resume automatically once payment is received.",
				},
			},
			{
				Heading: "Quality and complaints",
				Body: []string{
					"If something is wrong with a delivery, tell us on WhatsApp. Where we agree there was a shortfall or a quality problem, we will correct your bill or arrange a replacement.",
				},
			},
			{
				Heading: "Ending your subscription",
				Body: []string{
					"You may stop deliveries at any time. Any amount owing for deliveries already made remains payable, and we will issue a final bill.",
				},
			},
			{
				Heading: "Liability",
				Body: []string{
					"Our liability in relation to any delivery is limited to the value of that delivery. Nothing in these terms limits liability that cannot be limited under applicable law.",
				},
			},
			{
				Heading: "Governing law",
				Body: []string{
					"These terms are governed by the laws of India. Disputes are subject to the jurisdiction of the courts where our registered office is located.",
				},
			},
		}

	case "privacy":
		d.Title = "Privacy Policy"
		d.Sections = []section{
			{
				Heading: "What we collect",
				Bullets: []string{
					"Your name and mobile number.",
					"Your delivery address and its approximate map location, so drivers can find you.",
					"Your order history and delivery records.",
					"Your billing and payment history.",
					"WhatsApp messages you send us in connection with your deliveries.",
				},
			},
			{
				Heading: "Why we collect it",
				Body: []string{
					"To deliver your order, to bill you accurately, to answer your questions, and to keep the records required of a business supplying food.",
				},
			},
			{
				Heading: "Who we share it with",
				Bullets: []string{
					"Our delivery drivers see your name, address and that day's order.",
					"Our payment provider, Razorpay, processes payments. We do not see or store your card or UPI credentials.",
					"WhatsApp and our messaging provider, Twilio, carry our messages to you.",
					"We do not sell your data, and we do not share it for advertising.",
				},
			},
			{
				Heading: "How long we keep it",
				Body: []string{
					"Delivery and billing records are kept while your account is active and afterwards for as long as required for tax and accounting purposes. You may ask us to close your account and delete your data once any outstanding balance is settled.",
				},
			},
			{
				Heading: "Your choices",
				Body: []string{
					fmt.Sprintf("You can ask us what we hold about you, correct it, or ask us to delete it. Message us on WhatsApp or email %s.", display(b.Email)),
				},
			},
			{
				Heading: "Security",
				Body: []string{
					"Data is stored on managed infrastructure with access limited to those who need it. Payments are handled entirely by our payment provider.",
				},
			},
		}

	case "refunds":
		d.Title = "Refund & Cancellation Policy"
		d.Sections = []section{
			{
				Heading: "Cancelling a delivery",
				Body: []string{
					"You can cancel or change any upcoming delivery on WhatsApp, free of charge, up to the cutoff time for that slot. A cancelled delivery is not charged.",
					"After the cutoff, that day's order has already been prepared and loaded, and cannot be cancelled.",
				},
			},
			{
				Heading: "If something is wrong with a delivery",
				Bullets: []string{
					"Tell us on WhatsApp on the day of delivery, or by 10:00 the next morning.",
					"If an item was missing, short, or not of acceptable quality, we correct your bill for that item.",
					"Where appropriate we will arrange a replacement on the next delivery instead.",
					"Corrections are applied to your monthly bill, so you are only ever charged for what you actually received.",
				},
			},
			{
				Heading: "Refunds",
				Body: []string{
					"Because you are billed monthly in arrears for deliveries already made, corrections are normally applied by reducing your bill rather than by refunding money.",
					"Where you have already paid and a correction is agreed afterwards, we will either credit the amount against your next bill or refund it to your original payment method, as you prefer.",
					"Refunds to the original payment method are processed within 5–7 working days of being agreed.",
				},
			},
			{
				Heading: "Duplicate payments",
				Body: []string{
					"If you pay a bill twice — for example in cash and then again online — we will refund the duplicate amount to your original payment method within 5–7 working days.",
				},
			},
			{
				Heading: "Cancelling your subscription",
				Body: []string{
					"You can stop deliveries at any time, with no cancellation fee. You remain liable for deliveries already made, and we will send a final bill.",
				},
			},
		}

	case "delivery":
		d.Title = "Delivery Policy"
		d.Sections = []section{
			{
				Heading: "Where we deliver",
				Body: []string{
					fmt.Sprintf("We deliver in %s, along fixed routes. If your address is not on one of our routes we will tell you when you register.", display(b.ServiceArea)),
				},
			},
			{
				Heading: "When we deliver",
				Bullets: []string{
					"Morning round: deliveries are made early in the morning.",
					"Evening round: deliveries are made in the evening.",
					"You choose one slot when you register.",
					"We deliver every day of the week, including on the days you select if you are on an alternate-day or custom schedule.",
				},
			},
			{
				Heading: "How delivery works",
				Body: []string{
					"Products are left at your door in the manner agreed when you register. You receive a WhatsApp message after each delivery listing exactly what was delivered.",
					"There is no delivery charge. The price you see is the price of the goods.",
				},
			},
			{
				Heading: "If we cannot deliver",
				Body: []string{
					"If we are unable to complete a delivery — because we cannot reach your address, or because of a problem with the round — we will message you and you will not be charged for that delivery.",
				},
			},
			{
				Heading: "Changing or pausing",
				Body: []string{
					"You can change an upcoming order or pause deliveries entirely at any time on WhatsApp. Changes for a given day must be made before that slot's cutoff time.",
				},
			},
		}

	case "contact":
		d.Title = "Contact Us"
		d.Sections = []section{
			{
				Heading: "Get in touch",
				Body: []string{
					"The quickest way to reach us is WhatsApp — it's the same number you use for your deliveries, and messages are answered by our team.",
				},
			},
			{
				Heading: "Details",
				Bullets: []string{
					"Business name: " + display(b.LegalName),
					"Trading as: " + display(b.TradingName),
					"Address: " + display(b.Address),
					"Phone / WhatsApp: " + display(b.Phone),
					"Email: " + display(b.Email),
					"Service area: " + display(b.ServiceArea),
				},
			},
			{
				Heading: "Hours",
				Body: []string{
					"Our delivery rounds run early morning and evening, every day. Office enquiries are answered during business hours, Monday to Saturday.",
				},
			},
		}
	}

	if d.Title == "" {
		d.Title = "Not found"
		d.Sections = []section{{
			Heading: "This page doesn't exist",
			Body:    []string{"Try the home page, or message us on WhatsApp."},
		}}
	}

	return d
}
