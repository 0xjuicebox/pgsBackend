package payment

// The hosted bill page at GET /pay/{invoiceID}.
//
// WHY THIS EXISTS RATHER THAN PUTTING THE RAZORPAY URL IN THE MESSAGE
//
// Bills have to reach customers who haven't messaged us in weeks, which means
// an approved WhatsApp template rather than free-form text. Templates impose
// two constraints that a raw Razorpay link can't satisfy:
//
//   1. Template variables cannot contain newlines, tabs, or runs of 4+ spaces.
//      The itemised breakdown is one line per product, so it can never be a
//      template parameter. It has to live on a page.
//
//   2. A URL button's variable can only be appended to a base URL fixed at
//      approval time. Razorpay's short_url changes whenever a link is
//      cancelled and reissued — which CreateLinkForInvoice does every time an
//      invoice amount is corrected. Embedding it would mean re-approving the
//      template, or sending a dead link.
//
// So the template carries a stable URL of ours with the invoice id appended,
// and everything volatile is resolved server-side at open time. Corrections,
// expiries and reissues all become invisible to the template.
//
// The page also self-heals expired links: Razorpay links expire after 30 days,
// and CreateLinkForInvoice reissues when the amount has moved, so opening the
// page always produces a payable link or a clear reason why not.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

//go:embed templates/pay.html
var payTemplateFS embed.FS
var payTmpl = template.Must(template.ParseFS(payTemplateFS, "templates/pay.html"))

// Product ordering and labels are duplicated from billing rather than imported,
// because billing imports payment (for BillingResource.Payment) and importing
// back would be a cycle. Eleven strings is a cheaper price than an interface.
var payProducts = []string{
	"milk", "curd", "butter", "ghee", "lassi", "paneer",
	"jaggery", "khand", "oil", "atta", "burfi",
}

var payProductLabels = map[string]string{
	"milk": "Milk", "curd": "Curd", "butter": "Butter", "ghee": "Ghee",
	"lassi": "Buttermilk", "paneer": "Paneer", "jaggery": "Jaggery",
	"khand": "Desi Khand", "oil": "Mustard Oil", "atta": "Atta", "burfi": "Burfi",
}

var payLitreProducts = map[string]bool{"milk": true, "curd": true, "lassi": true, "oil": true}

// formatPayQty mirrors delivery.formatQty exactly: 500 -> "500 ml",
// 2000 -> "2 L". The customer sees this number in the WhatsApp delivery
// confirmation too, so the two must agree or the bill looks wrong.
func formatPayQty(v int, product string) string {
	litre := payLitreProducts[product]
	if v < 1000 {
		if litre {
			return fmt.Sprintf("%d ml", v)
		}
		return fmt.Sprintf("%d g", v)
	}
	val := float64(v) / 1000
	unit := "kg"
	if litre {
		unit = "L"
	}
	return strings.TrimSuffix(fmt.Sprintf("%.2f", val), ".00") + " " + unit
}

// PayPageRoutes is mounted at /pay in main.go:
//
//	r.Mount("/pay", pay.PayPageRoutes())
//
// Deliberately not part of Service.Routes() — that's mounted under /payment
// and holds the webhook. This is a customer-facing page and belongs at a short
// URL, because it goes inside a WhatsApp button.
func (s *Service) PayPageRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{invoiceID}", s.ServePayPage)
	return r
}

type payLine struct {
	Label    string
	Quantity string
	Rate     string
	Total    string
}

type payPageData struct {
	// Error, when set, replaces the whole bill view. Everything below is
	// ignored in that case.
	Error string

	CustomerName string
	Month        string
	Lines        []payLine
	Total        string

	// Paid short-circuits the pay button and shows a receipt instead.
	Paid       bool
	PaidMethod string

	// PayURL is empty when a link couldn't be created. The page then shows
	// manual instructions rather than a dead button — same fallback the
	// WhatsApp bill uses when Razorpay is unreachable.
	PayURL string
}

func (s *Service) ServePayPage(w http.ResponseWriter, r *http.Request) {
	invoiceID := chi.URLParam(r, "invoiceID")

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	data, status := s.buildPayPage(ctx, invoiceID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache: the amount can change after a correction, and a cached
	// "already paid" page would be actively misleading.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.WriteHeader(status)

	if err := payTmpl.Execute(w, data); err != nil {
		fmt.Printf("⚠️ pay page render failed for invoice %s: %v\n", invoiceID, err)
	}
}

func (s *Service) buildPayPage(ctx context.Context, invoiceID string) (payPageData, int) {
	var (
		custName     string
		month        string
		status       string
		total        float64
		breakdownRaw []byte
	)

	err := s.DB.QueryRow(ctx, `
		SELECT c.name, i.billing_month, i.status, i.total_amount, i.breakdown
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1::uuid
	`, invoiceID).Scan(&custName, &month, &status, &total, &breakdownRaw)

	if err == pgx.ErrNoRows {
		return payPageData{
			Error: "We couldn't find this bill. It may have been cancelled — please reply on WhatsApp and our team will help.",
		}, http.StatusNotFound
	}
	if err != nil {
		// Includes a malformed UUID in the URL, which surfaces here as a cast
		// error rather than ErrNoRows. Same message either way; the customer
		// can't act on the difference.
		fmt.Printf("⚠️ pay page lookup failed for %q: %v\n", invoiceID, err)
		return payPageData{
			Error: "We couldn't load this bill just now. Please try again in a moment, or reply on WhatsApp for help.",
		}, http.StatusInternalServerError
	}

	data := payPageData{
		CustomerName: custName,
		Month:        prettyMonth(month),
		Total:        formatRupees(total),
	}

	// Breakdown is written by billing.aggregate as
	// {quantities:{}, prices:{}, lineTotals:{}}. A malformed or missing
	// breakdown shouldn't hide the total the customer owes, so failures here
	// degrade to a bill with no line items rather than an error page.
	var bd struct {
		Quantities map[string]int     `json:"quantities"`
		Prices     map[string]float64 `json:"prices"`
		LineTotals map[string]float64 `json:"lineTotals"`
	}
	if len(breakdownRaw) > 0 {
		if err := json.Unmarshal(breakdownRaw, &bd); err != nil {
			fmt.Printf("⚠️ pay page: bad breakdown on invoice %s: %v\n", invoiceID, err)
		}
	}

	// Iterate payProducts, not the map, so line order is stable across loads
	// and matches the WhatsApp bill.
	for _, p := range payProducts {
		qty := bd.Quantities[p]
		if qty <= 0 {
			continue
		}
		// breakdown.prices is already per LITRE or KILOGRAM — the unit a
		// customer quotes and the same figure the admin typed on the route.
		// No conversion needed here; aggregate does it once when building the
		// breakdown.
		unit := "kg"
		if payLitreProducts[p] {
			unit = "L"
		}
		data.Lines = append(data.Lines, payLine{
			Label:    payProductLabels[p],
			Quantity: formatPayQty(qty, p),
			Rate:     formatRupees(bd.Prices[p]) + "/" + unit,
			Total:    formatRupees(bd.LineTotals[p]),
		})
	}

	if strings.HasPrefix(status, "PAID") {
		data.Paid = true
		if status == "PAID_CASH" {
			data.PaidMethod = "in cash"
		} else {
			data.PaidMethod = "online"
		}
		return data, http.StatusOK
	}

	// Unpaid: mint or reuse a link. CreateLinkForInvoice reuses the existing
	// one when the amount still matches, cancels and reissues when it doesn't,
	// and refuses on a zero total.
	if s.Enabled() {
		url, err := s.CreateLinkForInvoice(ctx, invoiceID)
		if err != nil {
			// Deliberately not surfaced to the customer as an error — they get
			// manual instructions, which is exactly what the WhatsApp bill
			// falls back to when the gateway is down.
			fmt.Printf("⚠️ pay page: link creation failed for invoice %s: %v\n", invoiceID, err)
		} else {
			data.PayURL = url
		}
	}

	return data, http.StatusOK
}

// formatRupees renders an amount with Indian digit grouping — 1,23,456.50
// rather than 123,456.50. Getting this wrong on a bill reads as sloppy to an
// Indian customer, and Go's formatting has no locale support.
func formatRupees(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}

	whole := int64(v)
	paise := int64((v-float64(whole))*100 + 0.5)
	if paise >= 100 { // rounding carried over
		whole++
		paise -= 100
	}

	s := fmt.Sprintf("%d", whole)

	// Indian grouping: last three digits, then twos.
	var grouped string
	if len(s) <= 3 {
		grouped = s
	} else {
		last3 := s[len(s)-3:]
		rest := s[:len(s)-3]
		var parts []string
		for len(rest) > 2 {
			parts = append([]string{rest[len(rest)-2:]}, parts...)
			rest = rest[:len(rest)-2]
		}
		if rest != "" {
			parts = append([]string{rest}, parts...)
		}
		grouped = strings.Join(parts, ",") + "," + last3
	}

	out := grouped
	if paise > 0 {
		out = fmt.Sprintf("%s.%02d", grouped, paise)
	}
	if neg {
		out = "-" + out
	}
	return out
}
