package messaging

// РХД ПР-5 на настоящей Kafka: poison pill уходит в DLQ, временная ошибка и
// недоступная DLQ не теряют событие, падение после коммита в базу даёт
// повторную доставку, которую отсекает inbox в PostgreSQL.
//
//	TEST_KAFKA_BROKERS=localhost:9092 TEST_PG_DSN=… go test ./internal/platform/messaging/
//
// Без TEST_KAFKA_BROKERS тесты с брокером пропускаются.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/segmentio/kafka-go"

	"github.com/shinilornekora/tourflow/backend/internal/platform/pgtest"
)

func TestIsPermanent(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("%w: bad", ErrPermanent), true},
		{&pgconn.PgError{Code: "22P02"}, true}, // «abc» вместо UUID
		{&pgconn.PgError{Code: "23503"}, true}, // нарушение ограничения
		{&pgconn.PgError{Code: "42501", Routine: "ExecWithCheckOptions", Message: "новая строка нарушает политику защиты на уровне строк"}, true},
		{&pgconn.PgError{Code: "42501", Message: "permission denied for table notifications"}, false}, // чинится настройкой
		{&pgconn.PgError{Code: "40001"}, false},
		{&pgconn.PgError{Code: "55P03"}, false}, // lock_timeout
		{errors.New("connection refused"), false},
		{context.DeadlineExceeded, false},
	} {
		if got := IsPermanent(c.err); got != c.want {
			t.Errorf("IsPermanent(%v) = %v", c.err, got)
		}
	}
}

type kafkaEnv struct {
	brokers []string
	pub     *KafkaPublisher
	log     *slog.Logger
	topic   string
}

func newKafka(t *testing.T, ctx context.Context, name string) *kafkaEnv {
	t.Helper()
	b := os.Getenv("TEST_KAFKA_BROKERS")
	if b == "" {
		t.Skip("TEST_KAFKA_BROKERS не задан")
	}
	k := &kafkaEnv{brokers: strings.Split(b, ","), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		topic: "test." + name + "." + uuid.NewString()[:8]}
	EnsureTopics(ctx, k.brokers, k.log, k.topic)
	k.pub = NewKafkaPublisher(k.brokers)
	t.Cleanup(func() { _ = k.pub.Close() })
	return k
}

func (k *kafkaEnv) envelope(t *testing.T) (uuid.UUID, []byte) {
	t.Helper()
	id := uuid.New()
	return id, []byte(`{"event_id":"` + id.String() + `","event_type":"X","schema_version":1,"occurred_at":"2026-11-16T09:00:00Z","aggregate_id":"` +
		uuid.NewString() + `","aggregate_version":1,"producer":"test","payload":{}}`)
}

// send публикует значения с одним ключом — в одну партицию, по порядку.
func (k *kafkaEnv) send(t *testing.T, ctx context.Context, values ...[]byte) {
	t.Helper()
	msgs := make([]Message, 0, len(values))
	for _, v := range values {
		msgs = append(msgs, Message{Topic: k.topic, Key: "same-key", Value: v})
	}
	if err := k.pub.Publish(ctx, msgs...); err != nil {
		t.Fatal(err)
	}
}

// run запускает потребителя группы на d или до stop() из обработчика.
func (k *kafkaEnv) run(ctx context.Context, d time.Duration, dlq Publisher, h func(ctx context.Context, env Envelope, stop func()) error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	c := &Consumer{Brokers: k.brokers, GroupID: "test-" + k.topic, Topics: []string{k.topic}, DLQ: dlq, Log: k.log,
		MaxAttempts: 2, Timeout: 2 * time.Second, MaxBackoff: 300 * time.Millisecond,
		Handler: func(hctx context.Context, env Envelope) error { return h(hctx, env, cancel) }}
	_ = c.Run(cctx)
}

func (k *kafkaEnv) dlq(t *testing.T, ctx context.Context) []kafka.Message {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{Brokers: k.brokers, GroupID: "dlq-reader-" + uuid.NewString(),
		GroupTopics: []string{k.topic + ".dlq"}, StartOffset: kafka.FirstOffset, MaxWait: 300 * time.Millisecond})
	defer r.Close()
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var out []kafka.Message
	for {
		m, err := r.FetchMessage(rctx)
		if err != nil {
			return out
		}
		out = append(out, m)
	}
}

func header(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// Poison pill между двумя хорошими событиями: оба хороших обработаны по
// порядку, яд — ровно один раз в DLQ с причиной; после перезапуска группы
// ничего не приходит повторно.
func TestPoisonPillGoesToDLQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "poison")
	g1, v1 := k.envelope(t)
	g2, v2 := k.envelope(t)
	k.send(t, ctx, v1, []byte("это не JSON"), v2)

	var mu sync.Mutex
	var handled []uuid.UUID
	k.run(ctx, 30*time.Second, k.pub, func(_ context.Context, env Envelope, stop func()) error {
		mu.Lock()
		defer mu.Unlock()
		handled = append(handled, env.EventID)
		if len(handled) == 2 {
			go func() { time.Sleep(500 * time.Millisecond); stop() }() // дать зафиксировать offset
		}
		return nil
	})
	if len(handled) != 2 || handled[0] != g1 || handled[1] != g2 {
		t.Fatalf("good events around the poison pill must be handled in order: %v", handled)
	}
	dead := k.dlq(t, ctx)
	if len(dead) != 1 || string(dead[0].Value) != "это не JSON" {
		t.Fatalf("poison pill must be in DLQ exactly once: %d", len(dead))
	}
	if !strings.Contains(header(dead[0], "dlq_reason"), "decode envelope") || header(dead[0], "dlq_source") != k.topic {
		t.Fatalf("DLQ headers: %+v", dead[0].Headers)
	}
	handled = nil
	k.run(ctx, 12*time.Second, k.pub, func(_ context.Context, env Envelope, _ func()) error {
		mu.Lock()
		handled = append(handled, env.EventID)
		mu.Unlock()
		return nil
	})
	if len(handled) != 0 {
		t.Fatalf("redelivered after restart: %v", handled)
	}
}

// Временная ошибка (база легла на несколько секунд): событие не уходит в
// DLQ и не теряется — потребитель повторяет, пока не получится, и только
// потом берётся за следующее.
func TestTransientErrorIsRetriedNotDropped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "transient")
	g1, v1 := k.envelope(t)
	g2, v2 := k.envelope(t)
	k.send(t, ctx, v1, v2)
	var mu sync.Mutex
	var handled []uuid.UUID
	calls := 0
	k.run(ctx, 40*time.Second, k.pub, func(_ context.Context, env Envelope, stop func()) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if env.EventID == g1 && calls <= 5 { // больше, чем порог тревоги MaxAttempts
			return errors.New("connection refused")
		}
		handled = append(handled, env.EventID)
		if len(handled) == 2 {
			go func() { time.Sleep(500 * time.Millisecond); stop() }()
		}
		return nil
	})
	if len(handled) != 2 || handled[0] != g1 || handled[1] != g2 {
		t.Fatalf("transient failure must delay, not drop or reorder: handled=%v calls=%d", handled, calls)
	}
	if dead := k.dlq(t, ctx); len(dead) != 0 {
		t.Fatalf("transient error sent %d records to DLQ", len(dead))
	}
}

// flakyDLQ первые fails публикаций отклоняет — «DLQ недоступна».
type flakyDLQ struct {
	Publisher
	mu    sync.Mutex
	fails int
	tried int
}

func (f *flakyDLQ) Publish(ctx context.Context, msgs ...Message) error {
	f.mu.Lock()
	f.tried++
	down := f.tried <= f.fails
	f.mu.Unlock()
	if down {
		return errors.New("dlq broker down")
	}
	return f.Publisher.Publish(ctx, msgs...)
}

// Упали и обработка (яд), и DLQ: offset не фиксируется, публикация в DLQ
// повторяется. Яд не потерян — он в DLQ, когда та поднялась, — и только
// после этого обработано следующее событие. Раньше ошибка DLQ лишь
// писалась в лог, а offset фиксировался: событие пропадало.
func TestDLQOutageDoesNotLoseEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "dlqdown")
	g, v := k.envelope(t)
	k.send(t, ctx, []byte("яд"), v)
	dlq := &flakyDLQ{Publisher: k.pub, fails: 3}
	var handled []uuid.UUID
	var triedWhenHandled int
	k.run(ctx, 40*time.Second, dlq, func(_ context.Context, env Envelope, stop func()) error {
		handled = append(handled, env.EventID)
		dlq.mu.Lock()
		triedWhenHandled = dlq.tried
		dlq.mu.Unlock()
		go func() { time.Sleep(500 * time.Millisecond); stop() }()
		return nil
	})
	if len(handled) != 1 || handled[0] != g {
		t.Fatalf("the event after the poison pill must be handled: %v", handled)
	}
	if triedWhenHandled != 4 {
		t.Fatalf("next event must wait until the DLQ accepted the poison pill (4th try), tried=%d", triedWhenHandled)
	}
	if dead := k.dlq(t, ctx); len(dead) != 1 || string(dead[0].Value) != "яд" {
		t.Fatalf("poison pill must reach DLQ exactly once after the outage: %d", len(dead))
	}
}

// Ошибка данных из базы (корректный JSON, но значение не лезет в столбец):
// сразу в DLQ, без повторов, партиция не стоит.
func TestDataErrorGoesToDLQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "dataerr")
	g1, v1 := k.envelope(t)
	g2, v2 := k.envelope(t)
	k.send(t, ctx, v1, v2)
	var handled []uuid.UUID
	calls := 0
	k.run(ctx, 30*time.Second, k.pub, func(_ context.Context, env Envelope, stop func()) error {
		calls++
		if env.EventID == g1 {
			return &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "abc"`}
		}
		handled = append(handled, env.EventID)
		go func() { time.Sleep(500 * time.Millisecond); stop() }()
		return nil
	})
	if len(handled) != 1 || handled[0] != g2 || calls != 2 {
		t.Fatalf("data error must not be retried or block the partition: handled=%v calls=%d", handled, calls)
	}
	if dead := k.dlq(t, ctx); len(dead) != 1 {
		t.Fatalf("data error must land in DLQ once, got %d", len(dead))
	}
}

// Падение после коммита в базу: обработчик одной транзакцией PostgreSQL
// отметил событие в inbox и записал эффект, после чего потребитель
// остановлен до фиксации offset (отмена контекста — для брокера это то же,
// что kill: offset не зафиксирован). После перезапуска группы событие
// приходит снова, inbox отсекает повтор — эффект ровно один.
func TestCrashAfterCommitIsRedeliveredAndDeduplicated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "crash")
	db := pgtest.Fresh(t, "notification_test", fstest.MapFS{"001.sql": {Data: []byte(InboxDDL +
		`CREATE TABLE effects (event_id uuid PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());`)}})
	g1, v1 := k.envelope(t)
	g2, v2 := k.envelope(t)
	k.send(t, ctx, v1, v2)
	var mu sync.Mutex
	deliveries := map[uuid.UUID]int{}
	crashed := false
	handler := func(hctx context.Context, env Envelope, stop func()) error {
		mu.Lock()
		deliveries[env.EventID]++
		mu.Unlock()
		err := pgx.BeginFunc(hctx, db, func(tx pgx.Tx) error {
			fresh, err := MarkProcessed(hctx, tx, env)
			if err != nil || !fresh {
				return err
			}
			_, err = tx.Exec(hctx, `INSERT INTO effects(event_id) VALUES ($1)`, env.EventID)
			return err
		})
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if env.EventID == g1 && !crashed { // коммит прошёл — и процесс убит до ответа брокеру
			crashed = true
			stop()
			<-hctx.Done()
			return hctx.Err()
		}
		if deliveries[g2] > 0 {
			go func() { time.Sleep(500 * time.Millisecond); stop() }()
		}
		return nil
	}
	k.run(ctx, 30*time.Second, k.pub, handler) // первый запуск: падение на g1
	if deliveries[g1] != 1 || deliveries[g2] != 0 {
		t.Fatalf("first run: %v", deliveries)
	}
	k.run(ctx, 30*time.Second, k.pub, handler) // перезапуск
	if deliveries[g1] != 2 {
		t.Fatalf("event must be redelivered after the crash, deliveries=%v", deliveries)
	}
	var effects, inbox int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM effects), (SELECT count(*) FROM processed_events)`).Scan(&effects, &inbox); err != nil {
		t.Fatal(err)
	}
	if effects != 2 || inbox != 2 {
		t.Fatalf("inbox must keep each effect exactly once: effects=%d inbox=%d deliveries=%v", effects, inbox, deliveries)
	}
}

// ---- РХД ПР-5, вариант Б: эволюция контракта и карантин ----

// Контракт конверта: что читатель терпит и что откладывает.
func TestEnvelopeContract(t *testing.T) {
	base := `"event_id":"` + uuid.NewString() + `","event_type":"X","aggregate_id":"` + uuid.NewString() +
		`","aggregate_version":1,"producer":"test","payload":{}`
	for _, c := range []struct {
		name      string
		json      string
		permanent bool
	}{
		{"valid", `{` + base + `,"schema_version":1,"occurred_at":"2026-11-16T09:00:00Z"}`, false},
		// Новое необязательное поле в корне — совместимое изменение: читатель его не видит.
		{"extra optional field", `{` + base + `,"schema_version":1,"occurred_at":"2026-11-16T09:00:00Z","client_meta":{"ip":"127.0.0.1"}}`, false},
		// Незнакомая версия схемы: смысл полей мог измениться — в карантин.
		{"schema_version 999", `{` + base + `,"schema_version":999,"occurred_at":"2026-11-16T09:00:00Z"}`, true},
		{"schema_version missing", `{` + base + `,"occurred_at":"2026-11-16T09:00:00Z"}`, true},
		// Нулевое время события: издатель нарушил контракт, повтор это не исправит.
		{"zero occurred_at", `{` + base + `,"schema_version":1,"occurred_at":"0001-01-01T00:00:00Z"}`, true},
		{"occurred_at missing", `{` + base + `,"schema_version":1}`, true},
		{"no event_type", `{"event_id":"` + uuid.NewString() + `","schema_version":1,"occurred_at":"2026-11-16T09:00:00Z","payload":{}}`, true},
	} {
		var env Envelope
		if err := json.Unmarshal([]byte(c.json), &env); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err := ValidateEnvelope(env)
		if (err != nil) != c.permanent || (err != nil && !errors.Is(err, ErrPermanent)) {
			t.Errorf("%s: ValidateEnvelope = %v, want permanent=%v", c.name, err, c.permanent)
		}
	}
}

// Б-1 и Б-2 на настоящей Kafka с inbox в PostgreSQL. В тему уходят по порядку:
//
//	e1 — обычное событие;
//	e1 ещё раз, но с новым необязательным полем client_meta и тем же event_id;
//	q  — новое событие со schema_version 999;
//	z  — новое событие с нулевым occurred_at;
//	e2 — обычное событие.
//
// Ожидается: расширенная копия e1 доходит до обработчика и отсекается inbox
// как дубликат (в DLQ не уходит); q и z уходят в DLQ, не доходя до
// обработчика, и не оставляют следов в inbox и в эффектах; e2 обрабатывается
// следом без ручного вмешательства.
func TestContractEvolutionAndQuarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := newKafka(t, ctx, "contract")
	db := pgtest.Fresh(t, "notification_test", fstest.MapFS{"001.sql": {Data: []byte(InboxDDL +
		`CREATE TABLE effects (event_id uuid PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());`)}})
	mk := func(id uuid.UUID, schema int, occurred, extra string) []byte {
		return []byte(fmt.Sprintf(`{"event_id":"%s","event_type":"X","schema_version":%d,"occurred_at":"%s",`+
			`"aggregate_id":"%s","aggregate_version":1,"producer":"test","payload":{}%s}`, id, schema, occurred, uuid.NewString(), extra))
	}
	const at = "2026-11-16T09:00:00Z"
	e1, q, z, e2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	k.send(t, ctx,
		mk(e1, 1, at, ""),
		mk(e1, 1, at, `,"client_meta":{"ip":"127.0.0.1"}`),
		mk(q, 999, at, ""),
		mk(z, 1, "0001-01-01T00:00:00Z", ""),
		mk(e2, 1, at, ""))

	var mu sync.Mutex
	var statuses []string // что вернул обработчик на каждую доставку
	handler := func(hctx context.Context, env Envelope, stop func()) error {
		status := "duplicate"
		err := pgx.BeginFunc(hctx, db, func(tx pgx.Tx) error {
			fresh, err := MarkProcessed(hctx, tx, env)
			if err != nil || !fresh {
				return err
			}
			status = "processed"
			_, err = tx.Exec(hctx, `INSERT INTO effects(event_id) VALUES ($1)`, env.EventID)
			return err
		})
		if err != nil {
			return err
		}
		mu.Lock()
		statuses = append(statuses, env.EventID.String()[:8]+":"+status)
		mu.Unlock()
		if env.EventID == e2 {
			go func() { time.Sleep(500 * time.Millisecond); stop() }()
		}
		return nil
	}
	k.run(ctx, 40*time.Second, k.pub, handler)

	t.Logf("события: e1=%s q=%s (schema 999) z=%s (occurred_at = 0) e2=%s", e1.String()[:8], q.String()[:8], z.String()[:8], e2.String()[:8])
	t.Logf("статусы обработчика: %v", statuses)
	want := []string{e1.String()[:8] + ":processed", e1.String()[:8] + ":duplicate", e2.String()[:8] + ":processed"}
	if strings.Join(statuses, " ") != strings.Join(want, " ") {
		t.Fatalf("handler statuses: %v, want %v", statuses, want)
	}
	var effects, inbox, quarantinedInbox int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM effects), (SELECT count(*) FROM processed_events),
		(SELECT count(*) FROM processed_events WHERE event_id = ANY($1))`, []uuid.UUID{q, z}).Scan(&effects, &inbox, &quarantinedInbox); err != nil {
		t.Fatal(err)
	}
	if effects != 2 || inbox != 2 || quarantinedInbox != 0 {
		t.Errorf("effects=%d inbox=%d, quarantined events in inbox=%d; want 2, 2, 0", effects, inbox, quarantinedInbox)
	}
	t.Logf("в базе: эффектов %d, отметок inbox %d, из них карантинных %d", effects, inbox, quarantinedInbox)
	dead := k.dlq(t, ctx)
	if len(dead) != 2 {
		t.Fatalf("DLQ must hold the two quarantined events, got %d", len(dead))
	}
	for i, m := range dead {
		t.Logf("DLQ[%d]: исходное смещение %s, причина: %s", i, header(m, "dlq_src_offset"), header(m, "dlq_reason"))
	}
	// Сообщение в DLQ — исходные байты: версия схемы издателя сохранена.
	var first Envelope
	if err := json.Unmarshal(dead[0].Value, &first); err != nil || first.EventID != q || first.SchemaVersion != 999 {
		t.Errorf("DLQ[0]: %+v %v", first, err)
	}
	if r := header(dead[0], "dlq_reason"); !strings.Contains(r, "schema_version 999") {
		t.Errorf("DLQ[0] reason: %q", r)
	}
	if r := header(dead[1], "dlq_reason"); !strings.Contains(r, "occurred_at") {
		t.Errorf("DLQ[1] reason: %q", r)
	}
}
