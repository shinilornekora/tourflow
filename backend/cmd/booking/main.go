// booking — остатки номеров и брони. Ядро системы.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/booking/api"
	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/booking/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/config"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/svc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	base, err := svc.Boot(ctx, "booking", ":8083", migrations.FS, migrations.Deny...)
	if err != nil {
		return err
	}
	defer base.Close()

	loc, err := time.LoadLocation(config.String("HOTEL_TIMEZONE", "Europe/Moscow"))
	if err != nil {
		return fmt.Errorf("HOTEL_TIMEZONE: %w", err)
	}
	st := store.Store{Pool: base.Pool}
	// Служебные процессы (проекция каталога, чистка, сверка) идут под
	// владельцем: роль данных не может писать реплику каталога и inbox.
	maint := store.Store{Pool: base.Owner}
	service := &app.Service{Store: st, Location: loc}

	publisher := messaging.NewKafkaPublisher(base.Brokers)
	defer publisher.Close()
	relay := &messaging.Relay{Pool: base.Owner, Publisher: publisher, Log: base.Log}
	consumer := &messaging.Consumer{
		Brokers: base.Brokers, GroupID: "booking-catalog-projection",
		Topics: []string{messaging.TopicHotels, messaging.TopicRoomTypes},
		Handler: func(ctx context.Context, env messaging.Envelope) error {
			return service.HandleCatalogEvent(ctx, maint, env)
		},
		DLQ: publisher, Log: base.Log,
	}

	messaging.EnsureTopics(ctx, base.Brokers, base.Log, messaging.TopicHotels, messaging.TopicRoomTypes, messaging.TopicReservation)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); relay.Run(ctx) }()
	go func() { defer wg.Done(); _ = consumer.Run(ctx) }()
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := maint.PurgeIdempotency(ctx, 24*time.Hour); err == nil && n > 0 {
				base.Log.Info("idempotency keys purged", "count", n)
			}
			if n, err := maint.PurgeDelivered(ctx, 30*24*time.Hour); err == nil && n > 0 {
				base.Log.Info("delivered outbox/inbox rows purged", "count", n)
			}
			// Фоновая сверка остатков с бронями (ПР-1): расхождение — дефект.
			if bad, err := maint.Reconcile(ctx); err != nil {
				base.Log.Warn("inventory reconciliation failed", "error", err)
			} else if len(bad) > 0 {
				base.Log.Error("inventory does not match reservations", "mismatches", len(bad),
					"first_room_type", bad[0].RoomTypeID, "first_day", bad[0].Day.Format("2006-01-02"))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	mux := http.NewServeMux()
	(&api.Handler{Svc: service, Log: base.Log}).Routes(mux)
	err = base.Serve(ctx, base.Handler(mux))
	stop()
	wg.Wait()
	return err
}
