// Package pg — подключение к PostgreSQL, миграции и транзакции.
package pg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect открывает пул и ждёт готовности БД до deadline: в docker-compose
// сервис может стартовать раньше Postgres.
func Connect(ctx context.Context, log *slog.Logger, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "tourflow"
	// Защита от «зависших» запросов: ни одна команда не держит соединение
	// дольше 5 секунд. Длинные отчёты в этом сервисе не предусмотрены.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	// Ожидание чужой блокировки строки ограничено отдельно: обработчик
	// события или бронь не ждут FOR UPDATE дольше 2 секунд (РХД ПР-5).
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "2000"

	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("connect postgres: %w", err)
		}
		log.Warn("postgres not ready, retrying", "attempt", attempt, "error", err)
		select {
		case <-time.After(time.Duration(min(attempt, 5)) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Migrate применяет *.sql из fsys в лексикографическом порядке. Каждая
// миграция — в своей транзакции; конкурентный старт нескольких реплик
// сериализуется advisory-блокировкой.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	const lockID = 727274 // произвольная константа «tourflow migrations»
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}

// Querier — общее подмножество pgxpool.Pool и pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// InTx выполняет fn в транзакции. Ошибки сериализации и взаимоблокировки
// повторяются до 3 раз с джиттером: при уровне SERIALIZABLE или при
// встречных блокировках это штатная ситуация, а не авария.
func InTx(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = pgx.BeginTxFunc(ctx, pool, opts, fn)
		if !IsRetryable(err) {
			return err
		}
		select {
		case <-time.After(time.Duration(10*(attempt+1)) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// IsRetryable — serialization_failure (40001) или deadlock_detected (40P01).
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// IsUniqueViolation — нарушение уникальности (23505), опционально по имени ограничения.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return constraint == "" || pgErr.ConstraintName == constraint
	}
	return false
}

// IsCheckViolation — нарушение CHECK (23514).
func IsCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return constraint == "" || pgErr.ConstraintName == constraint
	}
	return false
}

// IsFKViolation — нарушение внешнего ключа (23503).
func IsFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// SetActor выставляет контекст безопасности для RLS на время транзакции.
// Именно set_config(..., true) = SET LOCAL: значение сбрасывается при
// COMMIT/ROLLBACK и не «перетекает» к следующему запросу, который получит
// это же соединение из пула (урок РХД-2).
func SetActor(ctx context.Context, tx pgx.Tx, userID, role string) error {
	_, err := tx.Exec(ctx,
		`SELECT set_config('app.user_id', $1, true), set_config('app.role', $2, true)`,
		userID, role)
	return err
}
