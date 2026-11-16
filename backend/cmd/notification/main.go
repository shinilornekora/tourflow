// notification — уведомления гостей о бронях.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/shinilornekora/tourflow/backend/internal/notification/api"
	"github.com/shinilornekora/tourflow/backend/internal/notification/app"
	"github.com/shinilornekora/tourflow/backend/internal/notification/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/notification/store"
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

	base, err := svc.Boot(ctx, "notification", ":8084", migrations.FS)
	if err != nil {
		return err
	}
	defer base.Close()

	service := &app.Service{Store: store.Store{Pool: base.Pool}, Channel: app.LogChannel{Log: base.Log}, Log: base.Log}

	dlq := messaging.NewKafkaPublisher(base.Brokers)
	defer dlq.Close()
	consumer := &messaging.Consumer{
		Brokers: base.Brokers, GroupID: "notification",
		Topics:  []string{messaging.TopicReservation},
		Handler: service.HandleReservationEvent,
		DLQ:     dlq, Log: base.Log,
	}

	messaging.EnsureTopics(ctx, base.Brokers, base.Log, messaging.TopicReservation)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = consumer.Run(ctx) }()

	mux := http.NewServeMux()
	(&api.Handler{Svc: service, Log: base.Log}).Routes(mux)
	err = base.Serve(ctx, base.Handler(mux))
	stop()
	wg.Wait()
	return err
}
