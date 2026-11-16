package booking_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/booking/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/booking/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

// fixedNow — «сегодня» 1 декабря 2026 по Москве.
var fixedNow = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)

type env struct {
	pool    *pgxpool.Pool // роль данных — под ней работает код сервиса
	owner   *pgxpool.Pool // владелец таблиц: ретранслятор и служебные проверки
	store   store.Store
	sys     store.Store // служебные процессы (проекция каталога) — под владельцем
	svc     *app.Service
	admin   uuid.UUID // администратор каталога: автор изменений отеля в событиях
	hotel   uuid.UUID
	room    uuid.UUID
	manager uuid.UUID
}

func setup(t *testing.T, totalRooms int) *env {
	pools := pgtest.FreshPools(t, "booking_test", migrations.FS, migrations.Deny...)
	pool := pools.App
	msk, _ := time.LoadLocation("Europe/Moscow")
	e := &env{pool: pool, owner: pools.Owner, store: store.Store{Pool: pool}, sys: store.Store{Pool: pools.Owner}, hotel: uuid.New(), room: uuid.New(), manager: uuid.New(), admin: uuid.New()}
	e.svc = &app.Service{Store: e.store, Location: msk, Now: func() time.Time { return fixedNow }}
	e.applyHotel(t, "active", 1)
	e.applyRoom(t, totalRooms, "active", 1)
	return e
}

func (e *env) event(t *testing.T, typ string, agg uuid.UUID, version int64, payload any) messaging.Envelope {
	t.Helper()
	ev, err := messaging.NewEnvelope(context.Background(), "catalog", typ, agg, version, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func (e *env) applyHotel(t *testing.T, status string, version int64) {
	t.Helper()
	ev := e.event(t, "HotelUpserted", e.hotel, version, app.HotelSnapshot{
		HotelID: e.hotel, Name: "Гранд Отель", City: "Казань", Status: status, ManagerIDs: []uuid.UUID{e.manager}, Version: version,
		ChangedBy: &e.admin,
	})
	if err := e.svc.HandleCatalogEvent(context.Background(), e.sys, ev); err != nil {
		t.Fatalf("apply hotel: %v", err)
	}
}

func (e *env) applyRoom(t *testing.T, total int, status string, version int64) {
	t.Helper()
	ev := e.event(t, "RoomTypeUpserted", e.room, version, app.RoomTypeSnapshot{
		RoomTypeID: e.room, HotelID: e.hotel, Name: "Стандарт", Capacity: 2, TotalRooms: total,
		Price: domain.Money{Amount: 500000, Currency: "RUB"}, Status: status, Version: version,
	})
	if err := e.svc.HandleCatalogEvent(context.Background(), e.sys, ev); err != nil {
		t.Fatalf("apply room: %v", err)
	}
}

func as(id uuid.UUID, role auth.Role) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{UserID: id, Role: role})
}

func day(n int) domain.Date { return domain.NewDate(2026, time.December, 1).AddDays(n) }

func (e *env) book(customer uuid.UUID, in, out int, key string) (domain.Reservation, bool, error) {
	return e.svc.Book(as(customer, auth.RoleCustomer), app.BookInput{
		RoomTypeID: e.room, CheckIn: day(in), CheckOut: day(out), Guests: 2, IdempotencyKey: key,
	})
}

// relayScan читает outbox так, как его видит ретранслятор: под владельцем
// и с объявленным контекстом app.relay.
func (e *env) relayScan(t *testing.T, sql string, dest ...any) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.relay','on',true)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql).Scan(dest...)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) outboxCount(t *testing.T, eventType string) int {
	t.Helper()
	var n int
	e.relayScan(t, `SELECT count(*) FROM outbox WHERE '`+eventType+`' IN ('', envelope->>'event_type')`, &n)
	return n
}

func (e *env) maxBooked(t *testing.T) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(max(booked),0) FROM inventory WHERE room_type_id=$1`, e.room).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Гонка за последний номер: 25 клиентов одновременно бронируют единственный
// номер на одни и те же даты. Ровно один успех, остальные — no_availability.
func TestLastRoomRace(t *testing.T) {
	e := setup(t, 1)
	const n = 25
	var ok, sold atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := e.book(uuid.New(), 5, 8, uuid.NewString())
			switch {
			case err == nil:
				ok.Add(1)
			case apperr.As(err).Code == "no_availability":
				sold.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || sold.Load() != n-1 {
		t.Fatalf("want 1 success and %d sold-out, got %d/%d", n-1, ok.Load(), sold.Load())
	}
	if b := e.maxBooked(t); b != 1 {
		t.Fatalf("inventory booked = %d, want 1", b)
	}
}

// Пересекающиеся многодневные брони вперемешку с отменами: ни в один день
// продано не больше total_rooms, остатки сходятся с подтверждёнными бронями,
// взаимоблокировок нет.
func TestOverlappingStaysKeepInvariant(t *testing.T) {
	const total = 3
	e := setup(t, total)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var booked []struct {
		id   uuid.UUID
		cust uuid.UUID
	}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			in := 1 + rnd.Intn(10)
			out := in + 1 + rnd.Intn(5)
			cust := uuid.New()
			r, _, err := e.book(cust, in, out, uuid.NewString())
			if err != nil {
				if apperr.As(err).Code != "no_availability" {
					t.Errorf("book: %v", err)
				}
				return
			}
			mu.Lock()
			booked = append(booked, struct {
				id   uuid.UUID
				cust uuid.UUID
			}{r.ID, cust})
			mu.Unlock()
			if rnd.Intn(3) == 0 {
				if _, err := e.svc.Cancel(as(cust, auth.RoleCustomer), r.ID, ""); err != nil {
					t.Errorf("cancel: %v", err)
				}
			}
		}(int64(i))
	}
	wg.Wait()
	if b := e.maxBooked(t); b > total {
		t.Fatalf("overbooking: %d rooms sold on some day, total %d", b, total)
	}
	// Сверка: для каждого дня booked = число подтверждённых броней на этот
	// день. Брони скрыты RLS, поэтому читаем от имени system.
	var bad int
	conn, err := e.owner.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, _ = tx.Exec(context.Background(), `SELECT set_config('app.role','system',true)`)
	if err := tx.QueryRow(context.Background(), `
		SELECT count(*) FROM inventory i
		WHERE i.room_type_id = $1 AND i.booked <> (
			SELECT count(*) FROM reservations r
			WHERE r.room_type_id = i.room_type_id AND r.status = 'confirmed'
			  AND r.check_in <= i.day AND r.check_out > i.day)`, e.room).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d inventory days disagree with confirmed reservations", bad)
	}
	if len(booked) == 0 {
		t.Fatal("expected at least some successful bookings")
	}
}

func TestIdempotency(t *testing.T) {
	e := setup(t, 5)
	cust := uuid.New()
	key := "idem-" + uuid.NewString()

	first, replayed, err := e.book(cust, 2, 4, key)
	if err != nil || replayed {
		t.Fatalf("first: replayed=%v err=%v", replayed, err)
	}
	again, replayed, err := e.book(cust, 2, 4, key)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay must return the same reservation: replayed=%v id=%v err=%v", replayed, again.ID, err)
	}
	if _, _, err := e.book(cust, 2, 5, key); apperr.As(err).Code != "idempotency_key_reused" {
		t.Fatalf("same key with another body must fail, got %v", err)
	}
	// Тот же ключ у другого клиента — независимый запрос.
	if _, replayed, err := e.book(uuid.New(), 2, 4, key); err != nil || replayed {
		t.Fatalf("keys are scoped per customer: replayed=%v err=%v", replayed, err)
	}
	if b := e.maxBooked(t); b != 2 {
		t.Fatalf("two real bookings expected, inventory says %d", b)
	}

	// Параллельный повтор (двойной клик + ретрай сети): 10 одинаковых запросов.
	cust2, key2 := uuid.New(), "idem-"+uuid.NewString()
	ids := make(chan uuid.UUID, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := e.book(cust2, 6, 7, key2)
			if err != nil {
				t.Errorf("parallel replay: %v", err)
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	uniq := map[uuid.UUID]bool{}
	for id := range ids {
		uniq[id] = true
	}
	if len(uniq) != 1 {
		t.Fatalf("parallel retries created %d reservations, want 1", len(uniq))
	}
	if _, _, err := e.book(cust, 2, 4, "short"); apperr.As(err).Code != "idempotency_key_required" {
		t.Fatalf("short key must be rejected, got %v", err)
	}
}

func TestRowLevelSecurity(t *testing.T) {
	e := setup(t, 5)
	alice, bob := uuid.New(), uuid.New()
	res, _, err := e.book(alice, 3, 5, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Get(as(bob, auth.RoleCustomer), res.ID); !apperr.Is(err, apperr.KindNotFound) {
		t.Fatalf("bob must not see alice's reservation, got %v", err)
	}
	if _, err := e.svc.Cancel(as(bob, auth.RoleCustomer), res.ID, ""); !apperr.Is(err, apperr.KindNotFound) {
		t.Fatalf("bob must not cancel alice's reservation, got %v", err)
	}
	if _, err := e.svc.Get(as(e.manager, auth.RoleManager), res.ID); err != nil {
		t.Fatalf("hotel manager must see it: %v", err)
	}
	stranger := uuid.New()
	list, total, err := e.svc.HotelReservations(as(stranger, auth.RoleManager), app.ReservationFilter{HotelID: e.hotel, Limit: 10})
	if err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("manager of another hotel must see nothing: total=%d err=%v", total, err)
	}
	list, total, err = e.svc.HotelReservations(as(e.manager, auth.RoleManager), app.ReservationFilter{HotelID: e.hotel, Limit: 10})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("own manager sees 1: total=%d err=%v", total, err)
	}

	// Запрос «в обход» сервиса без контекста безопасности: fail-closed.
	var visible int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM reservations`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("without app.role RLS must hide everything, saw %d", visible)
	}

	// Контекст SET LOCAL не утекает в следующую транзакцию на том же соединении.
	conn, err := e.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.role','admin',true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM reservations`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if visible != 1 {
		t.Fatalf("admin sees 1, got %d", visible)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM reservations`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("security context leaked to the next transaction on a pooled connection: saw %d", visible)
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	e := setup(t, 5)
	if _, _, err := e.book(uuid.New(), 1, 2, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recs, total, err := e.svc.AuditLog(as(e.manager, auth.RoleManager), e.hotel, 10, 0)
	if err != nil || total != 1 || recs[0].Action != "reservation.created" {
		t.Fatalf("manager reads audit: total=%d err=%v", total, err)
	}
	// Слой 1 — права и RLS под ролью данных (под ней работают запросы).
	// С отдельной ролью данных UPDATE/DELETE запрещены правами (42501); без
	// неё (одна роль) — для них нет политики RLS, и изменение затрагивает 0 строк.
	for _, q := range []string{`UPDATE audit_log SET action='x'`, `DELETE FROM audit_log`} {
		var tag pgconn.CommandTag
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.role','admin',true)`); err != nil {
				return err
			}
			var err error
			tag, err = tx.Exec(ctx, q)
			return err
		})
		switch {
		case pgtest.Split() && pgCode(err) != "42501":
			t.Fatalf("%q under the data role must be denied (42501), got %v", q, err)
		case !pgtest.Split() && (err != nil || tag.RowsAffected() != 0):
			t.Fatalf("%q changed %d rows, err=%v", q, tag.RowsAffected(), err)
		}
	}
	// Роль данных не может снять защиту: ни RLS, ни триггер, ни TRUNCATE.
	if pgtest.Split() {
		for _, q := range []string{
			`ALTER TABLE audit_log NO FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update`,
			`DROP TRIGGER audit_log_no_update ON audit_log`,
			`TRUNCATE audit_log`,
		} {
			if _, err := e.pool.Exec(ctx, q); pgCode(err) != "42501" {
				t.Fatalf("data role ran %q: %v", q, err)
			}
		}
	}
	// Слой 2 — триггер под ролью-владельцем: даже если владелец снимет FORCE
	// RLS, изменение и TRUNCATE падают. Владелец может удалить и сам триггер —
	// это граница защиты (T41, T111): под владельцем работают только миграции
	// и служебные процессы сервиса, запросы пользователей — нет.
	for _, q := range []string{`UPDATE audit_log SET action='x'`, `DELETE FROM audit_log`, `TRUNCATE audit_log`} {
		err := pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE audit_log NO FORCE ROW LEVEL SECURITY`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
			return errors.New("rollback")
		})
		if pgCode(err) != "42501" { // триггер поднимает insufficient_privilege
			t.Fatalf("%q must be rejected by the trigger, got %v", q, err)
		}
	}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestCancelReleasesRoom(t *testing.T) {
	e := setup(t, 1)
	alice, bob := uuid.New(), uuid.New()
	r, _, err := e.book(alice, 10, 12, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.book(bob, 11, 13, uuid.NewString()); apperr.As(err).Code != "no_availability" {
		t.Fatalf("overlapping stay must be sold out, got %v", err)
	}
	// Выезд Алисы 12-го и заезд Боба 12-го не конфликтуют: полуинтервал.
	if _, _, err := e.book(bob, 12, 13, uuid.NewString()); err != nil {
		t.Fatalf("back-to-back stays must fit: %v", err)
	}
	if _, err := e.svc.Cancel(as(alice, auth.RoleCustomer), r.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.book(bob, 10, 12, uuid.NewString()); err != nil {
		t.Fatalf("room must be free after cancel: %v", err)
	}
}

func TestAvailabilityHidesSoldOutForCustomers(t *testing.T) {
	e := setup(t, 1)
	if _, _, err := e.book(uuid.New(), 3, 4, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	pub, err := e.svc.Availability(context.Background(), e.hotel, day(3), day(4), 1)
	if err != nil || len(pub) != 0 {
		t.Fatalf("customer must not see a sold-out category: %v %v", pub, err)
	}
	mgr, err := e.svc.Availability(as(e.manager, auth.RoleManager), e.hotel, day(3), day(4), 1)
	if err != nil || len(mgr) != 1 || mgr[0].Free != 0 {
		t.Fatalf("manager sees the category with free=0: %+v %v", mgr, err)
	}
	other, _ := e.svc.Availability(context.Background(), e.hotel, day(4), day(6), 1)
	if len(other) != 1 || other[0].Total.Amount != 2*500000 {
		t.Fatalf("free on other dates, total for 2 nights: %+v", other)
	}
}

// Отключение отеля в каталоге → событие → booking отменяет будущие брони
// (но не текущие) и кладёт ReservationCancelled в outbox.
func TestHotelDeactivationCompensation(t *testing.T) {
	e := setup(t, 5)
	future, _, err := e.book(uuid.New(), 4, 6, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Бронь с заездом сегодня: день заезда считается уже начавшимся проживанием
	// (гость может быть в пути или в номере), такие брони система не отменяет (T24).
	current, _, err := e.book(uuid.New(), 0, 3, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	e.applyHotel(t, "inactive", 2)

	sys := auth.WithPrincipal(ctx, auth.Principal{Role: auth.RoleAdmin, UserID: uuid.New()})
	got, _ := e.svc.Get(sys, future.ID)
	if got.Status != domain.Cancelled {
		t.Fatalf("future reservation must be cancelled, got %s", got.Status)
	}
	got, _ = e.svc.Get(sys, current.ID)
	if got.Status != domain.Confirmed {
		t.Fatalf("ongoing stay must stay confirmed, got %s", got.Status)
	}
	if n := e.outboxCount(t, "ReservationCancelled"); n != 1 {
		t.Fatalf("one ReservationCancelled expected in outbox, got %d", n)
	}
	// След компенсации для администратора: запись аудита с числом отмен.
	recs, _, err := e.svc.AuditLog(sys, e.hotel, 10, 0)
	if err != nil || recs[0].Action != "hotel.deactivation_processed" || recs[0].Details["cancelled_reservations"] != float64(1) {
		t.Fatalf("deactivation must leave an audit trail: %+v %v", recs, err)
	}
	// Запись — от system, а кто отключил отель в каталоге, видно в деталях.
	if recs[0].ActorRole != "system" || recs[0].Details["deactivated_by"] != e.admin.String() {
		t.Fatalf("audit trail must name who deactivated the hotel: %+v", recs[0])
	}
	if _, _, err := e.book(uuid.New(), 8, 9, uuid.NewString()); apperr.As(err).Code != "not_sellable" {
		t.Fatalf("inactive hotel must not sell, got %v", err)
	}

	// Повтор того же снимка и устаревший снимок ничего не меняют.
	e.applyHotel(t, "inactive", 2)
	e.applyHotel(t, "active", 1)
	if _, _, err := e.book(uuid.New(), 8, 9, uuid.NewString()); apperr.As(err).Code != "not_sellable" {
		t.Fatalf("stale snapshot (v1) must be ignored, got %v", err)
	}
}

func TestDuplicateEventIsIgnored(t *testing.T) {
	e := setup(t, 1)
	ev := e.event(t, "RoomTypeUpserted", e.room, 5, app.RoomTypeSnapshot{
		RoomTypeID: e.room, HotelID: e.hotel, Name: "Стандарт", Capacity: 2, TotalRooms: 7,
		Price: domain.Money{Amount: 1, Currency: "RUB"}, Status: "active", Version: 5,
	})
	for i := 0; i < 3; i++ {
		if err := e.svc.HandleCatalogEvent(context.Background(), e.sys, ev); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE event_id=$1`, ev.EventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("event must be recorded once, got %d", n)
	}
	var total int
	if err := e.pool.QueryRow(context.Background(), `SELECT total_rooms FROM room_type_replica WHERE room_type_id=$1`, e.room).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 7 {
		t.Fatalf("replica must have total_rooms=7, got %d", total)
	}
}

func TestOutboxRelay(t *testing.T) {
	e := setup(t, 3)
	for i := 0; i < 3; i++ {
		if _, _, err := e.book(uuid.New(), 1, 2, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	pub := &fakePublisher{}
	relay := &messaging.Relay{Pool: e.owner, Publisher: pub, BatchSize: 2}
	ctx := context.Background()

	pub.fail = true
	if n, err := relay.Once(ctx); err != nil || n != 0 {
		t.Fatalf("failed publish must not mark rows: n=%d err=%v", n, err)
	}
	pub.fail = false
	total := 0
	for {
		n, err := relay.Once(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total != 3 || len(pub.msgs) != 3 {
		t.Fatalf("3 events expected, published %d (%d msgs)", total, len(pub.msgs))
	}
	if pub.msgs[0].Topic != messaging.TopicReservation || pub.msgs[0].Headers["event_type"] != "ReservationConfirmed" {
		t.Fatalf("unexpected message: %+v", pub.msgs[0])
	}
	var attempts int
	e.relayScan(t, `SELECT max(attempts) FROM outbox`, &attempts)
	if attempts != 1 {
		t.Fatalf("failed attempt must be counted, got %d", attempts)
	}
}

type fakePublisher struct {
	mu   sync.Mutex
	fail bool
	msgs []messaging.Message
}

func (f *fakePublisher) Publish(_ context.Context, msgs ...messaging.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("broker down")
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}
