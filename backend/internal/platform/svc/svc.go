// Package svc — общий «скелет» процесса сервиса: конфиг, логгер, БД,
// миграции, проверка токенов. Бизнес-зависимости каждый main собирает сам.
package svc

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/config"
	"github.com/shinilornekora/tourflow/backend/internal/platform/httpx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/logx"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

type Base struct {
	Common config.Common
	Log    *slog.Logger
	// Pool — роль данных (запросы пользователей), Owner — владелец таблиц
	// (миграции, ретранслятор outbox, служебные задачи). Без
	// APP_DATABASE_URL это один и тот же пул.
	Pool     *pgxpool.Pool
	Owner    *pgxpool.Pool
	pools    pg.Pools
	Verifier *auth.Verifier
	Probes   *httpx.Probes
	Brokers  []string
}

// Boot поднимает общие зависимости. migrations может быть nil.
func Boot(ctx context.Context, name, addr string, migrations fs.FS, deny ...pg.Deny) (*Base, error) {
	common, err := config.LoadCommon(name, addr)
	if err != nil {
		return nil, err
	}
	log := logx.New(name, common.LogLevel)
	b := &Base{Common: common, Log: log, Brokers: config.List("KAFKA_BROKERS", []string{"localhost:9092"})}

	if migrations != nil {
		dsn, err := config.Required("DATABASE_URL")
		if err != nil {
			return nil, err
		}
		maxConns, err := config.Int("DB_MAX_CONNS", 10)
		if err != nil {
			return nil, err
		}
		b.pools, err = pg.OpenPools(ctx, log, dsn, config.String("APP_DATABASE_URL", ""), int32(maxConns), migrations, deny...)
		if err != nil {
			return nil, err
		}
		b.Pool, b.Owner = b.pools.App, b.pools.Owner
		b.Probes = httpx.NewProbes(b.Pool.Ping)
	} else {
		b.Probes = httpx.NewProbes(nil)
	}

	jwks := config.String("JWKS_URL", "http://localhost:8081/.well-known/jwks.json")
	b.Verifier = auth.NewVerifier(auth.NewRemoteJWKS(jwks, 5*time.Minute))
	return b, nil
}

// Handler оборачивает mux стандартными middleware и проверкой токена.
func (b *Base) Handler(mux *http.ServeMux) http.Handler {
	b.Probes.Register(mux)
	onAuthErr := func(w http.ResponseWriter, r *http.Request, err error) { httpx.Error(w, r, b.Log, err) }
	return httpx.Chain(mux, append(httpx.Standard(b.Log), b.Verifier.Middleware(onAuthErr))...)
}

// Serve запускает HTTP до отмены ctx.
func (b *Base) Serve(ctx context.Context, h http.Handler) error {
	return httpx.Run(ctx, b.Log, b.Common.HTTPAddr, h, b.Probes, b.Common.ShutdownTimeout)
}

func (b *Base) Close() { b.pools.Close() }
