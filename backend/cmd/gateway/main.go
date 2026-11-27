// gateway — единая точка входа /api/* для SPA.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/gateway"
	"github.com/shinilornekora/tourflow/backend/internal/platform/config"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/resilience"
	"github.com/shinilornekora/tourflow/backend/internal/platform/svc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func upstream(name, env, def string, timeout time.Duration, prefixes ...string) (*gateway.Upstream, error) {
	u, err := url.Parse(config.String(env, def))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env, err)
	}
	return &gateway.Upstream{Name: name, URL: u, Prefixes: prefixes, Timeout: timeout}, nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	base, err := svc.Boot(ctx, "gateway", ":8080", nil)
	if err != nil {
		return err
	}

	// Бюджеты времени по сервисам: чтение каталога должно быть быстрым,
	// бронь пишет и ждёт блокировки строк — ей даём больше.
	identity, err := upstream("identity", "IDENTITY_URL", "http://localhost:8081", 5*time.Second, "/api/auth", "/api/users")
	if err != nil {
		return err
	}
	catalog, err := upstream("catalog", "CATALOG_URL", "http://localhost:8082", 5*time.Second, "/api/hotels", "/api/room-types", "/api/cities")
	if err != nil {
		return err
	}
	booking, err := upstream("booking", "BOOKING_URL", "http://localhost:8083", 8*time.Second, "/api/reservations", "/api/availability", "/api/audit")
	if err != nil {
		return err
	}
	// Модель чтения броней (CQRS): GET /api/reservations* — в booking-query,
	// команды — в booking. BOOKING_READS=booking возвращает чтение в booking
	// (откат, если проекция отстала или сломалась).
	bookingQuery, err := upstream("booking-query", "BOOKING_QUERY_URL", "http://localhost:8085", 3*time.Second, "/api/reservations")
	if err != nil {
		return err
	}
	bookingQuery.Methods = []string{http.MethodGet, http.MethodHead}
	notification, err := upstream("notification", "NOTIFICATION_URL", "http://localhost:8084", 3*time.Second, "/api/notifications")
	if err != nil {
		return err
	}

	// Общий лимит на IP: 30 запросов в секунду, всплеск до 60. Настраивается
	// только ради нагрузочного замера (docs/LOAD.md): генератор ходит с
	// одного адреса, и без этого замер измерял бы лимитер, а не систему.
	rps, err := config.Int("RATE_LIMIT_RPS", 30)
	if err != nil {
		return err
	}
	burst, err := config.Int("RATE_LIMIT_BURST", 60)
	if err != nil {
		return err
	}

	upstreams := []*gateway.Upstream{identity, catalog, booking, notification}
	if config.String("BOOKING_READS", "query") != "booking" {
		upstreams = append(upstreams, bookingQuery)
	}

	g := &gateway.Gateway{
		Log:         base.Log,
		Verifier:    base.Verifier,
		Upstreams:   upstreams,
		AllowOrigin: config.List("CORS_ORIGINS", []string{"http://localhost:3000"}),
		GlobalLimit: resilience.NewTokenBucket(float64(rps), float64(burst)),
		AuthLimit:   resilience.NewTokenBucket(5.0/60.0, 10), // 5 в минуту, всплеск до 10
		TrustProxy:  config.String("TRUST_PROXY", "false") == "true",
	}
	g.Init()

	mux := http.NewServeMux()
	base.Probes.Register(mux)
	mux.Handle("/api/", g)
	mux.Handle("/internal/", g)
	handler := httpx.Chain(mux, httpx.Standard(base.Log)...)
	return base.Serve(ctx, handler)
}
