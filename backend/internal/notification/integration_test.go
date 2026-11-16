package notification_test

// РХД ПР-5: надёжный потребитель событий брони. Inbox, проверка версии и
// уведомление — одна транзакция под ролью данных с RLS. Сценарии: повторная
// доставка, устаревшее событие, битое событие (→ DLQ), изоляция адресатов.
// Сам потребитель Kafka (DLQ, временные ошибки, падение после коммита)
// проверяется в internal/platform/messaging.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shinilornekora/tourflow/backend/internal/notification/app"
	"github.com/shinilornekora/tourflow/backend/internal/notification/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/notification/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

type countingChannel struct{ sent int }

func (c *countingChannel) Send(context.Context, app.Notification) error { c.sent++; return nil }

type env struct {
	pools pg.Pools
	svc   *app.Service
	ch    *countingChannel
}

func setup(t *testing.T) *env {
	pools := pgtest.FreshPools(t, "notification_test", migrations.FS)
	ch := &countingChannel{}
	return &env{pools: pools, ch: ch, svc: &app.Service{Store: store.Store{Pool: pools.App}, Channel: ch,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

func event(t *testing.T, typ string, reservation, customer uuid.UUID, version int64) messaging.Envelope {
	t.Helper()
	payload := map[string]any{"reservation_id": reservation, "customer_id": customer, "hotel_name": "Гранд Отель",
		"room_type_name": "Стандарт", "check_in": "2026-12-05", "check_out": "2026-12-07",
		"total": map[string]any{"amount": 1000000, "currency": "RUB"}, "reason": "Отменено гостем", "cancelled_by_role": "customer"}
	ev, err := messaging.NewEnvelope(context.Background(), "booking", typ, reservation, version, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func as(id uuid.UUID) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{UserID: id, Role: auth.RoleCustomer})
}

func (e *env) list(t *testing.T, user uuid.UUID) []app.Notification {
	t.Helper()
	l, _, err := e.svc.List(as(user), false, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Повторная доставка и «падение после коммита»: то же событие приходит
// снова (брокер не получил подтверждение) — уведомление одно, письмо одно.
func TestRedeliveryCreatesOneNotification(t *testing.T) {
	e := setup(t)
	alice, res := uuid.New(), uuid.New()
	ev := event(t, "ReservationConfirmed", res, alice, 1)
	for i := 0; i < 3; i++ {
		if err := e.svc.HandleReservationEvent(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if l := e.list(t, alice); len(l) != 1 || l[0].Kind != "reservation_confirmed" {
		t.Fatalf("one notification expected: %+v", l)
	}
	if e.ch.sent != 1 {
		t.Fatalf("external channel must fire once, fired %d", e.ch.sent)
	}
	var inbox int
	if err := e.pools.Owner.QueryRow(context.Background(), `SELECT count(*) FROM processed_events`).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("inbox: %d %v", inbox, err)
	}
}

// Порядок между репликами ретранслятора не гарантирован: «подтверждена»
// (v1) может прийти после «отменена» (v2). Устаревшее событие отмечается
// обработанным, но уведомления не порождает.
func TestStaleEventIsNotDelivered(t *testing.T) {
	e := setup(t)
	alice, res := uuid.New(), uuid.New()
	ctx := context.Background()
	if err := e.svc.HandleReservationEvent(ctx, event(t, "ReservationCancelled", res, alice, 2)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleReservationEvent(ctx, event(t, "ReservationConfirmed", res, alice, 1)); err != nil {
		t.Fatal(err)
	}
	l := e.list(t, alice)
	if len(l) != 1 || l[0].Kind != "reservation_cancelled" || e.ch.sent != 1 {
		t.Fatalf("late v1 must not notify: %+v sent=%d", l, e.ch.sent)
	}
}

// Битое событие — постоянная ошибка (потребитель положит его в DLQ), следов
// в базе нет.
func TestBrokenEventIsPermanent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	bad := event(t, "ReservationConfirmed", uuid.New(), uuid.New(), 1)
	bad.Payload = json.RawMessage(`{"customer_id": 42`)
	if err := e.svc.HandleReservationEvent(ctx, bad); !messaging.IsPermanent(err) {
		t.Fatalf("broken payload must be permanent, got %v", err)
	}
	noCustomer := event(t, "ReservationConfirmed", uuid.New(), uuid.Nil, 1)
	if err := e.svc.HandleReservationEvent(ctx, noCustomer); !messaging.IsPermanent(err) {
		t.Fatalf("event without customer must be permanent, got %v", err)
	}
	var inbox int
	if err := e.pools.Owner.QueryRow(ctx, `SELECT count(*) FROM processed_events`).Scan(&inbox); err != nil || inbox != 0 {
		t.Fatalf("broken events left traces: %d %v", inbox, err)
	}
}

// Изоляция адресатов (ПР-2/ПР-5): RLS под ролью данных.
func TestNotificationsIsolatedByRLS(t *testing.T) {
	e := setup(t)
	alice, bob := uuid.New(), uuid.New()
	ctx := context.Background()
	if err := e.svc.HandleReservationEvent(ctx, event(t, "ReservationConfirmed", uuid.New(), alice, 1)); err != nil {
		t.Fatal(err)
	}
	own := e.list(t, alice)
	if len(own) != 1 {
		t.Fatalf("alice must see her notification: %+v", own)
	}
	if l := e.list(t, bob); len(l) != 0 {
		t.Fatalf("bob sees alice's notifications: %+v", l)
	}
	if err := e.svc.MarkRead(as(bob), own[0].ID); err == nil {
		t.Fatal("bob marked alice's notification as read")
	}
	raw := func(user, role, sql string, args ...any) (int64, error) {
		var n int64
		err := pgx.BeginFunc(ctx, e.pools.App, func(tx pgx.Tx) error {
			if role != "" {
				if err := pg.SetActor(ctx, tx, user, role); err != nil {
					return err
				}
			}
			tag, err := tx.Exec(ctx, sql, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	// Прямой SQL в обход сервиса: чужое не видно, без контекста — ноль,
	// записать уведомление другому нельзя.
	if n, err := raw(bob.String(), "user", `SELECT * FROM notifications`); err != nil || n != 0 {
		t.Fatalf("bob raw select: %d %v", n, err)
	}
	if n, err := raw("", "", `SELECT * FROM notifications`); err != nil || n != 0 {
		t.Fatalf("no context: %d %v", n, err)
	}
	if n, err := raw(bob.String(), "user", `UPDATE notifications SET title = 'взлом'`); err != nil || n != 0 {
		t.Fatalf("bob raw update: %d %v", n, err)
	}
	_, err := raw(bob.String(), "user", `INSERT INTO notifications(id, user_id, kind, title, body, source_event)
		VALUES (gen_random_uuid(), $1, 'spam', 'x', 'y', gen_random_uuid())`, alice)
	if code(err) != "42501" {
		t.Fatalf("bob wrote a notification for alice: %v", err)
	}
	if pgtest.Split() {
		for _, q := range []string{`ALTER TABLE notifications DISABLE ROW LEVEL SECURITY`, `DROP POLICY notifications_own ON notifications`} {
			if _, err := e.pools.App.Exec(ctx, q); code(err) != "42501" {
				t.Fatalf("data role ran %q: %v", q, err)
			}
		}
	}
}
