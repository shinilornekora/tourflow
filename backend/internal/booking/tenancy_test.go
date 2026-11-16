package booking_test

// РХД ПР-2: два уровня принуждения и отрицательные тесты.
//
//	Уровень 1 — приложение: актор берётся только из проверенного токена,
//	            правила домена (CanView, Cancel).
//	Уровень 2 — PostgreSQL: RLS с FORCE под ролью данных, которая таблицами
//	            не владеет и снять политику не может.
//
// Здесь проверяется уровень 2: запросы идут прямым SQL «в обход» сервиса,
// как сделал бы код, забывший условие WHERE или проверку домена.
// Модель размещения — docs/TENANCY.md.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

// raw выполняет SQL под ролью данных с заданным контекстом RLS.
func (e *env) raw(t *testing.T, user uuid.UUID, role auth.Role, sql string, args ...any) (int64, error) {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if role != "" {
			if err := pg.SetActor(ctx, tx, user.String(), string(role)); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, sql, args...)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func TestNegativeTenancy(t *testing.T) {
	e := setup(t, 5)
	alice, bob, stranger := uuid.New(), uuid.New(), uuid.New()
	res, _, err := e.book(alice, 3, 5, "key-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	// Положительный контроль: свои строки видны — иначе «ноль у чужого»
	// ничего бы не доказывал.
	for _, q := range []string{`SELECT * FROM reservations`, `SELECT * FROM idempotency_keys`} {
		if n, err := e.raw(t, alice, auth.RoleCustomer, q); err != nil || n != 1 {
			t.Fatalf("alice must see her own row (%s): %d %v", q, n, err)
		}
	}
	if n, err := e.raw(t, e.manager, auth.RoleManager, `SELECT * FROM audit_log`); err != nil || n != 1 {
		t.Fatalf("hotel manager must see the audit record: %d %v", n, err)
	}

	t.Run("1 чужое не читается", func(t *testing.T) {
		for _, q := range []string{`SELECT * FROM reservations`, `SELECT * FROM idempotency_keys`, `SELECT * FROM audit_log`} {
			if n, err := e.raw(t, bob, auth.RoleCustomer, q); err != nil || n != 0 {
				t.Errorf("bob: %s → %d rows, %v", q, n, err)
			}
			if n, err := e.raw(t, stranger, auth.RoleManager, q); err != nil || n != 0 {
				t.Errorf("manager of another hotel: %s → %d rows, %v", q, n, err)
			}
		}
	})

	t.Run("2 чужое не меняется", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE reservations SET guests = 1`,
			`UPDATE idempotency_keys SET request_hash = '\x00'`,
		} {
			if n, err := e.raw(t, bob, auth.RoleCustomer, q); err != nil || n != 0 {
				t.Errorf("bob: %s → %d rows, %v", q, n, err)
			}
		}
		// Удаление: с отдельной ролью данных его нет по правам (42501),
		// с одной ролью — политика RLS оставляет ноль затронутых строк.
		for _, q := range []string{`DELETE FROM reservations`, `DELETE FROM idempotency_keys`} {
			n, err := e.raw(t, bob, auth.RoleCustomer, q)
			switch {
			case pgtest.Split() && pgCode(err) != "42501":
				t.Errorf("bob: %s must be denied, got %d rows, %v", q, n, err)
			case !pgtest.Split() && (err != nil || n != 0):
				t.Errorf("bob: %s → %d rows, %v", q, n, err)
			}
		}
	})

	t.Run("3 строку за другого не записать (WITH CHECK)", func(t *testing.T) {
		_, err := e.raw(t, bob, auth.RoleCustomer, `INSERT INTO reservations(id, hotel_id, room_type_id, customer_id, check_in, check_out,
			guests, status, total_amount, currency, hotel_name, room_type_name)
			VALUES (gen_random_uuid(), $1, $2, $3, '2026-12-10', '2026-12-11', 1, 'confirmed', 100, 'RUB', 'x', 'y')`, e.hotel, e.room, alice)
		if pgCode(err) != "42501" {
			t.Errorf("bob inserted a reservation for alice: %v", err)
		}
		_, err = e.raw(t, bob, auth.RoleCustomer, `INSERT INTO idempotency_keys(customer_id, idem_key, request_hash) VALUES ($1, 'stolen-key-1', '\x00')`, alice)
		if pgCode(err) != "42501" {
			t.Errorf("bob claimed a key for alice: %v", err)
		}
	})

	t.Run("4 аудит не подделать", func(t *testing.T) {
		// От чужого имени и от имени system клиент записать не может.
		for _, c := range []struct {
			actor any
			role  string
		}{{alice, "customer"}, {nil, "system"}, {bob, "admin"}} {
			_, err := e.raw(t, bob, auth.RoleCustomer, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id)
				VALUES ($1, $2, $3, 'reservation.cancelled', $4)`, e.hotel, c.actor, c.role, res.ID)
			if pgCode(err) != "42501" {
				t.Errorf("bob forged an audit record as %v/%s: %v", c.actor, c.role, err)
			}
		}
	})

	t.Run("4а аудит чужого отеля не дописать", func(t *testing.T) {
		// Автор настоящий, но к отелю отношения не имеет: клиент без брони и
		// менеджер другого отеля. Иначе подделывался бы след саги (T121).
		for _, c := range []struct {
			who    uuid.UUID
			role   auth.Role
			action string
			entity uuid.UUID
		}{
			{bob, auth.RoleCustomer, "reservation.cancelled", res.ID},
			{bob, auth.RoleCustomer, "hotel.deactivation_processed", e.hotel},
			{stranger, auth.RoleManager, "hotel.deactivation_processed", e.hotel},
			{alice, auth.RoleCustomer, "hotel.deactivation_processed", res.ID},
			// Даже менеджер этого отеля: записи hotel.* пишет только system.
			{e.manager, auth.RoleManager, "hotel.deactivation_processed", e.hotel},
		} {
			_, err := e.raw(t, c.who, c.role, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id)
				VALUES ($1, $2, $3, $4, $5)`, e.hotel, c.who, string(c.role), c.action, c.entity)
			if pgCode(err) != "42501" {
				t.Errorf("%s wrote %s into a foreign hotel's audit: %v", c.role, c.action, err)
			}
		}
		// Положительный контроль: менеджер этого отеля и автор брони писать могут.
		if _, err := e.raw(t, e.manager, auth.RoleManager, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id)
			VALUES ($1, $2, 'hotel_manager', 'reservation.note', $3)`, e.hotel, e.manager, res.ID); err != nil {
			t.Errorf("hotel manager must be able to write audit: %v", err)
		}
		if _, err := e.raw(t, alice, auth.RoleCustomer, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id)
			VALUES ($1, $2, 'customer', 'reservation.note', $3)`, e.hotel, alice, res.ID); err != nil {
			t.Errorf("reservation owner must be able to write audit: %v", err)
		}
	})

	t.Run("4б служебный контекст — только владельцу", func(t *testing.T) {
		if !pgtest.Split() {
			t.Skip("TEST_PG_APP_DSN не задан: роль данных совпадает с владельцем")
		}
		// Роль данных объявляет себя system: политика читает это как пустую роль.
		_, err := e.raw(t, uuid.Nil, auth.RoleSystem, `INSERT INTO audit_log(hotel_id, actor_id, actor_role, action, entity_id)
			VALUES ($1, NULL, 'system', 'hotel.deactivation_processed', $1)`, e.hotel)
		if pgCode(err) != "42501" {
			t.Errorf("data role forged a system audit record: %v", err)
		}
		for _, q := range []string{`SELECT * FROM reservations`, `SELECT * FROM idempotency_keys`, `SELECT * FROM audit_log`} {
			if n, err := e.raw(t, uuid.Nil, auth.RoleSystem, q); err != nil || n != 0 {
				t.Errorf("data role as self-declared system: %s → %d rows, %v", q, n, err)
			}
		}
	})

	t.Run("5 без контекста — ноль строк (fail closed)", func(t *testing.T) {
		for _, q := range []string{`SELECT * FROM reservations`, `SELECT * FROM idempotency_keys`, `SELECT * FROM audit_log`} {
			if n, err := e.raw(t, uuid.Nil, "", q); err != nil || n != 0 {
				t.Errorf("no context: %s → %d rows, %v", q, n, err)
			}
		}
	})

	t.Run("6 роль данных не снимает защиту", func(t *testing.T) {
		if !pgtest.Split() {
			t.Skip("TEST_PG_APP_DSN не задан: роль данных совпадает с владельцем")
		}
		ctx := context.Background()
		for _, q := range []string{
			`ALTER TABLE reservations DISABLE ROW LEVEL SECURITY`,
			`ALTER TABLE reservations NO FORCE ROW LEVEL SECURITY`,
			`DROP POLICY reservations_access ON reservations`,
			`CREATE POLICY open_all ON reservations USING (true)`,
			`ALTER TABLE reservations OWNER TO CURRENT_USER`,
			`SELECT count(*) FROM outbox`,
			`UPDATE outbox SET published_at = now()`,
			`DELETE FROM outbox`,
			`INSERT INTO reconciliation_issues(room_type_id, day, booked, confirmed) VALUES (gen_random_uuid(), now(), 0, 0)`,
			// источник прав RLS и inbox пишет только проекция под владельцем
			`INSERT INTO hotel_manager_replica(hotel_id, user_id) VALUES (gen_random_uuid(), gen_random_uuid())`,
			`DELETE FROM hotel_manager_replica`,
			`INSERT INTO hotel_replica(hotel_id, name, city, status, version) VALUES (gen_random_uuid(), 'x', 'y', 'active', 1)`,
			`DELETE FROM room_type_replica`,
			// цена, вместимость и статус в реплике: у роли данных UPDATE только на row_lock
			`UPDATE room_type_replica SET price_amount = 1, total_rooms = 100000`,
			`UPDATE hotel_replica SET status = 'inactive'`,
			`UPDATE hotel_replica SET version = 1000000`,
			`SELECT count(*) FROM processed_events`,
			`DELETE FROM reservations`,
			`DELETE FROM inventory`,
			`DELETE FROM idempotency_keys`,
			// подмена источника прав временной таблицей: права TEMP у роли нет
			`CREATE TEMP TABLE hotel_manager_replica (hotel_id uuid, user_id uuid)`,
		} {
			if _, err := e.pool.Exec(ctx, q); pgCode(err) != "42501" {
				t.Errorf("data role ran %q: %v", q, err)
			}
		}
		var bypass bool
		if err := e.pool.QueryRow(ctx, `SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil || bypass {
			t.Fatalf("data role must not bypass RLS: %v %v", bypass, err)
		}
	})
}

// Уровень приложения работает и без RLS: менеджер чужого отеля получает
// пустой список броней и аудита, даже когда политики в базе выключены.
// Так проверяется, что уровни действительно два, а не один RLS.
func TestManagerScopeWithoutRLS(t *testing.T) {
	e := setup(t, 5)
	ctx := context.Background()
	if _, _, err := e.book(uuid.New(), 3, 5, "key-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"reservations", "audit_log"} {
		if _, err := e.owner.Exec(ctx, `ALTER TABLE `+tbl+` DISABLE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
	}
	// Контроль: RLS действительно выключена — прямой запрос чужого менеджера видит строку.
	stranger := uuid.New()
	if n, err := e.raw(t, stranger, auth.RoleManager, `SELECT * FROM reservations`); err != nil || n != 1 {
		t.Fatalf("RLS must be off for this test: %d %v", n, err)
	}

	for _, f := range []app.ReservationFilter{{Limit: 10}, {HotelID: e.hotel, Limit: 10}} {
		list, total, err := e.svc.HotelReservations(as(stranger, auth.RoleManager), f)
		if err != nil || total != 0 || len(list) != 0 {
			t.Errorf("foreign manager got %d reservations (filter %+v): %v", total, f, err)
		}
		list, total, err = e.svc.HotelReservations(as(e.manager, auth.RoleManager), f)
		if err != nil || total != 1 || len(list) != 1 {
			t.Errorf("own manager must see the reservation (filter %+v): %d %v", f, total, err)
		}
	}
	if recs, total, err := e.svc.AuditLog(as(stranger, auth.RoleManager), e.hotel, 10, 0); err != nil || total != 0 || len(recs) != 0 {
		t.Errorf("foreign manager read %d audit records: %v", total, err)
	}
	if _, total, err := e.svc.AuditLog(as(e.manager, auth.RoleManager), e.hotel, 10, 0); err != nil || total != 1 {
		t.Errorf("own manager must read audit: %d %v", total, err)
	}
}
