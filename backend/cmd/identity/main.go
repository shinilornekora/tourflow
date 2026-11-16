// identity — пользователи, роли, выпуск токенов.
//
// Зависимости собираются вручную, без DI-контейнера: весь граф виден в одной
// функции, ошибка сборки — ошибка компиляции, а не паника при старте.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shinilornekora/tourflow/backend/internal/identity/api"
	"github.com/shinilornekora/tourflow/backend/internal/identity/app"
	"github.com/shinilornekora/tourflow/backend/internal/identity/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/identity/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/config"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/logx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	common, err := config.LoadCommon("identity", ":8081")
	if err != nil {
		return err
	}
	log := logx.New(common.ServiceName, common.LogLevel)

	dsn, err := config.Required("DATABASE_URL")
	if err != nil {
		return err
	}
	accessTTL, err := config.Duration("ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return err
	}
	refreshTTL, err := config.Duration("REFRESH_TOKEN_TTL", 30*24*time.Hour)
	if err != nil {
		return err
	}
	bcryptCost, err := config.Int("BCRYPT_COST", 12)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Две роли, как у остальных сервисов (T111): миграции — под владельцем,
	// запросы — под ролью данных, которая не может менять схему. В identity
	// лежат хэши паролей и refresh-токены, DDL коду запросов здесь не нужен.
	pools, err := pg.OpenPools(ctx, log, dsn, config.String("APP_DATABASE_URL", ""), 10, migrations.FS)
	if err != nil {
		return err
	}
	defer pools.Close()
	pool := pools.App

	signer, err := auth.NewSigner(config.String("JWT_SIGNING_SEED", ""), accessTTL)
	if err != nil {
		return err
	}
	if config.String("JWT_SIGNING_SEED", "") == "" {
		log.Warn("JWT_SIGNING_SEED is empty: using ephemeral key, tokens will not survive restart")
	}

	svc := &app.Service{
		Users:      store.Users{Pool: pool},
		Tokens:     store.Tokens{Pool: pool},
		Hasher:     store.Bcrypt{Cost: bcryptCost},
		Issuer:     signer,
		RefreshTTL: refreshTTL,
	}

	if email := config.String("BOOTSTRAP_ADMIN_EMAIL", ""); email != "" {
		created, err := svc.EnsureAdmin(ctx, email, config.String("BOOTSTRAP_ADMIN_PASSWORD", ""), "Администратор")
		if err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
		if created {
			log.Info("bootstrap admin created", "email", email)
		}
	}

	verifier := auth.NewVerifier(auth.StaticKeys(signer.PublicKeys()))
	probes := httpx.NewProbes(pool.Ping)
	mux := http.NewServeMux()
	probes.Register(mux)
	mux.HandleFunc("GET /.well-known/jwks.json", signer.JWKSHandler())
	(&api.Handler{
		Svc: svc, Log: log,
		SecureCookie:  config.String("COOKIE_SECURE", "false") == "true",
		InternalToken: config.String("INTERNAL_TOKEN", ""),
	}).Routes(mux)

	onAuthErr := func(w http.ResponseWriter, r *http.Request, err error) { httpx.Error(w, r, log, err) }
	handler := httpx.Chain(mux, append(httpx.Standard(log), verifier.Middleware(onAuthErr))...)
	return httpx.Run(ctx, log, common.HTTPAddr, handler, probes, common.ShutdownTimeout)
}
