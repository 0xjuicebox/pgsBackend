package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/billing"
	"github.com/0xjuicebox/pgsBackend/internal/config"
	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/delivery"
	"github.com/0xjuicebox/pgsBackend/internal/driver"
	"github.com/0xjuicebox/pgsBackend/internal/notification"
	"github.com/0xjuicebox/pgsBackend/internal/override"
	"github.com/0xjuicebox/pgsBackend/internal/payment"
	"github.com/0xjuicebox/pgsBackend/internal/registration"
	"github.com/0xjuicebox/pgsBackend/internal/route"
	"github.com/0xjuicebox/pgsBackend/internal/schedule"
	"github.com/0xjuicebox/pgsBackend/internal/stats"
	"github.com/0xjuicebox/pgsBackend/internal/subscription"
	"github.com/0xjuicebox/pgsBackend/internal/update"
	"github.com/0xjuicebox/pgsBackend/internal/web"
	"github.com/0xjuicebox/pgsBackend/internal/webhook"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Notice: No .env file found, reading environment variables from system")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL environment variable is missing!")
	}

	pgConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatalf("Unable to parse connection string: %v", err)
	}
	// -----------------------------------------------------------------
	// Session timezone. This is load-bearing, not cosmetic.
	//
	// Supabase sessions default to UTC. Every CURRENT_DATE in this
	// codebase was therefore returning the UTC date, while the Go code
	// that writes those dates computes them in Asia/Kolkata. The two
	// disagree for the first five and a half hours of every Indian day.
	//
	// Concretely: update.ApprovePendingChange sets effective_from to
	// tomorrow-in-IST, and applyDueChanges promotes it when
	// `effective_from <= CURRENT_DATE`. Approve on Monday and the change
	// is due Tuesday — but until 05:30 IST Tuesday, CURRENT_DATE still
	// reads Monday, so the sweeper skips it. The morning manifest is
	// built at the 03:00 cutoff from the customer's OLD order and the van
	// leaves before the change lands. "Effective from tomorrow" silently
	// means "effective from tomorrow mid-morning" for the morning slot.
	//
	// route.StartPricePromotionSweeper has the identical mismatch
	// (pricing.go computes the 1st in IST, the sweeper compares against
	// CURRENT_DATE), as does the `target_date >= CURRENT_DATE` filter in
	// override.go. Setting it once here fixes all of them, because the
	// parameter applies to every connection the pool hands out.
	//
	// Verify with: SHOW timezone;  -> Asia/Kolkata
	// -----------------------------------------------------------------
	pgConfig.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"

	pgConfig.MaxConns = 20
	pgConfig.MinConns = 2
	pgConfig.MaxConnIdleTime = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, pgConfig)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}

	// Confirm the timezone actually took. A silently-ignored RuntimeParam
	// would reintroduce the bug above with no visible symptom until a
	// customer's order changed a day late — which is exactly the class of
	// failure this codebase has been worst at surfacing. One round trip at
	// boot is cheap insurance.
	var dbTZ string
	if err := pool.QueryRow(ctx, "SHOW timezone").Scan(&dbTZ); err != nil {
		log.Printf("⚠️  Could not read database timezone: %v", err)
	} else if dbTZ != "Asia/Kolkata" {
		log.Printf("⚠️  Database timezone is %q, expected Asia/Kolkata — date-based sweepers will drift.", dbTZ)
	} else {
		var dbDate time.Time
		if err := pool.QueryRow(ctx, "SELECT CURRENT_DATE").Scan(&dbDate); err == nil {
			log.Printf("Database timezone: %s (CURRENT_DATE = %s)", dbTZ, dbDate.Format("2006-01-02"))
		}
	}

	log.Println("Successfully connected and pinged Supabase PostgreSQL engine.")

	// Background sweepers
	go driver.StartAutoEndSweeper(pool)
	go route.StartPricePromotionSweeper(pool)
	// Freezes each round's plan at its order cutoff, so the manifest becomes
	// a stored fact rather than a live query. Without it there was no record
	// a driver was ever given a list, and past manifests were reconstructed
	// from present-day subscriptions and were therefore wrong.
	go route.StartManifestLockSweeper(pool)
	go update.StartChangeSweeper(pool)

	// WhatsApp — NewWhatsAppService reads Twilio creds from env itself
	whatsApp := notification.NewWhatsAppService()

	// Approved Twilio content templates.
	//
	// WhatsApp only permits free-form text within 24 hours of the customer's
	// last inbound message. Bills, reminders, suspension notices, delivery
	// confirmations and receipts all go to customers who haven't just
	// messaged, so each needs an approved template or it silently fails with
	// error 63016. Reported at boot because an unset SID means a whole
	// category of message stops reaching quiet customers — a failure that
	// otherwise surfaces weeks later as "nobody paid this month".
	templates := notification.LoadTemplateSIDs()
	if missing := templates.TemplateHealth(); len(missing) > 0 {
		log.Printf("⚠️  WhatsApp templates not configured: %s — these messages fall back to free-form and will only reach customers who messaged in the last 24h.",
			strings.Join(missing, ", "))
	} else {
		log.Println("WhatsApp templates: all configured.")
	}

	// -----------------------------------------------------------------
	// Payments
	//
	// Constructed before the resources that depend on it. Enabled() is
	// false when the env vars are absent, and billing checks that before
	// creating links — so a deployment without Razorpay credentials still
	// sends bills, just with the manual "transfer and reply PAID" line.
	// -----------------------------------------------------------------
	pay := payment.New(
		pool,
		os.Getenv("RAZORPAY_KEY_ID"),
		os.Getenv("RAZORPAY_KEY_SECRET"),
		os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
	)
	if pay.Enabled() {
		log.Println("Razorpay payments enabled.")
	} else {
		log.Println("Notice: Razorpay credentials absent — bills will use manual payment instructions.")
	}

	// Resources
	cr := customer.CustomerResource{DB: pool, WhatsApp: whatsApp, Templates: templates}
	rr := route.RouteResource{DB: pool}
	dr := driver.DriverResource{DB: pool}
	dlr := delivery.DeliveryResource{DB: pool, WhatsApp: whatsApp, Templates: templates}
	sr := subscription.SubscriptionResource{DB: pool}
	or := override.OverrideResource{DB: pool, WhatsApp: whatsApp}
	wr := webhook.WhatsAppResource{WhatsApp: whatsApp, DB: pool}
	regr := registration.Resource{DB: pool, WhatsApp: whatsApp}
	ur := update.UpdateResource{DB: pool, WhatsApp: whatsApp, Templates: templates}
	statsResource := stats.StatsResource{DB: pool}
	br := billing.BillingResource{DB: pool, WhatsApp: whatsApp, Payment: pay, Templates: templates}

	// OnPaid is assigned here, after br exists, because the callback needs
	// BillingResource to lift a non-payment suspension. Payment is what
	// resumes a suspended customer, so the two are inseparable.
	//
	// Receipt on payment. A callback so internal/payment never has to know
	// the notification package exists.
	//
	// Uses the approved template: a customer who pays by tapping the link in
	// a bill hasn't necessarily messaged us, so a free-form receipt would be
	// rejected outside the 24-hour window. Falls back to free-form when the
	// template isn't configured.
	pay.OnPaid = func(invoiceID string, amount float64) {
		cbCtx, cbCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cbCancel()

		var phone, name, month, customerID string
		err := pool.QueryRow(cbCtx, `
			SELECT c.phone_number, c.name, i.billing_month, c.id::text
			FROM invoices i JOIN customers c ON c.id = i.customer_id
			WHERE i.id = $1::uuid`, invoiceID).Scan(&phone, &name, &month, &customerID)
		if err != nil {
			fmt.Printf("⚠️ payment receipt: couldn't load customer for invoice %s: %v\n", invoiceID, err)
			return
		}

		prettyMonth := month
		if t, perr := time.Parse("2006-01", month); perr == nil {
			prettyMonth = t.Format("January 2006")
		}

		// Lift a non-payment suspension before sending anything, so the
		// message can reflect the customer's actual state. Fires for both the
		// Razorpay webhook and an admin marking a bill PAID_CASH, since both
		// route through OnPaid.
		//
		// resumed is false when the customer was restored to 'disabled'
		// rather than 'active' — they had paused for a holiday before being
		// suspended, so paying clears the debt without restarting
		// deliveries. They get an ordinary receipt, not a welcome-back.
		resumed, rerr := br.ResumeIfSuspended(cbCtx, invoiceID)
		if rerr != nil {
			fmt.Printf("⚠️ payment receipt: resume check failed for invoice %s: %v\n", invoiceID, rerr)
		}
		if resumed {
			log.Printf("▶️  resumed suspended customer after payment on invoice %s", invoiceID)

			// The customer paid by tapping a link in a suspension template.
			// They have NOT messaged us, so there is no open 24-hour window
			// and the free-form version this replaces was silently dropped —
			// at the exact moment someone had just handed over money and was
			// waiting to hear their milk was coming back.
			nextDelivery := "your next scheduled delivery"
			if slots, serr := schedule.LoadSlots(cbCtx, pool, customerID); serr == nil {
				nextDelivery = schedule.FirstDeliveryLabel(slots, time.Now())
			} else {
				fmt.Printf("⚠️ resume notice: couldn't load schedule for %s: %v\n", customerID, serr)
			}

			if templates.Resumed != "" {
				if terr := whatsApp.SendResumedAfterPayment(
					phone, templates.Resumed, name,
					fmt.Sprintf("%.0f", amount), prettyMonth, nextDelivery,
				); terr == nil {
					return
				} else {
					fmt.Printf("⚠️ resumed template failed, falling back: %v\n", terr)
				}
			}

			whatsApp.SendDeliveryUpdate(phone, fmt.Sprintf(
				"✅ Payment received — thank you, %s!\n\nWe've received ₹%.0f for your %s bill, and your deliveries have been resumed. Your next delivery is %s. 🥛",
				name, amount, prettyMonth, nextDelivery))
			return
		}

		if templates.PaymentReceipt != "" {
			if err := whatsApp.SendPaymentReceipt(
				phone, templates.PaymentReceipt, name,
				fmt.Sprintf("%.0f", amount), prettyMonth,
			); err == nil {
				return
			} else {
				fmt.Printf("⚠️ payment receipt: template send failed, falling back: %v\n", err)
			}
		}

		whatsApp.SendDeliveryUpdate(phone, fmt.Sprintf(
			"✅ Payment received — thank you, %s!\n\nWe've received ₹%.0f. Your bill is now settled.",
			name, amount))
	}
	cfgR := config.ConfigResource{DB: pool}

	// Dunning: auto-generate on the 1st, remind daily to the 5th, suspend on
	// the 6th. Started after br so the callback wiring above is in place.
	//
	// Hourly rather than daily on purpose: a once-a-day timer only fires if
	// the process happens to be alive at that moment, so a deploy at 00:59 on
	// the 1st would skip the entire month's billing with nothing to show for
	// it. Every phase is idempotent, so running often is harmless.
	go br.StartDunningSweeper()

	// Router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{
			"http://localhost:8081",  // Expo web dev
			"http://localhost:19006", // Expo web (legacy port)
			os.Getenv("EXPO_PUBLIC_APP_URL"),
		},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300, // cache preflight 5 min — stops the double round-trip on every call
	}))
	r.Use(middleware.Timeout(80 * time.Second))
	r.Use(middleware.Heartbeat("/healthz"))

	// Mount all resources
	r.Mount("/customer", cr.Routes())
	r.Mount("/route", rr.Routes())
	r.Mount("/driver", dr.Routes())
	r.Mount("/delivery", dlr.Routes())
	r.Mount("/subscription", sr.Routes())
	r.Mount("/override", or.Routes())
	r.Mount("/webhook", wr.Routes())
	r.Mount("/register", regr.Routes())
	r.Mount("/update", ur.Routes())
	r.Mount("/stats", statsResource.Routes())
	r.Mount("/billing", br.Routes())
	r.Mount("/config", cfgR.Routes())

	// POST /payment/razorpay — the gateway's webhook lands here.
	r.Mount("/payment", pay.Routes())

	// Public site: landing page and the commerce policies.
	//
	// Mounted at the root and LAST, so every specific path above wins. chi
	// resolves the more specific pattern first, but ordering this deliberately
	// keeps the routing readable.
	//
	// Razorpay will not enable live API keys until a website has been
	// submitted and approved, and a reviewer opens the URL. Before this, / was
	// a 404 — which reads as a dead service.
	businessInfo := web.LoadBusinessInfo()
	if missing := businessInfo.Missing(); len(missing) > 0 {
		log.Printf("⚠️  Public site fields not set: %s — these render as [NOT SET] and must be filled before submitting the site to Razorpay.",
			strings.Join(missing, ", "))
	}
	web.Resource{Info: businessInfo}.RegisterOn(r)

	// GET /pay/{invoiceID} — the customer-facing hosted bill.
	//
	// Mounted at a short root path, not under /payment, because this URL goes
	// inside an approved WhatsApp template button. Template base URLs are
	// fixed at Meta approval time, so this path must stay stable forever —
	// changing it means re-approval.
	r.Mount("/pay", pay.PayPageRoutes())

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Server starting on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
