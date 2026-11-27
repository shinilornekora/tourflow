// Package store — PostgreSQL-адаптер модели чтения (отдельный экземпляр).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/app"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

// Store: Pool — роль данных (API чтения), Owner — владелец (проектор).
// В тестах и без APP_DATABASE_URL это один пул.
type Store struct {
	Pool  *pgxpool.Pool
	Owner *pgxpool.Pool
}

var _ app.Store = Store{}

func (s Store) owner() *pgxpool.Pool {
	if s.Owner != nil {
		return s.Owner
	}
	return s.Pool
}

// read — транзакция только для чтения с контекстом RLS.
func (s Store) read(ctx context.Context, actor auth.Principal, fn func(pgx.Tx) error) error {
	return pg.InTx(ctx, s.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, actor.UserID.String(), string(actor.Role)); err != nil {
			return err
		}
		return fn(tx)
	})
}

// ---------- проектор ----------

type projTx struct{ tx pgx.Tx }

func (s Store) InProjectionTx(ctx context.Context, fn func(app.ProjectionTx) error) error {
	return pg.InTx(ctx, s.owner(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		return fn(projTx{tx: tx})
	})
}

func (p projTx) MarkProcessed(ctx context.Context, env messaging.Envelope) (bool, error) {
	tag, err := p.tx.Exec(ctx, `INSERT INTO projection_inbox(event_id, event_type) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		env.EventID, env.EventType)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p projTx) UpsertCard(ctx context.Context, c app.Card) (bool, error) {
	var created *time.Time
	if !c.CreatedAt.IsZero() {
		created = &c.CreatedAt
	}
	// created_at неизвестен, только если первым пришло событие отмены
	// старого формата: временно берём момент отмены, событие
	// подтверждения (версия ниже) карточку уже не изменит.
	tag, err := p.tx.Exec(ctx, `INSERT INTO reservations_cards(reservation_id, hotel_id, hotel_name, room_type_id, room_type_name,
			customer_id, check_in, check_out, guests, status, total_amount, currency, created_at, cancelled_at, cancel_reason,
			cancelled_by_role, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,coalesce($13, $14, now()),$14,nullif($15,''),nullif($16,''),$17)
		ON CONFLICT (reservation_id) DO UPDATE SET
			hotel_id=EXCLUDED.hotel_id, hotel_name=EXCLUDED.hotel_name, room_type_id=EXCLUDED.room_type_id,
			room_type_name=EXCLUDED.room_type_name, customer_id=EXCLUDED.customer_id, check_in=EXCLUDED.check_in,
			check_out=EXCLUDED.check_out, guests=EXCLUDED.guests, status=EXCLUDED.status,
			total_amount=EXCLUDED.total_amount, currency=EXCLUDED.currency,
			created_at=coalesce($13, reservations_cards.created_at),
			cancelled_at=EXCLUDED.cancelled_at, cancel_reason=EXCLUDED.cancel_reason,
			cancelled_by_role=EXCLUDED.cancelled_by_role, version=EXCLUDED.version, projected_at=now()
		WHERE reservations_cards.version < EXCLUDED.version`,
		c.ID, c.HotelID, c.HotelName, c.RoomTypeID, c.RoomTypeName, c.CustomerID, c.CheckIn, c.CheckOut, c.Guests,
		c.Status, c.Total.Amount, c.Total.Currency, created, c.CancelledAt, c.CancelReason, c.CancelledByRole, c.Version)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p projTx) ApplyHotel(ctx context.Context, s app.HotelSnapshot) (bool, error) {
	tag, err := p.tx.Exec(ctx, `INSERT INTO hotel_versions(hotel_id, name, city, status, version) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (hotel_id) DO UPDATE SET name=EXCLUDED.name, city=EXCLUDED.city, status=EXCLUDED.status, version=EXCLUDED.version
		WHERE hotel_versions.version < EXCLUDED.version`, s.HotelID, s.Name, s.City, s.Status, s.Version)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err // устаревший снимок: менеджеров не трогаем
	}
	ids := s.ManagerIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	if _, err := p.tx.Exec(ctx, `DELETE FROM hotel_managers WHERE hotel_id=$1 AND NOT (user_id = ANY($2))`, s.HotelID, ids); err != nil {
		return false, err
	}
	if _, err := p.tx.Exec(ctx, `INSERT INTO hotel_managers(hotel_id, user_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`,
		s.HotelID, ids); err != nil {
		return false, err
	}
	return true, nil
}

// PurgeInbox удаляет старые отметки inbox. Повтор старше срока хранения
// топика прийти уже не может.
func (s Store) PurgeInbox(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.owner().Exec(ctx, `DELETE FROM projection_inbox WHERE processed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	return tag.RowsAffected(), err
}

// ---------- чтение ----------

const cardCols = `reservation_id, hotel_id, hotel_name, room_type_id, room_type_name, customer_id,
	to_char(check_in, 'YYYY-MM-DD'), to_char(check_out, 'YYYY-MM-DD'), nights, guests, status, total_amount, trim(currency),
	created_at, cancelled_at, coalesce(cancel_reason, ''), coalesce(cancelled_by_role, ''), version`

func scanCard(row pgx.Row) (app.Card, error) {
	var c app.Card
	err := row.Scan(&c.ID, &c.HotelID, &c.HotelName, &c.RoomTypeID, &c.RoomTypeName, &c.CustomerID, &c.CheckIn, &c.CheckOut,
		&c.Nights, &c.Guests, &c.Status, &c.Total.Amount, &c.Total.Currency, &c.CreatedAt, &c.CancelledAt, &c.CancelReason,
		&c.CancelledByRole, &c.Version)
	return c, err
}

// where строит условия выборки. Видимость по отелям задаёт сервис
// (ScopeHotels); RLS проверяет её второй раз, независимо от запроса.
func where(f app.Filter) (string, []any) {
	var conds []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if f.CustomerID != uuid.Nil {
		conds = append(conds, "customer_id = "+arg(f.CustomerID))
	}
	if f.HotelID != uuid.Nil {
		conds = append(conds, "hotel_id = "+arg(f.HotelID))
	}
	if f.ScopeHotels {
		conds = append(conds, "hotel_id = ANY("+arg(f.HotelIDs)+")")
	}
	if f.Status != "" {
		conds = append(conds, "status = "+arg(f.Status))
	}
	if !f.From.IsZero() {
		conds = append(conds, "check_out > "+arg(f.From.Format(time.DateOnly))+"::date")
	}
	if !f.To.IsZero() {
		conds = append(conds, "check_in < "+arg(f.To.Format(time.DateOnly))+"::date")
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s Store) List(ctx context.Context, actor auth.Principal, f app.Filter) ([]app.Card, int, error) {
	out := []app.Card{}
	var total int
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		out = out[:0]
		w, args := where(f)
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reservations_cards`+w, args...).Scan(&total); err != nil {
			return err
		}
		n := len(args)
		args = append(args, f.Limit, f.Offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM reservations_cards%s
			ORDER BY check_in DESC, created_at DESC, reservation_id LIMIT $%d OFFSET $%d`, cardCols, w, n+1, n+2), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCard(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, total, err
}

func (s Store) Get(ctx context.Context, actor auth.Principal, id uuid.UUID) (app.Card, error) {
	var c app.Card
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		var err error
		c, err = scanCard(tx.QueryRow(ctx, `SELECT `+cardCols+` FROM reservations_cards WHERE reservation_id=$1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("Бронь")
		}
		return err
	})
	return c, err
}

func (s Store) ManagedHotelIDs(ctx context.Context, actor auth.Principal) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT hotel_id FROM hotel_managers WHERE user_id=$1 ORDER BY hotel_id`, actor.UserID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		out = append(out, ids...)
		return err
	})
	return out, err
}

func (s Store) HotelSummary(ctx context.Context, actor auth.Principal, f app.Filter, today time.Time) (app.HotelSummary, error) {
	sum := app.HotelSummary{Revenue: app.Money{Currency: "RUB"}, ByHotel: []app.HotelStats{}}
	f.Status = "" // сводка считает оба статуса
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		w, args := where(f)
		args = append(args, today.Format(time.DateOnly))
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT hotel_id, max(hotel_name),
				count(*) FILTER (WHERE status='confirmed'),
				count(*) FILTER (WHERE status='cancelled'),
				count(*) FILTER (WHERE status='confirmed' AND check_in >= $%d::date),
				coalesce(sum(nights) FILTER (WHERE status='confirmed'), 0),
				coalesce(sum(guests) FILTER (WHERE status='confirmed'), 0),
				coalesce(sum(total_amount) FILTER (WHERE status='confirmed'), 0)
			FROM reservations_cards%s GROUP BY hotel_id ORDER BY 8 DESC, 1`, len(args), w), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h app.HotelStats
			var upcoming, guests int
			if err := rows.Scan(&h.HotelID, &h.HotelName, &h.Confirmed, &h.Cancelled, &upcoming, &h.Nights, &guests, &h.Revenue.Amount); err != nil {
				return err
			}
			h.Revenue.Currency = "RUB"
			sum.Confirmed += h.Confirmed
			sum.Cancelled += h.Cancelled
			sum.Upcoming += upcoming
			sum.Nights += h.Nights
			sum.Guests += guests
			sum.Revenue.Amount += h.Revenue.Amount
			sum.ByHotel = append(sum.ByHotel, h)
		}
		return rows.Err()
	})
	return sum, err
}

func (s Store) CustomerSummary(ctx context.Context, actor auth.Principal, customer uuid.UUID, today time.Time) (app.CustomerSummary, error) {
	sum := app.CustomerSummary{TotalSpent: app.Money{Currency: "RUB"}}
	err := s.read(ctx, actor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE status='confirmed' AND check_in > $2::date),
				count(*) FILTER (WHERE status='confirmed' AND check_in <= $2::date AND check_out > $2::date),
				count(*) FILTER (WHERE status='confirmed' AND check_out <= $2::date),
				count(*) FILTER (WHERE status='cancelled'),
				coalesce(sum(total_amount) FILTER (WHERE status='confirmed'), 0),
				to_char(min(check_in) FILTER (WHERE status='confirmed' AND check_in > $2::date), 'YYYY-MM-DD')
			FROM reservations_cards WHERE customer_id=$1`, customer, today.Format(time.DateOnly)).
			Scan(&sum.Upcoming, &sum.InProgress, &sum.Completed, &sum.Cancelled, &sum.TotalSpent.Amount, &sum.NextCheckIn)
	})
	return sum, err
}
