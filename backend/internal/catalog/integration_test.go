package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shinilornekora/tourflow/backend/internal/catalog/app"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/domain"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/catalog/store"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

type fakeDirectory map[uuid.UUID]auth.Role

func (f fakeDirectory) Role(_ context.Context, id uuid.UUID) (auth.Role, error) {
	r, ok := f[id]
	if !ok {
		return "", apperr.NotFound("Пользователь")
	}
	return r, nil
}

func as(id uuid.UUID, role auth.Role) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{UserID: id, Role: role})
}

// setup возвращает сервис под ролью данных и пул владельца (для проверок
// outbox, который роль данных читать не может).
func setup(t *testing.T) (*app.Service, pg.Pools, fakeDirectory) {
	pools := pgtest.FreshPools(t, "catalog_test", migrations.FS, migrations.Deny...)
	dir := fakeDirectory{}
	return &app.Service{Store: store.Store{Pool: pools.App}, Directory: dir}, pools, dir
}

var hotelFields = domain.HotelFields{Name: "Гранд Отель", City: "Казань", Stars: 4, Address: "ул. Баумана, 1"}

func TestHotelLifecycleAndPermissions(t *testing.T) {
	svc, pools, dir := setup(t)
	admin, manager, customer := uuid.New(), uuid.New(), uuid.New()
	dir[manager], dir[customer] = auth.RoleManager, auth.RoleCustomer

	if _, err := svc.CreateHotel(as(manager, auth.RoleManager), hotelFields); !apperr.Is(err, apperr.KindForbidden) {
		t.Fatalf("only admin creates hotels: %v", err)
	}
	h, err := svc.CreateHotel(as(admin, auth.RoleAdmin), hotelFields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AssignManager(as(admin, auth.RoleAdmin), h.ID, customer); apperr.As(err).Code != "not_a_manager" {
		t.Fatalf("customer cannot be assigned: %v", err)
	}
	h, err = svc.AssignManager(as(admin, auth.RoleAdmin), h.ID, manager)
	if err != nil || !h.IsManagedBy(manager) || h.Version != 2 {
		t.Fatalf("assign manager: %+v %v", h, err)
	}

	// Менеджер правит свой отель, но не может его выключить.
	upd := hotelFields
	upd.Description = "Лучший вид на Кремль"
	if h, err = svc.UpdateHotel(as(manager, auth.RoleManager), h.ID, upd, h.Version); err != nil {
		t.Fatalf("manager edits own hotel: %v", err)
	}
	if _, err := svc.SetHotelStatus(as(manager, auth.RoleManager), h.ID, domain.HotelInactive, 0); !apperr.Is(err, apperr.KindForbidden) {
		t.Fatalf("manager cannot deactivate: %v", err)
	}
	// Чужой менеджер — нет.
	if _, err := svc.UpdateHotel(as(uuid.New(), auth.RoleManager), h.ID, upd, 0); !apperr.Is(err, apperr.KindForbidden) {
		t.Fatalf("other manager must be forbidden: %v", err)
	}
	// Устаревшая версия из второй вкладки.
	if _, err := svc.UpdateHotel(as(admin, auth.RoleAdmin), h.ID, upd, 1); apperr.As(err).Code != "version_mismatch" {
		t.Fatalf("stale version must conflict: %v", err)
	}

	rt, err := svc.CreateRoomType(as(manager, auth.RoleManager), h.ID, domain.RoomFields{
		Name: "Стандарт", Capacity: 2, TotalRooms: 10, Price: domain.Money{Amount: 450000}, Amenities: []string{"Wi-Fi", "wi-fi", " Кондиционер "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Amenities) != 2 || rt.Price.Currency != "RUB" {
		t.Fatalf("amenities dedup / currency default: %+v", rt)
	}
	if _, err := svc.CreateRoomType(as(manager, auth.RoleManager), h.ID, domain.RoomFields{
		Name: "Стандарт", Capacity: 2, TotalRooms: 1, Price: domain.Money{Amount: 1},
	}); apperr.As(err).Code != "duplicate_name" {
		t.Fatalf("duplicate name in hotel: %v", err)
	}

	// Неактивный отель не виден анониму, но виден админу.
	if _, err := svc.SetHotelStatus(as(admin, auth.RoleAdmin), h.ID, domain.HotelInactive, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Hotel(context.Background(), h.ID); !apperr.Is(err, apperr.KindNotFound) {
		t.Fatalf("inactive hotel hidden from public: %v", err)
	}
	if _, _, err := svc.Hotel(as(admin, auth.RoleAdmin), h.ID); err != nil {
		t.Fatalf("admin sees inactive hotel: %v", err)
	}

	// Каждое изменение — событие в outbox, с монотонной версией агрегата.
	var versions []int64
	ctx := context.Background()
	err = pgx.BeginFunc(ctx, pools.Owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.relay','on',true)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT aggregate_version FROM outbox WHERE topic=$1 ORDER BY id`, messaging.TopicHotels)
		if err != nil {
			return err
		}
		if versions, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		// Автор изменения едет в событии: у каталога своего журнала нет (T124).
		var byAdmin, anonymous int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE envelope->'payload'->>'changed_by' = $2),
				count(*) FILTER (WHERE envelope->'payload'->>'changed_by' IS NULL)
			FROM outbox WHERE topic=$1`, messaging.TopicHotels, admin.String()).Scan(&byAdmin, &anonymous); err != nil {
			return err
		}
		if byAdmin == 0 || anonymous != 0 {
			t.Errorf("every hotel event must name its author: by admin %d, without author %d", byAdmin, anonymous)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 2, 3, 4}
	if len(versions) != len(want) {
		t.Fatalf("hotel events: %v, want %v", versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("hotel event versions %v, want %v", versions, want)
		}
	}
}

func TestSearchFiltersAndSort(t *testing.T) {
	svc, _, _ := setup(t)
	admin := as(uuid.New(), auth.RoleAdmin)
	mk := func(name, city string, stars int, price int64, capacity int) {
		h, err := svc.CreateHotel(admin, domain.HotelFields{Name: name, City: city, Stars: stars})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CreateRoomType(admin, h.ID, domain.RoomFields{Name: "Номер", Capacity: capacity, TotalRooms: 3, Price: domain.Money{Amount: price}}); err != nil {
			t.Fatal(err)
		}
	}
	mk("Волга", "Казань", 3, 300000, 2)
	mk("Кремлёвский", "Казань", 5, 900000, 4)
	mk("Нева", "Санкт-Петербург", 4, 600000, 2)
	mk("100%_скидка", "Казань", 2, 100000, 1)

	pub := context.Background()
	list, total, err := svc.ListHotels(pub, app.HotelFilter{City: "казань", Sort: "price", Limit: 10})
	if err != nil || total != 3 || list[0].Name != "100%_скидка" {
		t.Fatalf("city filter + price sort: total=%d first=%v err=%v", total, list, err)
	}
	_, total, _ = svc.ListHotels(pub, app.HotelFilter{StarsMin: 4, Limit: 10})
	if total != 2 {
		t.Fatalf("stars_min=4: %d", total)
	}
	_, total, _ = svc.ListHotels(pub, app.HotelFilter{Guests: 3, Limit: 10})
	if total != 1 {
		t.Fatalf("guests=3 only Kremlin: %d", total)
	}
	_, total, _ = svc.ListHotels(pub, app.HotelFilter{PriceMax: 600000, Limit: 10})
	if total != 3 {
		t.Fatalf("price_max: %d", total)
	}
	// % и _ в поиске — литералы, а не шаблоны LIKE.
	_, total, _ = svc.ListHotels(pub, app.HotelFilter{Query: "%_", Limit: 10})
	if total != 1 {
		t.Fatalf("LIKE metacharacters must be escaped: %d", total)
	}
	page2, total, _ := svc.ListHotels(pub, app.HotelFilter{Sort: "-stars", Limit: 2, Offset: 2})
	if total != 4 || len(page2) != 2 || page2[0].Stars != 3 {
		t.Fatalf("pagination: total=%d page=%v", total, page2)
	}
	if _, _, err := svc.ListHotels(pub, app.HotelFilter{Sort: "name; DROP TABLE hotels"}); !apperr.Is(err, apperr.KindValidation) {
		t.Fatalf("sort must be whitelisted: %v", err)
	}
	cities, _ := svc.Cities(pub)
	if len(cities) != 2 {
		t.Fatalf("cities: %v", cities)
	}
}

// РХД ПР-2, отрицательные тесты каталога: второй уровень принуждения — RLS
// под ролью данных. Запросы идут «в обход» сервиса, как сделал бы код,
// забывший проверку домена.
func TestCatalogRLSNegative(t *testing.T) {
	svc, pools, dir := setup(t)
	admin, mgrA, mgrB := uuid.New(), uuid.New(), uuid.New()
	dir[mgrA], dir[mgrB] = auth.RoleManager, auth.RoleManager
	ctx := context.Background()
	mk := func(name string, mgr uuid.UUID) domain.Hotel {
		f := hotelFields
		f.Name = name
		h, err := svc.CreateHotel(as(admin, auth.RoleAdmin), f)
		if err != nil {
			t.Fatal(err)
		}
		h, err = svc.AssignManager(as(admin, auth.RoleAdmin), h.ID, mgr)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b := mk("Отель А", mgrA), mk("Отель Б", mgrB)

	run := func(user uuid.UUID, role, sql string, args ...any) (int64, error) {
		var n int64
		err := pgx.BeginFunc(ctx, pools.App, func(tx pgx.Tx) error {
			if err := pg.SetActor(ctx, tx, user.String(), role); err != nil {
				return err
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
	// Положительный контроль: менеджер А меняет свой отель — иначе «ноль
	// строк» у чужого ничего бы не доказывал.
	if n, err := run(mgrA, "hotel_manager", `UPDATE hotels SET description='своё' WHERE id=$1`, a.ID); err != nil || n != 1 {
		t.Fatalf("manager A must edit own hotel: %d %v", n, err)
	}
	// 1. Чужой отель менеджер не меняет: строка не проходит USING.
	if n, err := run(mgrA, "hotel_manager", `UPDATE hotels SET description='взлом' WHERE id=$1`, b.ID); err != nil || n != 0 {
		t.Fatalf("manager A edited hotel B: %d %v", n, err)
	}
	// 2. Категорию в чужой отель не создать: WITH CHECK.
	if _, err := run(mgrA, "hotel_manager", `INSERT INTO room_types(id, hotel_id, name, capacity, total_rooms, price_amount)
		VALUES (gen_random_uuid(), $1, 'Чужая', 2, 1, 100)`, b.ID); code(err) != "42501" {
		t.Fatalf("manager A created a room type in hotel B: %v", err)
	}
	// 3. Назначить себя менеджером чужого отеля нельзя.
	if _, err := run(mgrA, "hotel_manager", `INSERT INTO hotel_managers(hotel_id, user_id) VALUES ($1, $2)`, b.ID, mgrA); code(err) != "42501" {
		t.Fatalf("manager A assigned himself to hotel B: %v", err)
	}
	// 3а. Чужая роль в своём отеле: назначать менеджеров и менять статус
	// может только администратор. Менеджер не добавит со-менеджера и не
	// отключит свой отель прямым SQL — а hotel_managers через события
	// становится источником прав RLS в booking.
	if _, err := run(mgrA, "hotel_manager", `INSERT INTO hotel_managers(hotel_id, user_id) VALUES ($1, $2)`, a.ID, uuid.New()); code(err) != "42501" {
		t.Fatalf("manager A added a co-manager to his own hotel: %v", err)
	}
	if n, err := run(mgrA, "hotel_manager", `DELETE FROM hotel_managers WHERE hotel_id=$1`, a.ID); err != nil || n != 0 {
		t.Fatalf("manager A removed managers of his own hotel: %d %v", n, err)
	}
	if _, err := run(mgrA, "hotel_manager", `UPDATE hotels SET status='inactive' WHERE id=$1`, a.ID); code(err) != "42501" {
		t.Fatalf("manager A deactivated his own hotel: %v", err)
	}
	// Положительный контроль: администратору обе операции доступны.
	if n, err := run(admin, "admin", `UPDATE hotels SET status='inactive' WHERE id=$1`, b.ID); err != nil || n != 1 {
		t.Fatalf("admin must be able to change status: %d %v", n, err)
	}
	if _, err := run(admin, "admin", `INSERT INTO hotel_managers(hotel_id, user_id) VALUES ($1, $2)`, b.ID, uuid.New()); err != nil {
		t.Fatalf("admin must be able to assign a manager: %v", err)
	}
	// 4. Клиент и запрос без контекста не создают отели.
	for _, role := range []string{"customer", ""} {
		if _, err := run(uuid.New(), role, `INSERT INTO hotels(id, name, city, stars) VALUES (gen_random_uuid(), 'Левый', 'Город', 3)`); code(err) != "42501" {
			t.Fatalf("role %q created a hotel: %v", role, err)
		}
	}
	// 5. Роль данных не снимает защиту и не читает очередь событий.
	if pgtest.Split() {
		for _, q := range []string{
			`ALTER TABLE hotels DISABLE ROW LEVEL SECURITY`,
			`DROP POLICY hotels_edit ON hotels`,
			`SELECT count(*) FROM outbox`,
			`CREATE TEMP TABLE hotel_managers (hotel_id uuid, user_id uuid)`,
			`DELETE FROM outbox`,
		} {
			if _, err := pools.App.Exec(ctx, q); code(err) != "42501" {
				t.Fatalf("data role ran %q: %v", q, err)
			}
		}
	}
}
