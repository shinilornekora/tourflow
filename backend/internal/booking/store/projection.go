package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

var _ app.ProjectionStore = Store{}

type projTx struct{ txStore }

func (s Store) InProjectionTx(ctx context.Context, fn func(app.ProjectionTx) error) error {
	return pg.InTx(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		return fn(projTx{txStore{tx: tx}})
	})
}

func (p projTx) MarkProcessed(ctx context.Context, env messaging.Envelope) (bool, error) {
	return messaging.MarkProcessed(ctx, p.tx, env)
}

func (p projTx) ApplyHotel(ctx context.Context, s app.HotelSnapshot) (bool, string, error) {
	var prev string
	err := p.tx.QueryRow(ctx, `SELECT status FROM hotel_replica WHERE hotel_id=$1 FOR UPDATE`, s.HotelID).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}
	tag, err := p.tx.Exec(ctx, `INSERT INTO hotel_replica(hotel_id, name, city, status, version) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (hotel_id) DO UPDATE SET name=EXCLUDED.name, city=EXCLUDED.city, status=EXCLUDED.status, version=EXCLUDED.version
		WHERE hotel_replica.version < EXCLUDED.version`, s.HotelID, s.Name, s.City, s.Status, s.Version)
	if err != nil {
		return false, "", err
	}
	if tag.RowsAffected() == 0 {
		return false, prev, nil // устаревший снимок
	}
	if _, err := p.tx.Exec(ctx, `DELETE FROM hotel_manager_replica WHERE hotel_id=$1 AND NOT (user_id = ANY($2))`, s.HotelID, s.ManagerIDs); err != nil {
		return false, "", err
	}
	if _, err := p.tx.Exec(ctx, `INSERT INTO hotel_manager_replica(hotel_id, user_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`,
		s.HotelID, s.ManagerIDs); err != nil {
		return false, "", err
	}
	return true, prev, nil
}

func (p projTx) ApplyRoomType(ctx context.Context, s app.RoomTypeSnapshot) (bool, error) {
	cur := s.Price.Currency
	if cur == "" {
		cur = "RUB"
	}
	tag, err := p.tx.Exec(ctx, `INSERT INTO room_type_replica(room_type_id, hotel_id, name, capacity, total_rooms, price_amount, price_currency, status, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (room_type_id) DO UPDATE SET hotel_id=EXCLUDED.hotel_id, name=EXCLUDED.name, capacity=EXCLUDED.capacity,
			total_rooms=EXCLUDED.total_rooms, price_amount=EXCLUDED.price_amount, price_currency=EXCLUDED.price_currency,
			status=EXCLUDED.status, version=EXCLUDED.version
		WHERE room_type_replica.version < EXCLUDED.version`,
		s.RoomTypeID, s.HotelID, s.Name, s.Capacity, s.TotalRooms, s.Price.Amount, cur, s.Status, s.Version)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p projTx) FutureConfirmed(ctx context.Context, hotel uuid.UUID, after domain.Date) ([]uuid.UUID, error) {
	rows, err := p.tx.Query(ctx, `SELECT id FROM reservations WHERE hotel_id=$1 AND status='confirmed' AND check_in > $2
		ORDER BY check_in, id`, hotel, after.Time())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// PurgeIdempotency удаляет ключи старше ttl: повтор через сутки — это уже
// новый запрос, а таблица не должна расти бесконечно. Служебная задача:
// идёт от имени system (политика RLS ключей пускает к чужим строкам только её).
func (s Store) PurgeIdempotency(ctx context.Context, ttl time.Duration) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1)`, ttl.Seconds())
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

// Mismatch — расхождение остатков с бронями на конкретные сутки.
type Mismatch struct {
	RoomTypeID uuid.UUID
	Day        time.Time
	Booked     int
	Confirmed  int
}

// Reconcile — фоновая сверка инварианта «inventory.booked = число
// подтверждённых броней, покрывающих эти сутки» (РХД ПР-1: третий уровень
// защиты после схемы и транзакции; ПКП-5: это сверка внутри одного
// владельца данных, а не фоновая синхронизация чужих таблиц). Ничего не
// исправляет: расхождение — признак дефекта, его записывают в
// reconciliation_issues и в журнал, решает человек.
func (s Store) Reconcile(ctx context.Context) ([]Mismatch, error) {
	var out []Mismatch
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH actual AS (
				SELECT r.room_type_id, d::date AS day, count(*)::int AS confirmed
				FROM reservations r, generate_series(r.check_in, r.check_out - 1, interval '1 day') d
				WHERE r.status = 'confirmed'
				GROUP BY 1, 2)
			SELECT coalesce(i.room_type_id, a.room_type_id), coalesce(i.day, a.day),
			       coalesce(i.booked, 0), coalesce(a.confirmed, 0)
			FROM inventory i FULL JOIN actual a ON a.room_type_id = i.room_type_id AND a.day = i.day
			WHERE coalesce(i.booked, 0) <> coalesce(a.confirmed, 0)
			ORDER BY 1, 2 LIMIT 1000`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mismatch, error) {
			var m Mismatch
			return m, r.Scan(&m.RoomTypeID, &m.Day, &m.Booked, &m.Confirmed)
		})
		if err != nil {
			return err
		}
		for _, m := range out {
			if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_issues(room_type_id, day, booked, confirmed) VALUES ($1,$2,$3,$4)`,
				m.RoomTypeID, m.Day, m.Booked, m.Confirmed); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// PurgeDelivered чистит служебные таблицы доставки: опубликованные события
// outbox и отметки inbox старше ttl. Окно дедупликации inbox равно ttl:
// повтор события старше этого срока уже не отсеется, поэтому ttl берётся с
// большим запасом над сроком хранения сообщений в Kafka (7 суток).
func (s Store) PurgeDelivered(ctx context.Context, ttl time.Duration) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.relay', 'on', true)`); err != nil {
			return err
		}
		a, err := tx.Exec(ctx, `DELETE FROM outbox WHERE published_at < now() - make_interval(secs => $1)`, ttl.Seconds())
		if err != nil {
			return err
		}
		b, err := tx.Exec(ctx, `DELETE FROM processed_events WHERE processed_at < now() - make_interval(secs => $1)`, ttl.Seconds())
		n = a.RowsAffected() + b.RowsAffected()
		return err
	})
	return n, err
}
