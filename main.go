package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/0xjuicebox/pgsBackend/internal/customer"
	"github.com/0xjuicebox/pgsBackend/internal/delivery"
	"github.com/0xjuicebox/pgsBackend/internal/driver"
	"github.com/0xjuicebox/pgsBackend/internal/override"
	"github.com/0xjuicebox/pgsBackend/internal/route"
	"github.com/0xjuicebox/pgsBackend/internal/subscription"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {

	if err := godotenv.Load(); err != nil {
		log.Println("Warning: No .env file found, falling back to system environment variables")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL environment variable is missing!")
	}

	config, err := pgxpool.ParseConfig(dsn)

	if err != nil {
		log.Fatalf("Unable to parse connection string: %v", err)
	}

	config.MaxConns = 20
	config.MinConns = 2
	config.MaxConnIdleTime = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}

	log.Println("Successfully connected and pinged Supabase PostgreSQL engine.")

	r := chi.NewRouter()

	//Middleware stack
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	cr := customer.CustomerResource{DB: pool}
	rr := route.RouteResource{DB: pool}
	dr := driver.DriverResource{DB: pool}
	sr := subscription.SubscriptionResource{DB: pool}
	or := override.OverrideResource{DB: pool}
	delR := delivery.DeliveryResource{DB: pool}
	r.Mount("/customer", cr.Routes())
	r.Mount("/route", rr.Routes())
	r.Mount("/driver", dr.Routes())
	r.Mount("/subscription", sr.Routes())
	r.Mount("/override", or.Routes())
	r.Mount("/delivery", delR.Routes())

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("welcome"))
	})

	http.ListenAndServe(":3000", r)
}
