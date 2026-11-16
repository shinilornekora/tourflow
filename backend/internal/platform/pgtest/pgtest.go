// Package pgtest — помощник интеграционных тестов с настоящим PostgreSQL.
//
// Тесты запускаются, только если задан TEST_PG_DSN, например:
//
//	TEST_PG_DSN="postgres://tourflow:tourflow@localhost:5432/%s?sslmode=disable"
//
// Вместо %s подставляется имя тестовой БД (booking_test, catalog_test...).
//
// TEST_PG_APP_DSN (тот же формат) — роль данных без прав владельца, как
// сервис в бою (pg.Pools). Если задан, код сервиса в тестах работает под
// ней, а миграции и служебные проверки — под владельцем из TEST_PG_DSN.
// Пользователь — НЕ суперпользователь: суперпользователь обходит RLS, и
// тесты политик доступа прошли бы ложно.
package pgtest

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

// Fresh пересоздаёт схему public, применяет миграции и возвращает пул, под
// которым работает код сервиса (роль данных, если задана).
func Fresh(t *testing.T, db string, migrations fs.FS, deny ...pg.Deny) *pgxpool.Pool {
	t.Helper()
	return FreshPools(t, db, migrations, deny...).App
}

// Split — заданы ли разные роли владельца и данных.
func Split() bool { return os.Getenv("TEST_PG_APP_DSN") != "" }

// FreshPools — то же, но с обоими пулами.
func FreshPools(t *testing.T, db string, migrations fs.FS, deny ...pg.Deny) pg.Pools {
	t.Helper()
	tmpl := os.Getenv("TEST_PG_DSN")
	if tmpl == "" {
		t.Skip("TEST_PG_DSN is not set: integration test skipped")
	}
	dsn := tmpl
	if strings.Contains(tmpl, "%s") {
		dsn = fmt.Sprintf(tmpl, db)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := pg.Connect(ctx, log, dsn, 40)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Fatal("TEST_PG_DSN must use a non-superuser role (superusers bypass RLS)")
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	pool.Close()
	appDSN := os.Getenv("TEST_PG_APP_DSN")
	if strings.Contains(appDSN, "%s") {
		appDSN = fmt.Sprintf(appDSN, db)
	}
	pools, err := pg.OpenPools(ctx, log, dsn, appDSN, 40, migrations, deny...)
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(pools.Close)
	return pools
}
