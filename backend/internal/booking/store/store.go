// Package store — PostgreSQL-адаптер бронирования.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
)

type Store struct{ Pool *pgxpool.Pool }

var _ app.Store = Store{}

// InTx: READ COMMITTED + явные блокировки строк. SERIALIZABLE тоже решил бы
// гонку за последний номер, но ценой повторов всей транзакции при каждом
// конфликте; здесь конфликтующие транзакции просто ждут друг друга на
// строках остатков, а повторы (InTx) остаются только для deadlock.
func (s Store) InTx(ctx context.Context, actor auth.Principal, fn func(app.Tx) error) error {
	return pg.InTx(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, actor.UserID.String(), string(actor.Role)); err != nil {
			return err
		}
		return fn(txStore{tx: tx})
	})
}

type txStore struct{ tx pgx.Tx }

func (t txStore) ClaimIdempotency(ctx context.Context, customer uuid.UUID, key string, hash []byte) (app.IdemResult, error) {
	// INSERT ... ON CONFLICT ждёт завершения конкурирующей транзакции с тем
	// же ключом: второй запрос увидит либо готовую бронь, либо (если первый
	// откатился) займёт ключ сам. Промежуточного «в процессе» снаружи не видно.
	tag, err := t.tx.Exec(ctx, `INSERT INTO idempotency_keys(customer_id, idem_key, request_hash) VALUES ($1,$2,$3)
		ON CONFLICT DO NOTHING`, customer, key, hash)
	if err != nil {
		return app.IdemResult{}, err
	}
	if tag.RowsAffected() == 1 {
		return app.IdemResult{Fresh: true}, nil
	}
	var stored, response []byte
	if err := t.tx.QueryRow(ctx, `SELECT request_hash, response FROM idempotency_keys WHERE customer_id=$1 AND idem_key=$2`,
		customer, key).Scan(&stored, &response); err != nil {
		return app.IdemResult{}, err
	}
	if string(stored) != string(hash) {
		return app.IdemResult{HashMismatch: true}, nil
	}
	if response == nil {
		// Ключ занят, но ответа нет: первый запрос откатился с ошибкой
		// бизнес-правила уже после вставки ключа — такое невозможно, потому что
		// ключ, бронь и ответ в одной транзакции. Защитный код на случай ручных правок.
		return app.IdemResult{}, apperr.Conflict("idempotency_in_progress", "Запрос с этим ключом ещё обрабатывается")
	}
	var res domain.Reservation
	if err := json.Unmarshal(response, &res); err != nil {
		return app.IdemResult{}, fmt.Errorf("stored response: %w", err)
	}
	return app.IdemResult{Response: res}, nil
}

func (t txStore) BindIdempotency(ctx context.Context, customer uuid.UUID, key string, res domain.Reservation) error {
	body, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `UPDATE idempotency_keys SET reservation_id=$3, response=$4 WHERE customer_id=$1 AND idem_key=$2`,
		customer, key, res.ID, body)
	return err
}

func (t txStore) Offer(ctx context.Context, roomType uuid.UUID) (domain.RoomOffer, error) {
	var o domain.RoomOffer
	var rtStatus, hStatus, cur string
	err := t.tx.QueryRow(ctx, `SELECT r.room_type_id, r.hotel_id, h.name, r.name, r.capacity, r.total_rooms,
			r.price_amount, r.price_currency, r.status, h.status
		FROM room_type_replica r JOIN hotel_replica h ON h.hotel_id = r.hotel_id
		WHERE r.room_type_id = $1
		FOR SHARE OF r, h`, roomType).
		Scan(&o.RoomTypeID, &o.HotelID, &o.HotelName, &o.Name, &o.Capacity, &o.TotalRooms, &o.PricePerDay.Amount, &cur, &rtStatus, &hStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, apperr.NotFound("Категория номеров")
	}
	o.PricePerDay.Currency = strings.TrimSpace(cur)
	o.Sellable = rtStatus == "active" && hStatus == "active"
	return o, err
}

func (t txStore) LockDays(ctx context.Context, roomType uuid.UUID, stay domain.Stay) ([]app.DayLoad, error) {
	from, to := stay.CheckIn.Time(), stay.CheckOut.Time()
	if _, err := t.tx.Exec(ctx, `INSERT INTO inventory(room_type_id, day)
		SELECT $1, d::date FROM generate_series($2::date, $3::date - 1, interval '1 day') d
		ON CONFLICT DO NOTHING`, roomType, from, to); err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(ctx, `SELECT day, booked FROM inventory
		WHERE room_type_id=$1 AND day >= $2 AND day < $3 ORDER BY day FOR UPDATE`, roomType, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.DayLoad
	for rows.Next() {
		var d time.Time
		var l app.DayLoad
		if err := rows.Scan(&d, &l.Booked); err != nil {
			return nil, err
		}
		l.Day = domain.DateFromTime(d)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != stay.Nights() {
		return nil, fmt.Errorf("lock days: expected %d rows, got %d", stay.Nights(), len(out))
	}
	return out, nil
}

func (t txStore) AddBooked(ctx context.Context, roomType uuid.UUID, stay domain.Stay, delta int) error {
	tag, err := t.tx.Exec(ctx, `UPDATE inventory SET booked = booked + $4 WHERE room_type_id=$1 AND day >= $2 AND day < $3`,
		roomType, stay.CheckIn.Time(), stay.CheckOut.Time(), delta)
	if pg.IsCheckViolation(err, "") {
		return fmt.Errorf("inventory underflow for %s: %w", roomType, err)
	}
	if err == nil && int(tag.RowsAffected()) != stay.Nights() {
		return fmt.Errorf("inventory update touched %d rows, expected %d", tag.RowsAffected(), stay.Nights())
	}
	return err
}

func (t txStore) InsertReservation(ctx context.Context, r domain.Reservation) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO reservations(id, hotel_id, room_type_id, customer_id, check_in, check_out, guests, status,
		total_amount, currency, hotel_name, room_type_name, created_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		r.ID, r.HotelID, r.RoomTypeID, r.CustomerID, r.Stay.CheckIn.Time(), r.Stay.CheckOut.Time(), r.Guests, string(r.Status),
		r.Total.Amount, r.Total.Currency, r.HotelName, r.RoomTypeName, r.CreatedAt, r.Version)
	return err
}

func (t txStore) UpdateReservation(ctx context.Context, r domain.Reservation, expected int64) error {
	tag, err := t.tx.Exec(ctx, `UPDATE reservations SET status=$3, cancelled_at=$4, cancelled_by=$5, cancel_reason=$6, version=$7
		WHERE id=$1 AND version=$2`, r.ID, expected, string(r.Status), r.CancelledAt, r.CancelledBy, nullIfEmpty(r.CancelReason), r.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("version_mismatch", "Бронь изменилась параллельно, обновите страницу")
	}
	return nil
}

const resCols = `id, hotel_id, room_type_id, customer_id, check_in, check_out, guests, status, total_amount, currency,
	hotel_name, room_type_name, created_at, cancelled_at, cancelled_by, coalesce(cancel_reason, ''), version`

func scanRes(row pgx.Row) (domain.Reservation, error) {
	var r domain.Reservation
	var in, out time.Time
	var status, cur string
	err := row.Scan(&r.ID, &r.HotelID, &r.RoomTypeID, &r.CustomerID, &in, &out, &r.Guests, &status, &r.Total.Amount, &cur,
		&r.HotelName, &r.RoomTypeName, &r.CreatedAt, &r.CancelledAt, &r.CancelledBy, &r.CancelReason, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, apperr.NotFound("Бронь")
	}
	r.Stay = domain.Stay{CheckIn: domain.DateFromTime(in), CheckOut: domain.DateFromTime(out)}
	r.Status = domain.Status(status)
	r.Total.Currency = strings.TrimSpace(cur)
	return r, err
}

func (t txStore) Reservation(ctx context.Context, id uuid.UUID, forUpdate bool) (domain.Reservation, error) {
	q := `SELECT ` + resCols + ` FROM reservations WHERE id=$1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanRes(t.tx.QueryRow(ctx, q, id))
}

func (t txStore) ManagesHotel(ctx context.Context, hotel, user uuid.UUID) (bool, error) {
	var ok bool
	err := t.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hotel_manager_replica WHERE hotel_id=$1 AND user_id=$2)`, hotel, user).Scan(&ok)
	return ok, err
}

func (t txStore) Audit(ctx context.Context, actor auth.Principal, e app.AuditEntry) error {
	details, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	var actorID *uuid.UUID
	if actor.Role != auth.RoleSystem {
		actorID = &actor.UserID
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id, details) VALUES ($1,$2,$3,$4,$5,$6)`,
		e.HotelID, actorID, string(actor.Role), e.Action, e.EntityID, details)
	return err
}

func (t txStore) Outbox(ctx context.Context, hotel uuid.UUID, env messaging.Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	// Как messaging.WriteOutbox, плюс hotel_id — арендатор события.
	_, err = t.tx.Exec(ctx, `INSERT INTO outbox(topic, msg_key, envelope, event_id, aggregate_version, hotel_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		messaging.TopicReservation, env.AggregateID.String(), body, env.EventID, env.AggregateVersion, hotel)
	if err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	return nil
}

// ---------- чтение вне транзакции изменения ----------

func (s Store) ListReservations(ctx context.Context, actor auth.Principal, f app.ReservationFilter) ([]domain.Reservation, int, error) {
	var out []domain.Reservation
	var total int
	err := s.InTx(ctx, actor, func(t app.Tx) error {
		tx := t.(txStore).tx
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
			conds = append(conds, "status = "+arg(string(f.Status)))
		}
		if !f.From.IsZero() {
			conds = append(conds, "check_out > "+arg(f.From.Time()))
		}
		if !f.To.IsZero() {
			conds = append(conds, "check_in < "+arg(f.To.Time()))
		}
		where := ""
		if len(conds) > 0 {
			where = " WHERE " + strings.Join(conds, " AND ")
		}
		// Видимость по отелям задаёт сервис (ScopeHotels); RLS проверяет её
		// второй раз, независимо от этого запроса.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reservations`+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+resCols+` FROM reservations`+where+
			` ORDER BY check_in DESC, created_at DESC, id LIMIT `+arg(f.Limit)+` OFFSET `+arg(f.Offset), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []domain.Reservation{}
		for rows.Next() {
			r, err := scanRes(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, total, err
}

func (s Store) Availability(ctx context.Context, hotel uuid.UUID, stay domain.Stay, guests int) ([]app.Availability, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.room_type_id, r.name, r.capacity, r.total_rooms,
			r.total_rooms - coalesce((SELECT max(i.booked) FROM inventory i
				WHERE i.room_type_id = r.room_type_id AND i.day >= $2 AND i.day < $3), 0) AS free,
			r.price_amount, r.price_currency
		FROM room_type_replica r JOIN hotel_replica h ON h.hotel_id = r.hotel_id
		WHERE r.hotel_id = $1 AND r.status = 'active' AND h.status = 'active' AND r.capacity >= $4
		ORDER BY r.price_amount, r.name`, hotel, stay.CheckIn.Time(), stay.CheckOut.Time(), guests)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nights := int64(stay.Nights())
	out := []app.Availability{}
	for rows.Next() {
		var a app.Availability
		var cur string
		if err := rows.Scan(&a.RoomTypeID, &a.Name, &a.Capacity, &a.TotalRooms, &a.Free, &a.PerNight.Amount, &cur); err != nil {
			return nil, err
		}
		if a.Free < 0 {
			a.Free = 0 // total_rooms уменьшили ниже уже проданного
		}
		a.PerNight.Currency = strings.TrimSpace(cur)
		a.Total = domain.Money{Amount: a.PerNight.Amount * nights, Currency: a.PerNight.Currency}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s Store) AuditLog(ctx context.Context, actor auth.Principal, hotel uuid.UUID, limit, offset int) ([]app.AuditRecord, int, error) {
	var out []app.AuditRecord
	var total int
	err := s.InTx(ctx, actor, func(t app.Tx) error {
		tx := t.(txStore).tx
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE hotel_id=$1`, hotel).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, hotel_id, actor_id, actor_role, action, entity_id, details, at
			FROM audit_log WHERE hotel_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, hotel, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []app.AuditRecord{}
		for rows.Next() {
			var a app.AuditRecord
			if err := rows.Scan(&a.ID, &a.HotelID, &a.ActorID, &a.ActorRole, &a.Action, &a.EntityID, &a.Details, &a.At); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, total, err
}

func (s Store) ManagedHotelIDs(ctx context.Context, user uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `SELECT hotel_id FROM hotel_manager_replica WHERE user_id=$1`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
