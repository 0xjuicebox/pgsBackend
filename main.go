package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
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
	"github.com/0xjuicebox/pgsBackend/internal/stats"
	"github.com/0xjuicebox/pgsBackend/internal/subscription"
	"github.com/0xjuicebox/pgsBackend/internal/update"
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
	log.Println("Successfully connected and pinged Supabase PostgreSQL engine.")

	// Background sweepers
	go driver.StartAutoEndSweeper(pool)
	go route.StartPricePromotionSweeper(pool)
	go update.StartChangeSweeper(pool)

	// WhatsApp — NewWhatsAppService reads Twilio creds from env itself
	whatsApp := notification.NewWhatsAppService()

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

	// Receipt on payment. A callback so internal/payment never has to know
	// the notification package exists.
	pay.OnPaid = func(invoiceID string, amount float64) {
		cbCtx, cbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cbCancel()

		var phone, name string
		err := pool.QueryRow(cbCtx, `
			SELECT c.phone_number, c.name
			FROM invoices i JOIN customers c ON c.id = i.customer_id
			WHERE i.id = $1::uuid`, invoiceID).Scan(&phone, &name)
		if err != nil {
			fmt.Printf("⚠️ payment receipt: couldn't load customer for invoice %s: %v\n", invoiceID, err)
			return
		}
		whatsApp.SendDeliveryUpdate(phone, fmt.Sprintf(
			"✅ Payment received — thank you, %s!\n\nWe've received ₹%.0f. Your bill is now settled.",
			name, amount))
	}

	// Resources
	cr := customer.CustomerResource{DB: pool, WhatsApp: whatsApp}
	rr := route.RouteResource{DB: pool}
	dr := driver.DriverResource{DB: pool}
	dlr := delivery.DeliveryResource{DB: pool, WhatsApp: whatsApp}
	sr := subscription.SubscriptionResource{DB: pool}
	or := override.OverrideResource{DB: pool, WhatsApp: whatsApp}
	wr := webhook.WhatsAppResource{WhatsApp: whatsApp, DB: pool}
	regr := registration.Resource{DB: pool, WhatsApp: whatsApp}
	ur := update.UpdateResource{DB: pool, WhatsApp: whatsApp}
	statsResource := stats.StatsResource{DB: pool}
	br := billing.BillingResource{DB: pool, WhatsApp: whatsApp, Payment: pay}
	cfgR := config.ConfigResource{DB: pool}

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

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Server starting on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
