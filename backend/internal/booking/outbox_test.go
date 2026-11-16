package booking_test

// РХД ПР-4: бизнес-объект (бронь + остатки), ответ команды (под ключом
// идемпотентности) и событие в outbox фиксируются одной транзакцией.
// Сценарии: успешная фиксация, повтор запроса, конфликт изменённого
// содержимого под прежним ключом, откат после записи события, изоляция
// очереди правами и RLS.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shinilornekora/tourflow/backend/internal/booking/app"
	"github.com/shinilornekora/tourflow/backend/internal/booking/domain"
	"github.com/shinilornekora/tourflow/backend/internal/booking/migrations"
	"github.com/shinilornekora/tourflow/backend/internal/platform/apperr"
	"github.com/shinilornekora/tourflow/backend/internal/platform/auth"
	"github.com/shinilornekora/tourflow/backend/internal/platform/messaging"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pg"
	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

func TestOutboxCommittedWithReservation(t *testing.T) {
	e := setup(t, 5)
	res, _, err := e.book(uuid.New(), 2, 4, "key-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var eventID, hotel, msgKey uuid.UUID
	var version int64
	var typ, topic string
	e.relayScan(t, `SELECT event_id, hotel_id, msg_key::uuid, aggregate_version, envelope->>'event_type', topic FROM outbox`,
		&eventID, &hotel, &msgKey, &version, &typ, &topic)
	if typ != "ReservationConfirmed" || topic != messaging.TopicReservation || version != 1 ||
		hotel != res.HotelID || msgKey != res.ID || eventID == uuid.Nil {
		t.Fatalf("outbox row: type=%s topic=%s v=%d hotel=%s key=%s", typ, topic, version, hotel, msgKey)
	}
	// Колонки совпадают с конвертом: потребитель дедуплицирует по event_id.
	var same bool
	e.relayScan(t, `SELECT event_id::text = envelope->>'event_id' AND aggregate_version::text = envelope->>'aggregate_version' FROM outbox`, &same)
	if !same {
		t.Fatal("outbox columns diverge from the envelope")
	}
}

// Повтор с тем же ключом: тот же ответ, ни новой брони, ни нового события.
// Ответ сохранён: после отмены брони повтор всё равно отдаёт то, что
// получил первый запрос, а не текущее состояние.
func TestOutboxReplayReturnsStoredResponse(t *testing.T) {
	e := setup(t, 5)
	cust, key := uuid.New(), "key-"+uuid.NewString()
	first, _, err := e.book(cust, 2, 4, key)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := e.book(cust, 2, 4, key)
	if err != nil || !replayed || again != first {
		t.Fatalf("replay: replayed=%v err=%v\nfirst=%+v\nagain=%+v", replayed, err, first, again)
	}
	if n := e.outboxCount(t, ""); n != 1 {
		t.Fatalf("replay must not add events, outbox has %d", n)
	}
	if _, err := e.svc.Cancel(as(cust, auth.RoleCustomer), first.ID, ""); err != nil {
		t.Fatal(err)
	}
	after, replayed, err := e.book(cust, 2, 4, key)
	if err != nil || !replayed || after.Status != domain.Confirmed || after.Version != 1 {
		t.Fatalf("replay after cancel must return the stored response (confirmed, v1): %+v replayed=%v err=%v", after, replayed, err)
	}
	if n := e.outboxCount(t, ""); n != 2 { // confirmed + cancelled, повтор ничего не добавил
		t.Fatalf("outbox has %d events, want 2", n)
	}
	if b := e.maxBooked(t); b != 0 {
		t.Fatalf("replay re-booked the room: booked=%d", b)
	}
}

// Тот же ключ, другое содержимое — ошибка клиента, и никаких следов.
func TestOutboxConflictUnderSameKey(t *testing.T) {
	e := setup(t, 5)
	cust, key := uuid.New(), "key-"+uuid.NewString()
	if _, _, err := e.book(cust, 2, 4, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.book(cust, 2, 5, key); apperr.As(err).Code != "idempotency_key_reused" {
		t.Fatalf("changed body under the same key: %v", err)
	}
	var reservations int
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM inventory WHERE booked > 0`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if n := e.outboxCount(t, ""); n != 1 || reservations != 2 { // 2 ночи первой брони
		t.Fatalf("conflict left traces: outbox=%d booked days=%d", n, reservations)
	}
}

// failAfter — хранилище, у которого транзакция падает ПОСЛЕ того, как
// сценарий записал бронь, ключ, аудит и событие.
type failAfter struct{ app.Store }

var errBoom = errors.New("boom after outbox write")

func (f failAfter) InTx(ctx context.Context, actor auth.Principal, fn func(app.Tx) error) error {
	return f.Store.InTx(ctx, actor, func(tx app.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return errBoom
	})
}

func TestOutboxRolledBackWithReservation(t *testing.T) {
	e := setup(t, 5)
	e.svc.Store = failAfter{e.store}
	cust, key := uuid.New(), "key-"+uuid.NewString()
	if _, _, err := e.book(cust, 2, 4, key); !errors.Is(err, errBoom) {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	ctx := context.Background()
	var keys, audit, booked int
	err := pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role','system',true)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM idempotency_keys), (SELECT count(*) FROM audit_log),
			(SELECT coalesce(sum(booked),0) FROM inventory)`).Scan(&keys, &audit, &booked)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.outboxCount(t, ""); n != 0 || keys != 0 || audit != 0 || booked != 0 {
		t.Fatalf("rollback left traces: outbox=%d keys=%d audit=%d booked=%d", n, keys, audit, booked)
	}
	// Ключ свободен: тот же запрос теперь проходит.
	e.svc.Store = e.store
	if _, replayed, err := e.book(cust, 2, 4, key); err != nil || replayed {
		t.Fatalf("after rollback the key must be free: replayed=%v err=%v", replayed, err)
	}
}

// Два слоя изоляции очереди. Права: роль данных только добавляет события.
// RLS: даже владелец без объявления ретранслятора очередь не видит.
func TestOutboxIsolated(t *testing.T) {
	e := setup(t, 5)
	if _, _, err := e.book(uuid.New(), 2, 4, "key-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if pgtest.Split() {
		for _, q := range []string{`SELECT count(*) FROM outbox`, `UPDATE outbox SET published_at = now()`, `DELETE FROM outbox`} {
			err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.relay','on',true)`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, q)
				return err
			})
			if pgCode(err) != "42501" {
				t.Fatalf("data role ran %q even as a self-declared relay: %v", q, err)
			}
		}
	}
	var seen int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&seen); err != nil || seen != 0 {
		t.Fatalf("owner without relay context sees %d rows, %v", seen, err)
	}
	for _, q := range []string{`UPDATE outbox SET published_at = now()`, `DELETE FROM outbox`} {
		if tag, err := e.owner.Exec(ctx, q); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("%q without relay context touched %d rows, %v", q, tag.RowsAffected(), err)
		}
	}
	if n := e.outboxCount(t, ""); n != 1 {
		t.Fatalf("outbox changed: %d", n)
	}
}

// Аренда: пачку, взятую упавшим ретранслятором, после истечения аренды
// забирает другой; живая аренда чужому не отдаётся.
func TestRelayLease(t *testing.T) {
	e := setup(t, 5)
	for i := 0; i < 2; i++ {
		if _, _, err := e.book(uuid.New(), 2, 4, "key-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	// «Упавший» ретранслятор: взял аренду и исчез.
	err := pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.relay','on',true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE outbox SET lease_owner = 'dead', lease_until = now() + interval '1 hour'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{}
	live := &messaging.Relay{Pool: e.owner, Publisher: pub}
	if n, err := live.Once(ctx); err != nil || n != 0 {
		t.Fatalf("a live lease must not be stolen: n=%d err=%v", n, err)
	}
	err = pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.relay','on',true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE outbox SET lease_until = now() - interval '1 second'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := live.Once(ctx); err != nil || n != 2 || len(pub.msgs) != 2 {
		t.Fatalf("expired lease must be taken over: n=%d msgs=%d err=%v", n, len(pub.msgs), err)
	}
	if n, err := live.Once(ctx); err != nil || n != 0 {
		t.Fatalf("published rows must not be sent again: n=%d err=%v", n, err)
	}
}

// Ключи, созданные до миграции 002 (без сохранённого ответа), миграция
// дозаполняет из брони. Формат дозаполнения обязан читаться кодом: иначе
// повтор старого запроса падал бы вместо ответа.
func TestLegacyKeyBackfill(t *testing.T) {
	e := setup(t, 5)
	cust, key := uuid.New(), "key-"+uuid.NewString()
	first, _, err := e.book(cust, 2, 4, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := migrations.FS.ReadFile("002_hardening.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	from, to := strings.Index(sql, "UPDATE idempotency_keys k SET response"), strings.Index(sql, "k.response IS NULL;")
	if from < 0 || to < from {
		t.Fatal("backfill statement not found in the migration")
	}
	backfill := sql[from : to+len("k.response IS NULL;")]
	ctx := context.Background()
	err = pgx.BeginFunc(ctx, e.owner, func(tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE idempotency_keys NO FORCE ROW LEVEL SECURITY`, // как в миграции: до включения RLS
			`UPDATE idempotency_keys SET response = NULL`,
			`SELECT set_config('app.role', 'system', true)`,
			backfill,
			`ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := e.book(cust, 2, 4, key)
	if err != nil || !replayed {
		t.Fatalf("replay of a legacy key: replayed=%v err=%v", replayed, err)
	}
	if again.ID != first.ID || again.Stay != first.Stay || again.Total != first.Total || again.Status != first.Status ||
		again.Version != 1 || !again.CreatedAt.Equal(first.CreatedAt) || again.HotelName != first.HotelName {
		t.Fatalf("backfilled response differs:\nfirst=%+v\nagain=%+v", first, again)
	}
}

// Повтор запроса после полуночи: бронь с заездом «сегодня» повторяют на
// следующий день, пока ключ ещё жив. Ответ — сохранённый, а не 422
// «дата заезда прошла»: ключ проверяется раньше дат.
func TestReplayAfterMidnight(t *testing.T) {
	e := setup(t, 5)
	alice, key := uuid.New(), "key-"+uuid.NewString()
	first, replayed, err := e.book(alice, 0, 2, key)
	if err != nil || replayed {
		t.Fatalf("first booking: replayed=%v err=%v", replayed, err)
	}
	e.svc.Now = func() time.Time { return fixedNow.Add(24 * time.Hour) }
	again, replayed, err := e.book(alice, 0, 2, key)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay next day must return the stored response: id=%s replayed=%v err=%v", again.ID, replayed, err)
	}
	// Новый запрос с прошедшей датой по-прежнему отклоняется, и ключ за ним не остаётся.
	key2 := "key-" + uuid.NewString()
	if _, _, err := e.book(alice, 0, 2, key2); err == nil {
		t.Fatal("a new booking for a past date must be rejected")
	}
	var n int
	// Служебный контекст действует только под владельцем таблиц.
	err = pgx.BeginFunc(context.Background(), e.owner, func(tx pgx.Tx) error {
		if err := pg.SetActor(context.Background(), tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE idem_key = $1`, key2).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected request left an idempotency key: %d", n)
	}
}

// ---- РХД ПР-4, вариант Б: изоляция и отказ команды ----

// Б-1. Положительная проверка принадлежности: событие читается так, как его
// читает ретранслятор, и арендатор (отель) и агрегат (бронь) в сохранённом
// конверте равны значениям из запроса. Факт «строка есть» этого не доказывает.
func TestOutboxEnvelopeBelongsToTenant(t *testing.T) {
	e := setup(t, 5)
	alice := uuid.New()
	res, _, err := e.book(alice, 2, 4, "key-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var hotel, aggregate, eventID uuid.UUID
	var payloadHotel, payloadCustomer, eventType string
	var version int64
	e.relayScan(t, `SELECT hotel_id, (envelope->>'aggregate_id')::uuid, event_id, aggregate_version,
		envelope->>'event_type', envelope->'payload'->>'hotel_id', envelope->'payload'->>'customer_id' FROM outbox`,
		&hotel, &aggregate, &eventID, &version, &eventType, &payloadHotel, &payloadCustomer)
	t.Logf("запрос:  отель %s, бронь %s (v%d), клиент %s", e.hotel, res.ID, res.Version, alice)
	t.Logf("конверт: отель %s, агрегат %s (v%d), клиент %s, тип %s", hotel, aggregate, version, payloadCustomer, eventType)
	if hotel != e.hotel || hotel != res.HotelID {
		t.Errorf("tenant of the event: %s, want hotel %s", hotel, e.hotel)
	}
	if aggregate != res.ID || version != res.Version {
		t.Errorf("aggregate of the event: %s v%d, want reservation %s v%d", aggregate, version, res.ID, res.Version)
	}
	if payloadHotel != e.hotel.String() || payloadCustomer != alice.String() {
		t.Errorf("payload: hotel=%s customer=%s", payloadHotel, payloadCustomer)
	}
	if eventType != domain.EventReservationConfirmed || eventID == uuid.Nil {
		t.Errorf("event: type=%s id=%s", eventType, eventID)
	}
}

// Б-2. Из контекста другого арендатора событие о чужой брони сохранить
// нельзя. Конверт уникальный (новый event_id), чтобы отказ политики (42501)
// нельзя было спутать с нарушением уникальности (23505). Затем под
// ретранслятором проверяется, что строки с этим event_id нет.
func TestOutboxForeignEventRejected(t *testing.T) {
	e := setup(t, 5)
	alice, bob, stranger := uuid.New(), uuid.New(), uuid.New()
	res, _, err := e.book(alice, 2, 4, "key-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	// forgeWith пишет событие прямым SQL; aggregate и customer — что стоит в конверте.
	forgeWith := func(who uuid.UUID, role auth.Role, aggregate, customer uuid.UUID) (uuid.UUID, error) {
		id := uuid.New()
		env := `{"event_id":"` + id.String() + `","event_type":"ReservationCancelled","schema_version":1,` +
			`"occurred_at":"2026-12-01T09:00:00Z","aggregate_id":"` + aggregate.String() + `","aggregate_version":2,` +
			`"producer":"booking","payload":{"reservation_id":"` + aggregate.String() + `","hotel_id":"` + e.hotel.String() +
			`","customer_id":"` + customer.String() + `"}}`
		_, err := e.raw(t, who, role, `INSERT INTO outbox(topic, msg_key, envelope, event_id, aggregate_version, hotel_id)
			VALUES ('booking.reservation.v1', $1, $2::jsonb, $3, 2, $4)`, aggregate.String(), env, id, e.hotel)
		return id, err
	}
	forge := func(who uuid.UUID, role auth.Role) (uuid.UUID, error) { return forgeWith(who, role, res.ID, alice) }
	absent := func(id uuid.UUID) {
		t.Helper()
		var n int
		e.relayScan(t, `SELECT count(*) FROM outbox WHERE event_id = '`+id.String()+`'`, &n)
		if n != 0 {
			t.Errorf("forged event %s is in the outbox", id)
		}
	}
	for _, c := range []struct {
		who  uuid.UUID
		role auth.Role
		name string
	}{{bob, auth.RoleCustomer, "another customer"}, {stranger, auth.RoleManager, "manager of another hotel"}, {uuid.Nil, "", "no context"}} {
		id, err := forge(c.who, c.role)
		t.Logf("%-26s → SQLSTATE %s, событие %s в outbox отсутствует", c.name, pgCode(err), id)
		if pgCode(err) != "42501" {
			t.Errorf("%s saved an event about a foreign reservation: %v", c.name, err)
		}
		absent(id)
	}
	// Честный контекст, но неправда в конверте: менеджер этого отеля пишет
	// событие о несуществующей брони и событие с чужим клиентом в payload;
	// клиент пишет событие о своей брони, но адресует его другому человеку
	// (notification отправил бы уведомление по payload.customer_id).
	for _, c := range []struct {
		who       uuid.UUID
		role      auth.Role
		aggregate uuid.UUID
		customer  uuid.UUID
		name      string
	}{
		{e.manager, auth.RoleManager, uuid.New(), alice, "manager, unknown reservation"},
		{e.manager, auth.RoleManager, res.ID, bob, "manager, foreign customer in payload"},
		{alice, auth.RoleCustomer, res.ID, bob, "owner, foreign customer in payload"},
	} {
		id, err := forgeWith(c.who, c.role, c.aggregate, c.customer)
		t.Logf("%-38s → SQLSTATE %s", c.name, pgCode(err))
		if pgCode(err) != "42501" {
			t.Errorf("%s: event that contradicts the reservation was saved: %v", c.name, err)
		}
		absent(id)
	}
	// Положительный контроль: владелец брони и менеджер её отеля событие сохранить могут.
	for _, c := range []struct {
		who  uuid.UUID
		role auth.Role
	}{{alice, auth.RoleCustomer}, {e.manager, auth.RoleManager}} {
		if _, err := forge(c.who, c.role); err != nil {
			t.Errorf("%s must be able to append an event about own reservation/hotel: %v", c.role, err)
		}
	}
	if n := e.outboxCount(t, ""); n != 3 { // событие брони + два контрольных
		t.Errorf("outbox rows: %d, want 3", n)
	}
}

// Б-3. Отклонённая команда не оставляет ничего. В категории один номер:
// первая бронь проходит, вторая (новый ключ, другой клиент) получает
// бизнес-отказ. Для второй команды — 0 броней, 0 ключей, 0 событий: событие
// о факте, которого не было, утверждало бы неправду всем читателям.
func TestOutboxRejectedCommandLeavesNothing(t *testing.T) {
	e := setup(t, 1)
	first, second := uuid.New(), uuid.New()
	if _, _, err := e.book(first, 2, 4, "key-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	before := e.outboxCount(t, "")
	key2 := "key-" + uuid.NewString()
	if _, _, err := e.book(second, 2, 4, key2); apperr.As(err).Code != "no_availability" {
		t.Fatalf("second command must be rejected by the business rule, got %v", err)
	}
	var reservations, keys, audit int
	err := pgx.BeginFunc(context.Background(), e.owner, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := pg.SetActor(ctx, tx, uuid.Nil.String(), string(auth.RoleSystem)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM reservations WHERE customer_id = $1),
			(SELECT count(*) FROM idempotency_keys WHERE customer_id = $1),
			(SELECT count(*) FROM audit_log WHERE actor_id = $1)`, second).Scan(&reservations, &keys, &audit)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("вторая команда: отказ no_availability; брони %d, ключи %d, аудит %d, события outbox %d → %d",
		reservations, keys, audit, before, e.outboxCount(t, ""))
	if reservations != 0 || keys != 0 || audit != 0 {
		t.Errorf("rejected command left traces: reservations=%d keys=%d audit=%d", reservations, keys, audit)
	}
	if after := e.outboxCount(t, ""); after != before {
		t.Errorf("rejected command produced an event: outbox %d → %d", before, after)
	}
	if e.maxBooked(t) != 1 {
		t.Errorf("inventory changed by the rejected command: %d", e.maxBooked(t))
	}
}
