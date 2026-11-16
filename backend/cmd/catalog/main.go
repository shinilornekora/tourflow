// catalog — отели, категории номеров, менеджеры отелей.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/shinilornekora/tourflow/backend/internal/catalog/api"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/app"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/directory"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/store"
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

	base, err := svc.Boot(ctx, "catalog", ":8082", migrations.FS, migrations.Deny...)
	if err != nil {
		return err
	}
	defer base.Close()

	identityURL := config.String("IDENTITY_URL", "http://localhost:8081")
	service := &app.Service{
		Store:     store.Store{Pool: base.Pool},
		Directory: directory.New(identityURL, config.String("INTERNAL_TOKEN", "")),
	}

	publisher := messaging.NewKafkaPublisher(base.Brokers)
	defer publisher.Close()
	relay := &messaging.Relay{Pool: base.Owner, Publisher: publisher, Log: base.Log}

	messaging.EnsureTopics(ctx, base.Brokers, base.Log, messaging.TopicHotels, messaging.TopicRoomTypes)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); relay.Run(ctx) }()

	mux := http.NewServeMux()
	(&api.Handler{Svc: service, Log: base.Log}).Routes(mux)
	err = base.Serve(ctx, base.Handler(mux))
	stop()
	wg.Wait()
	return err
}
