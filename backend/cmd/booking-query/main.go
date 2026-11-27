// booking-query — модель чтения броней: карточки, списки, сводки.
// Отдельный экземпляр PostgreSQL; данные приходят только событиями Kafka.
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

	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/api"
	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/app"
	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/store"
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

	base, err := svc.Boot(ctx, "booking-query", ":8085", migrations.FS, migrations.Deny...)
	if err != nil {
		return err
	}
	defer base.Close()

	loc, err := time.LoadLocation(config.String("HOTEL_TIMEZONE", "Europe/Moscow"))
	if err != nil {
		return fmt.Errorf("HOTEL_TIMEZONE: %w", err)
	}
	// API читает под ролью данных (без прав на запись), проектор пишет под
	// владельцем.
	st := store.Store{Pool: base.Pool, Owner: base.Owner}
	service := &app.Service{Store: st, Location: loc}

	dlq := messaging.NewKafkaPublisher(base.Brokers)
	defer dlq.Close()
	// Своя группа: проекция читает топики с начала независимо от booking и
	// notification. Новый экземпляр проекции восстанавливается переигровкой
	// событий, пока они хранятся в Kafka.
	consumer := &messaging.Consumer{
		Brokers: base.Brokers, GroupID: "booking-query-projection",
		Topics:  []string{messaging.TopicReservation, messaging.TopicHotels},
		Handler: service.Handle,
		DLQ:     dlq, Log: base.Log,
	}
	messaging.EnsureTopics(ctx, base.Brokers, base.Log, messaging.TopicReservation, messaging.TopicHotels)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(ctx) }()
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if n, err := st.PurgeInbox(ctx, 30*24*time.Hour); err == nil && n > 0 {
				base.Log.Info("projection inbox purged", "count", n)
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
