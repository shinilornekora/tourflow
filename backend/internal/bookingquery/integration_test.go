package bookingquery_test

// Модель чтения броней на настоящем PostgreSQL: проектор под владельцем,
// API — под ролью данных с RLS (TEST_PG_APP_DSN), как в бою.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/app"
	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/bookingquery/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

type env struct {
	pools pg.Pools
	svc   *app.Service
}

// «Сегодня» в тестах — 1 декабря 2026.
var today = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) *env {
	pools := pgtest.FreshPools(t, "bookingquery_test", migrations.FS, migrations.Deny...)
	return &env{pools: pools, svc: &app.Service{
		Store: store.Store{Pool: pools.App, Owner: pools.Owner}, Location: time.UTC,
		Now: func() time.Time { return today },
	}}
}

func as(id uuid.UUID, role auth.Role) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{UserID: id, Role: role})
}

type res struct {
	id, hotel, customer uuid.UUID
	in, out             string
	amount              int64
}

func (e *env) emit(t *testing.T, r res, typ string, version int64) {
	t.Helper()
	status := app.StatusConfirmed
	if typ == "ReservationCancelled" {
		status = app.StatusCancelled
	}
	p := map[string]any{
		"reservation_id": r.id, "hotel_id": r.hotel, "hotel_name": "Отель " + r.hotel.String()[:4], "room_type_id": uuid.New(),
		"room_type_name": "Стандарт", "customer_id": r.customer, "check_in": r.in, "check_out": r.out, "guests": 2,
		"total": map[string]any{"amount": r.amount, "currency": "RUB"}, "status": status,
		"created_at": today.Add(-time.Hour),
	}
	if status == app.StatusCancelled {
		p["reason"], p["cancelled_by_role"], p["cancelled_at"] = "Отменено гостем", "customer", today
	}
	ev, err := messaging.NewEnvelope(context.Background(), "booking", typ, r.id, version, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func (e *env) hotel(t *testing.T, hotel uuid.UUID, version int64, managers ...uuid.UUID) {
	t.Helper()
	ev, err := messaging.NewEnvelope(context.Background(), "catalog", "HotelUpserted", hotel, version, map[string]any{
		"hotel_id": hotel, "name": "Отель", "city": "Казань", "status": "active", "manager_ids": managers, "version": version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func (e *env) get(t *testing.T, ctx context.Context, id uuid.UUID) app.Card {
	t.Helper()
	c, err := e.svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func isNotFound(err error) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Kind == apperr.KindNotFound
}

// Повторная доставка и устаревшее событие: «подтверждена» (v1), пришедшая
// после «отменена» (v2), карточку не воскрешает; повтор ничего не меняет.
func TestProjectionIsIdempotentAndOrdered(t *testing.T) {
	e := setup(t)
	alice := uuid.New()
	r := res{id: uuid.New(), hotel: uuid.New(), customer: alice, in: "2026-12-10", out: "2026-12-12", amount: 900000}
	e.emit(t, r, "ReservationCancelled", 2)
	e.emit(t, r, "ReservationConfirmed", 1)
	c := e.get(t, as(alice, auth.RoleCustomer), r.id)
	if c.Status != app.StatusCancelled || c.Version != 2 || c.CancelReason != "Отменено гостем" {
		t.Fatalf("stale event overwrote the card: %+v", c)
	}

	r2 := res{id: uuid.New(), hotel: r.hotel, customer: alice, in: "2026-12-20", out: "2026-12-21", amount: 450000}
	ev, _ := messaging.NewEnvelope(context.Background(), "booking", "ReservationConfirmed", r2.id, 1, map[string]any{
		"reservation_id": r2.id, "hotel_id": r2.hotel, "hotel_name": "Отель", "room_type_id": uuid.New(), "room_type_name": "Люкс",
		"customer_id": alice, "check_in": r2.in, "check_out": r2.out, "guests": 1, "total": map[string]any{"amount": r2.amount},
	})
	for i := 0; i < 3; i++ {
		if err := e.svc.Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	var inbox int
	if err := e.pools.Owner.QueryRow(context.Background(), `SELECT count(*) FROM projection_inbox WHERE event_id=$1`, ev.EventID).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 {
		t.Fatalf("inbox rows for one event: %d", inbox)
	}
	list, total, err := e.svc.My(as(alice, auth.RoleCustomer), app.Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(list) != 2 || list[0].ID != r2.id || list[0].Nights != 1 || list[0].Total.Currency != "RUB" {
		t.Fatalf("my list: total=%d %+v", total, list)
	}
}

// Изоляция: клиент видит только свои брони, менеджер — брони своих отелей
// (назначения приходят событиями каталога), админ — все. Чужая бронь
// неотличима от несуществующей.
func TestTenancy(t *testing.T) {
	e := setup(t)
	alice, bob, mgr, admin := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	h1, h2 := uuid.New(), uuid.New()
	e.hotel(t, h1, 1, mgr)
	e.hotel(t, h2, 1)
	ra := res{id: uuid.New(), hotel: h1, customer: alice, in: "2026-12-10", out: "2026-12-12", amount: 100000}
	rb := res{id: uuid.New(), hotel: h2, customer: bob, in: "2026-12-10", out: "2026-12-11", amount: 200000}
	e.emit(t, ra, "ReservationConfirmed", 1)
	e.emit(t, rb, "ReservationConfirmed", 1)

	if _, err := e.svc.Get(as(bob, auth.RoleCustomer), ra.id); !isNotFound(err) {
		t.Fatalf("bob reads alice's reservation: %v", err)
	}
	if _, err := e.svc.Get(as(mgr, auth.RoleManager), rb.id); !isNotFound(err) {
		t.Fatalf("manager reads other hotel's reservation: %v", err)
	}
	if c := e.get(t, as(mgr, auth.RoleManager), ra.id); c.ID != ra.id {
		t.Fatal("manager can't read own hotel's reservation")
	}
	_, total, err := e.svc.HotelReservations(as(mgr, auth.RoleManager), app.Filter{Limit: 10})
	if err != nil || total != 1 {
		t.Fatalf("manager list: total=%d err=%v", total, err)
	}
	_, total, err = e.svc.HotelReservations(as(mgr, auth.RoleManager), app.Filter{HotelID: h2, Limit: 10})
	if err != nil || total != 0 {
		t.Fatalf("manager asked for other hotel: total=%d err=%v", total, err)
	}
	_, total, err = e.svc.HotelReservations(as(admin, auth.RoleAdmin), app.Filter{Limit: 10})
	if err != nil || total != 2 {
		t.Fatalf("admin list: total=%d err=%v", total, err)
	}
	if _, _, err := e.svc.HotelReservations(as(alice, auth.RoleCustomer), app.Filter{Limit: 10}); err == nil {
		t.Fatal("customer listed hotel reservations")
	}
	if _, _, err := e.svc.My(as(mgr, auth.RoleManager), app.Filter{Limit: 10}); err == nil {
		t.Fatal("manager used customer's cabinet")
	}

	// Менеджера сняли (v2), затем запоздало пришёл старый снимок (v1) —
	// доступ не возвращается.
	e.hotel(t, h1, 2)
	e.hotel(t, h1, 1, mgr)
	if _, err := e.svc.Get(as(mgr, auth.RoleManager), ra.id); !isNotFound(err) {
		t.Fatalf("removed manager still reads reservation: %v", err)
	}
}

// Второй уровень защиты: RLS отсекает чужие строки, даже если в запросе
// забыли условие, а роль данных не может писать проекцию вовсе.
func TestRLSAndReadOnlyRole(t *testing.T) {
	e := setup(t)
	alice, bob := uuid.New(), uuid.New()
	h := uuid.New()
	e.emit(t, res{id: uuid.New(), hotel: h, customer: alice, in: "2026-12-10", out: "2026-12-12", amount: 1}, "ReservationConfirmed", 1)
	e.emit(t, res{id: uuid.New(), hotel: h, customer: bob, in: "2026-12-10", out: "2026-12-12", amount: 1}, "ReservationConfirmed", 1)
	ctx := context.Background()

	count := func(user uuid.UUID, role string) int {
		var n int
		err := pg.InTx(ctx, e.pools.App, pgx.TxOptions{}, func(tx pgx.Tx) error {
			if err := pg.SetActor(ctx, tx, user.String(), role); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM reservations_cards`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(alice, "customer"); n != 1 {
		t.Fatalf("alice sees %d cards without WHERE, want 1", n)
	}
	if n := count(uuid.New(), "hotel_manager"); n != 0 {
		t.Fatalf("unassigned manager sees %d cards", n)
	}
	if n := count(uuid.Nil, ""); n != 0 {
		t.Fatalf("no context sees %d cards (must be fail-closed)", n)
	}

	if !pgtest.Split() {
		t.Skip("TEST_PG_APP_DSN is not set: data role checks skipped")
	}
	// Даже объявив себя проектором, роль данных писать не может: права.
	err := pg.InTx(ctx, e.pools.App, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), "system"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE reservations_cards SET status='cancelled', cancelled_at=now()`)
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatalf("data role updated the projection: %v", err)
	}
	err = pg.InTx(ctx, e.pools.App, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO hotel_managers(hotel_id, user_id) VALUES ($1, $2)`, h, alice)
		return err
	})
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatalf("data role assigned itself as manager: %v", err)
	}
}

func TestSummaries(t *testing.T) {
	e := setup(t)
	alice, mgr := uuid.New(), uuid.New()
	h1, h2 := uuid.New(), uuid.New()
	e.hotel(t, h1, 1, mgr)
	e.hotel(t, h2, 1, mgr)
	past := res{id: uuid.New(), hotel: h1, customer: alice, in: "2026-11-01", out: "2026-11-03", amount: 200000}
	now := res{id: uuid.New(), hotel: h1, customer: alice, in: "2026-11-30", out: "2026-12-02", amount: 300000}
	soon := res{id: uuid.New(), hotel: h2, customer: alice, in: "2026-12-05", out: "2026-12-08", amount: 600000}
	later := res{id: uuid.New(), hotel: h2, customer: alice, in: "2026-12-20", out: "2026-12-21", amount: 100000}
	gone := res{id: uuid.New(), hotel: h2, customer: alice, in: "2026-12-10", out: "2026-12-11", amount: 999999}
	for _, r := range []res{past, now, soon, later, gone} {
		e.emit(t, r, "ReservationConfirmed", 1)
	}
	e.emit(t, gone, "ReservationCancelled", 2)

	my, err := e.svc.MySummary(as(alice, auth.RoleCustomer))
	if err != nil {
		t.Fatal(err)
	}
	if my.Upcoming != 2 || my.InProgress != 1 || my.Completed != 1 || my.Cancelled != 1 ||
		my.TotalSpent.Amount != 1200000 || my.NextCheckIn == nil || *my.NextCheckIn != "2026-12-05" {
		t.Fatalf("customer summary: %+v", my)
	}

	sum, err := e.svc.HotelSummary(as(mgr, auth.RoleManager), app.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Confirmed != 4 || sum.Cancelled != 1 || sum.Upcoming != 2 || sum.Nights != 8 || sum.Revenue.Amount != 1200000 || len(sum.ByHotel) != 2 {
		t.Fatalf("hotel summary: %+v", sum)
	}
	if sum.ByHotel[0].HotelID != h2 || sum.ByHotel[0].Revenue.Amount != 700000 || sum.ByHotel[0].Cancelled != 1 {
		t.Fatalf("by hotel (sorted by revenue): %+v", sum.ByHotel)
	}
	// Период: проживания, пересекающие декабрь.
	dec, err := e.svc.HotelSummary(as(mgr, auth.RoleManager), app.Filter{
		HotelID: h1, From: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Confirmed != 1 || dec.Revenue.Amount != 300000 {
		t.Fatalf("december summary for hotel 1: %+v", dec)
	}
	other, err := e.svc.HotelSummary(as(uuid.New(), auth.RoleManager), app.Filter{})
	if err != nil || other.Confirmed != 0 || len(other.ByHotel) != 0 {
		t.Fatalf("manager without hotels: %+v %v", other, err)
	}
}
