// Package messaging — события между сервисами.
//
// Схема доставки «at-least-once»:
//
//	use case ──(одна транзакция)──▶ бизнес-таблицы + outbox
//	relay ──▶ Kafka (ключ = id агрегата → порядок внутри агрегата)
//	consumer ──(одна транзакция)──▶ inbox(event_id) + эффект
//
// Дубликаты возможны на каждом шаге (relay упал после publish, consumer упал
// после commit, но до commit offset), поэтому обработчики идемпотентны по
// event_id, а проекции дополнительно сравнивают aggregate_version.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"

	"github.com/shinilornekora/tourflow/backend/internal/platform/logx"
)

// Топики. Версия в имени топика (v1) меняется только при несовместимом
// изменении конверта; эволюция полезной нагрузки — через schema_version.
const (
	TopicHotels      = "catalog.hotel.v1"
	TopicRoomTypes   = "catalog.room_type.v1"
	TopicReservation = "booking.reservation.v1"
)

// Envelope — конверт события, общий для всех топиков.
type Envelope struct {
	EventID          uuid.UUID       `json:"event_id"`
	EventType        string          `json:"event_type"`
	SchemaVersion    int             `json:"schema_version"`
	OccurredAt       time.Time       `json:"occurred_at"`
	AggregateID      uuid.UUID       `json:"aggregate_id"`
	AggregateVersion int64           `json:"aggregate_version"`
	Producer         string          `json:"producer"`
	RequestID        string          `json:"request_id,omitempty"`
	Payload          json.RawMessage `json:"payload"`
}

// NewEnvelope собирает конверт для записи в outbox.
func NewEnvelope(ctx context.Context, producer, eventType string, aggregateID uuid.UUID, version int64, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:          uuid.New(),
		EventType:        eventType,
		SchemaVersion:    1,
		OccurredAt:       time.Now().UTC(),
		AggregateID:      aggregateID,
		AggregateVersion: version,
		Producer:         producer,
		RequestID:        logx.RequestID(ctx),
		Payload:          raw,
	}, nil
}

// ---------- outbox ----------

// OutboxDDL — таблица, которую каждый сервис-производитель создаёт в своей
// миграции (здесь — для справки и тестов).
const OutboxDDL = `CREATE TABLE IF NOT EXISTS outbox (
	id           bigserial PRIMARY KEY,
	topic        text NOT NULL,
	msg_key      text NOT NULL,
	envelope     jsonb NOT NULL,
	created_at   timestamptz NOT NULL DEFAULT now(),
	published_at timestamptz,
	attempts     int NOT NULL DEFAULT 0,
	last_error   text,
	event_id     uuid NOT NULL UNIQUE,
	aggregate_version bigint NOT NULL,
	lease_owner  text,
	lease_until  timestamptz
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox (id) WHERE published_at IS NULL;`

// WriteOutbox кладёт событие в outbox в той же транзакции, что и
// бизнес-изменение. event_id и aggregate_version дублируются в колонках:
// по ним ищут и проверяют события, не разбирая jsonb.
func WriteOutbox(ctx context.Context, tx pgx.Tx, topic string, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO outbox(topic, msg_key, envelope, event_id, aggregate_version) VALUES ($1, $2, $3, $4, $5)`,
		topic, env.AggregateID.String(), body, env.EventID, env.AggregateVersion)
	if err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	return nil
}

// Publisher отправляет сообщение в брокер.
type Publisher interface {
	Publish(ctx context.Context, msgs ...Message) error
}

type Message struct {
	Topic   string
	Key     string
	Value   []byte
	Headers map[string]string
}

// Relay переносит записи outbox в брокер.
//
// Строки берутся в аренду короткой транзакцией (FOR UPDATE SKIP LOCKED +
// lease_until), публикация идёт уже ВНЕ транзакции, итог отмечается второй
// короткой транзакцией. Упавший ретранслятор не держит блокировок: его
// аренда истечёт, и строки заберёт другой — событие уйдёт повторно
// (at-least-once), повтор отсекает inbox потребителя.
//
// Порядок: один ретранслятор публикует по возрастанию id. При нескольких
// репликах порядок между пачками НЕ гарантирован — потребители обязаны
// сравнивать aggregate_version (и сравнивают).
type Relay struct {
	Pool      *pgxpool.Pool
	Publisher Publisher
	Log       *slog.Logger
	Interval  time.Duration
	BatchSize int
	// Lease — срок аренды пачки (по умолчанию 30 с): больше, чем тайм-аут
	// публикации, чтобы живой ретранслятор не потерял свою пачку.
	Lease time.Duration
	Owner string
}

func (r *Relay) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for {
			n, err := r.Once(ctx)
			if err != nil {
				if ctx.Err() == nil && r.Log != nil {
					r.Log.Warn("outbox relay iteration failed", "error", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// relayTx — короткая транзакция ретранслятора с объявленным контекстом:
// политика RLS на outbox пускает к чтению и отметке только его.
func (r *Relay) relayTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pgx.BeginFunc(tctx, r.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(tctx, `SELECT set_config('app.relay', 'on', true)`); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Once публикует одну пачку и возвращает число опубликованных записей.
func (r *Relay) Once(ctx context.Context) (int, error) {
	size := r.BatchSize
	if size <= 0 {
		size = 100
	}
	lease := r.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	if r.Owner == "" {
		r.Owner = uuid.NewString()
	}
	var ids []int64
	var msgs []Message
	err := r.relayTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE outbox SET lease_owner = $1, lease_until = now() + make_interval(secs => $2)
			WHERE id IN (SELECT id FROM outbox
				WHERE published_at IS NULL AND (lease_until IS NULL OR lease_until < now())
				ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED)
			RETURNING id, topic, msg_key, envelope`, r.Owner, lease.Seconds(), size)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var m Message
			if err := rows.Scan(&id, &m.Topic, &m.Key, &m.Value); err != nil {
				return err
			}
			var env Envelope
			if err := json.Unmarshal(m.Value, &env); err == nil {
				m.Headers = map[string]string{"event_type": env.EventType, "event_id": env.EventID.String()}
				if env.RequestID != "" {
					m.Headers["request_id"] = env.RequestID
				}
			}
			ids = append(ids, id)
			msgs = append(msgs, m)
		}
		return rows.Err()
	})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	// RETURNING не гарантирует порядок — восстанавливаем порядок id.
	sort.Sort(byID{ids, msgs})

	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	pubErr := r.Publisher.Publish(pubCtx, msgs...)
	cancel()

	// Итог отмечаем, даже если ctx уже отменён (остановка сервиса): иначе
	// успешно опубликованная пачка уйдёт повторно после истечения аренды.
	mctx := context.WithoutCancel(ctx)
	if pubErr != nil {
		_ = r.relayTx(mctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(mctx, `UPDATE outbox SET attempts = attempts + 1, last_error = $3, lease_owner = NULL, lease_until = NULL
				WHERE id = ANY($1) AND lease_owner = $2`, ids, r.Owner, pubErr.Error())
			return err
		})
		return 0, nil // счётчик попыток сохранён, пачку повторим позже
	}
	err = r.relayTx(mctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(mctx, `UPDATE outbox SET published_at = now(), lease_owner = NULL, lease_until = NULL
			WHERE id = ANY($1) AND lease_owner = $2`, ids, r.Owner)
		return err
	})
	if err != nil {
		return 0, err // опубликовано, но не отмечено: уйдёт повторно, inbox отсечёт
	}
	return len(ids), nil
}

type byID struct {
	ids  []int64
	msgs []Message
}

func (b byID) Len() int           { return len(b.ids) }
func (b byID) Less(i, j int) bool { return b.ids[i] < b.ids[j] }
func (b byID) Swap(i, j int) {
	b.ids[i], b.ids[j] = b.ids[j], b.ids[i]
	b.msgs[i], b.msgs[j] = b.msgs[j], b.msgs[i]
}

// ---------- inbox ----------

// InboxDDL — таблица обработанных событий потребителя.
const InboxDDL = `CREATE TABLE IF NOT EXISTS processed_events (
	event_id     uuid PRIMARY KEY,
	event_type   text NOT NULL,
	processed_at timestamptz NOT NULL DEFAULT now()
);`

// MarkProcessed возвращает false, если событие уже обрабатывалось.
// Вызывается в той же транзакции, что и эффект обработки.
func MarkProcessed(ctx context.Context, tx pgx.Tx, env Envelope) (bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO processed_events(event_id, event_type) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		env.EventID, env.EventType)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ---------- Kafka ----------

type KafkaPublisher struct {
	w *kafka.Writer
}

func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{w: &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{}, // ключ → партиция → порядок
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true,
		BatchTimeout:           20 * time.Millisecond,
	}}
}

func (p *KafkaPublisher) Publish(ctx context.Context, msgs ...Message) error {
	out := make([]kafka.Message, 0, len(msgs))
	for _, m := range msgs {
		km := kafka.Message{Topic: m.Topic, Key: []byte(m.Key), Value: m.Value}
		for k, v := range m.Headers {
			km.Headers = append(km.Headers, kafka.Header{Key: k, Value: []byte(v)})
		}
		out = append(out, km)
	}
	return p.w.WriteMessages(ctx, out...)
}

func (p *KafkaPublisher) Close() error { return p.w.Close() }

// EnsureTopics создаёт топики заранее (идемпотентно). Автосоздание в
// брокере тоже включено, но оно создаёт топик только при первой записи —
// потребитель, стартовавший раньше производителя, остался бы без партиций.
// Ключ партиционирования — id агрегата, поэтому 3 партиции дают
// параллелизм между агрегатами при сохранении порядка внутри агрегата.
func EnsureTopics(ctx context.Context, brokers []string, log *slog.Logger, topics ...string) {
	var all []string
	for _, t := range topics {
		all = append(all, t, t+".dlq")
	}
	for attempt := 1; attempt <= 30; attempt++ {
		err := createTopics(ctx, brokers, all)
		if err == nil {
			return
		}
		log.Warn("kafka not ready, retrying topic creation", "attempt", attempt, "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func createTopics(ctx context.Context, brokers []string, topics []string) error {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := kafka.DialContext(dctx, "tcp", brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	ctrl, err := conn.Controller()
	if err != nil {
		return err
	}
	cc, err := kafka.DialContext(dctx, "tcp", fmt.Sprintf("%s:%d", ctrl.Host, ctrl.Port))
	if err != nil {
		return err
	}
	defer cc.Close()
	cfgs := make([]kafka.TopicConfig, 0, len(topics))
	for _, t := range topics {
		cfgs = append(cfgs, kafka.TopicConfig{Topic: t, NumPartitions: 3, ReplicationFactor: 1})
	}
	err = cc.CreateTopics(cfgs...)
	if errors.Is(err, kafka.TopicAlreadyExists) {
		return nil
	}
	return err
}

// Handler обрабатывает одно событие.
type Handler func(ctx context.Context, env Envelope) error

// ErrPermanent помечает ошибку, которую бессмысленно повторять
// (битый формат) — событие сразу уходит в DLQ.
var ErrPermanent = errors.New("permanent failure")

// IsPermanent — повтор того же события даст ту же ошибку: явный
// ErrPermanent или ошибка данных PostgreSQL — класс 22 (неверное значение),
// 23 (нарушение ограничения) и отказ политики RLS при записи (42501 из
// функции сервера ExecWithCheckOptions; по имени функции, а не по тексту:
// текст зависит от lc_messages). Всё остальное — связь, тайм-аут, 40001,
// «нет прав на таблицу» — временное: это чинится не правкой события.
func IsPermanent(err error) bool {
	if errors.Is(err, ErrPermanent) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23") ||
			(pe.Code == "42501" && pe.Routine == "ExecWithCheckOptions")
	}
	return false
}

type Consumer struct {
	Brokers []string
	GroupID string
	Topics  []string
	Handler Handler
	DLQ     Publisher
	Log     *slog.Logger
	// MaxAttempts — после стольких неудач временная ошибка становится
	// тревогой в логе (ERROR). Повторы продолжаются.
	MaxAttempts int
	// Timeout — жёсткий предел одной попытки обработки и одной публикации
	// в DLQ (по умолчанию 10 с).
	Timeout time.Duration
	// MaxBackoff — потолок паузы между повторами (по умолчанию 5 с).
	MaxBackoff time.Duration
}

// Run читает сообщения до отмены ctx. At-least-once: offset фиксируется
// только для сообщения, судьба которого решена — обработано или лежит в DLQ.
//
//   - Постоянная ошибка (IsPermanent, битый конверт) — в DLQ, дальше.
//   - Временная (база недоступна, тайм-аут) — повтор с паузой до успеха.
//     Партиция стоит: порядок событий агрегата важнее, а терять корректное
//     событие нельзя.
//   - DLQ недоступна — публикация в DLQ повторяется, offset не фиксируется.
//   - Остановка посреди сообщения — offset не фиксируется, после перезапуска
//     сообщение придёт снова, повтор отсечёт inbox.
func (c *Consumer) Run(ctx context.Context) error {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        c.Brokers,
		GroupID:        c.GroupID,
		GroupTopics:    c.Topics,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		MaxWait:        500 * time.Millisecond,
		CommitInterval: 0, // синхронный commit после обработки
		StartOffset:    kafka.FirstOffset,
		// Топик может появиться позже, чем подписался потребитель.
		WatchPartitionChanges:  true,
		PartitionWatchInterval: 5 * time.Second,
	})
	defer r.Close()
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Log.Warn("kafka fetch failed", "error", err)
			if !c.pause(ctx, 3) {
				return nil
			}
			continue
		}
		if !c.handle(ctx, m) {
			return nil // остановка: offset этого сообщения не фиксируем
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := r.CommitMessages(cctx, m); err != nil {
			c.Log.Warn("kafka commit failed", "error", err)
		}
		cancel()
	}
}

// handle возвращает true, если судьба сообщения решена и offset можно фиксировать.
func (c *Consumer) handle(ctx context.Context, m kafka.Message) bool {
	var env Envelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		return c.toDLQ(ctx, m, fmt.Errorf("%w: decode envelope: %v", ErrPermanent, err), 1)
	}
	if err := ValidateEnvelope(env); err != nil {
		return c.toDLQ(ctx, m, err, 1)
	}
	hctx := logx.WithRequestID(ctx, env.RequestID)
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(hctx, c.timeout())
		err := c.Handler(actx, env)
		cancel()
		if err == nil {
			return true
		}
		if IsPermanent(err) {
			return c.toDLQ(ctx, m, err, attempt)
		}
		if ctx.Err() != nil {
			return false
		}
		if attempt == c.maxAttempts() {
			c.Log.Error("event stuck, still retrying",
				"event_type", env.EventType, "event_id", env.EventID, "attempt", attempt, "error", err)
		} else {
			c.Log.Warn("event handling failed, retrying",
				"event_type", env.EventType, "event_id", env.EventID, "attempt", attempt, "error", err)
		}
		if !c.pause(ctx, attempt) {
			return false
		}
	}
}

// MaxSchemaVersion — старшая версия схемы конверта, которую понимает этот
// код. Совместимые изменения (новые необязательные поля) версию не меняют:
// читатель их просто не видит (tolerant reader, T11). Версия растёт, только
// когда меняется смысл существующих полей, — и такое сообщение старый
// читатель обязан отложить в DLQ, а не обработать «как понял».
const MaxSchemaVersion = 1

// ValidateEnvelope проверяет контракт конверта до вызова обработчика.
// Нарушение — постоянная ошибка: повтор того же сообщения её не исправит.
func ValidateEnvelope(env Envelope) error {
	switch {
	case env.EventID == uuid.Nil:
		return fmt.Errorf("%w: envelope without event_id", ErrPermanent)
	case env.EventType == "":
		return fmt.Errorf("%w: envelope without event_type", ErrPermanent)
	case env.SchemaVersion < 1 || env.SchemaVersion > MaxSchemaVersion:
		return fmt.Errorf("%w: unsupported schema_version %d (supported 1..%d)", ErrPermanent, env.SchemaVersion, MaxSchemaVersion)
	case env.OccurredAt.IsZero():
		return fmt.Errorf("%w: envelope without occurred_at", ErrPermanent)
	}
	return nil
}

// toDLQ кладёт сообщение в <topic>.dlq, повторяя публикацию до успеха.
func (c *Consumer) toDLQ(ctx context.Context, m kafka.Message, cause error, attempts int) bool {
	c.Log.Error("event moved to DLQ", "topic", m.Topic, "offset", m.Offset, "error", cause)
	if c.DLQ == nil {
		return true
	}
	headers := map[string]string{
		"dlq_reason":     cause.Error(),
		"dlq_source":     m.Topic,
		"dlq_attempts":   fmt.Sprint(attempts),
		"dlq_consumer":   c.GroupID,
		"dlq_failed_at":  time.Now().UTC().Format(time.RFC3339),
		"dlq_src_offset": fmt.Sprint(m.Offset),
	}
	msg := Message{Topic: m.Topic + ".dlq", Key: string(m.Key), Value: m.Value, Headers: headers}
	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, c.timeout())
		err := c.DLQ.Publish(pctx, msg)
		cancel()
		if err == nil {
			return true
		}
		c.Log.Error("publish to DLQ failed, offset not committed", "attempt", attempt, "error", err)
		if !c.pause(ctx, attempt) {
			return false
		}
	}
}

func (c *Consumer) pause(ctx context.Context, attempt int) bool {
	d := time.Duration(attempt*attempt) * 100 * time.Millisecond
	limit := c.MaxBackoff
	if limit <= 0 {
		limit = 5 * time.Second
	}
	if d > limit {
		d = limit
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (c *Consumer) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

func (c *Consumer) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return 5
}

// Decode разбирает полезную нагрузку; ошибка формата — постоянная.
func Decode[T any](env Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		return v, fmt.Errorf("%w: decode %s payload: %v", ErrPermanent, env.EventType, err)
	}
	return v, nil
}
