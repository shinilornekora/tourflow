package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/notification/app"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

type Store struct{ Pool *pgxpool.Pool }

var _ app.Store = Store{}

// as — транзакция с контекстом RLS: пользователь видит только свои
// уведомления; обработчик событий пишет от имени system.
func (s Store) as(ctx context.Context, user uuid.UUID, role string, fn func(pgx.Tx) error) error {
	return pg.InTx(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, user.String(), role); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Deliver — inbox, проверка версии и уведомление в одной транзакции
// (РХД ПР-5). fresh=false: событие уже обработано ИЛИ устарело — по этой
// брони уже было уведомление более новой версии (порядок между репликами
// ретранслятора не гарантирован).
func (s Store) Deliver(ctx context.Context, env messaging.Envelope, n app.Notification) (bool, error) {
	fresh := false
	err := s.as(ctx, uuid.Nil, "system", func(tx pgx.Tx) error {
		first, err := messaging.MarkProcessed(ctx, tx, env)
		if err != nil || !first {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO reservation_versions(reservation_id, version) VALUES ($1, $2)
			ON CONFLICT (reservation_id) DO UPDATE SET version = EXCLUDED.version
			WHERE reservation_versions.version < EXCLUDED.version`, env.AggregateID, env.AggregateVersion)
		if err != nil || tag.RowsAffected() == 0 {
			return err // устаревшее событие: отмечено обработанным, уведомления нет
		}
		_, err = tx.Exec(ctx, `INSERT INTO notifications(id, user_id, kind, title, body, reservation_id, source_event, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT ON CONSTRAINT notifications_source_unique DO NOTHING`,
			n.ID, n.UserID, n.Kind, n.Title, n.Body, n.ReservationID, n.SourceEvent, n.CreatedAt)
		fresh = err == nil
		return err
	})
	return fresh, err
}

func (s Store) List(ctx context.Context, user uuid.UUID, unreadOnly bool, limit, offset int) ([]app.Notification, int, error) {
	var total int
	out := []app.Notification{}
	err := s.as(ctx, user, "user", func(tx pgx.Tx) error {
		out = out[:0]
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND (NOT $2 OR read_at IS NULL)`,
			user, unreadOnly).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, user_id, kind, title, body, reservation_id, source_event, created_at, read_at
			FROM notifications WHERE user_id=$1 AND (NOT $2 OR read_at IS NULL)
			ORDER BY created_at DESC, id LIMIT $3 OFFSET $4`, user, unreadOnly, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n app.Notification
			if err := rows.Scan(&n.ID, &n.UserID, &n.Kind, &n.Title, &n.Body, &n.ReservationID, &n.SourceEvent, &n.CreatedAt, &n.ReadAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, total, err
}

func (s Store) Unread(ctx context.Context, user uuid.UUID) (int, error) {
	var n int
	err := s.as(ctx, user, "user", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND read_at IS NULL`, user).Scan(&n)
	})
	return n, err
}

func (s Store) MarkRead(ctx context.Context, user, id uuid.UUID) error {
	return s.as(ctx, user, "user", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE notifications SET read_at = coalesce(read_at, now()) WHERE id=$1 AND user_id=$2`, id, user)
		if err == nil && tag.RowsAffected() == 0 {
			return apperr.NotFound("Уведомление")
		}
		return err
	})
}

func (s Store) MarkAllRead(ctx context.Context, user uuid.UUID) (int64, error) {
	var n int64
	err := s.as(ctx, user, "user", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id=$1 AND read_at IS NULL`, user)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}
