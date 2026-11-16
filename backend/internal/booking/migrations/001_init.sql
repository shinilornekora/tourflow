-- ============ Реплики данных каталога (только чтение для booking) ============
-- Booking не ходит в catalog синхронно: нужные поля приходят событиями и
-- хранятся локально. version защищает от применения устаревшего события.

CREATE TABLE hotel_replica (
    hotel_id uuid PRIMARY KEY,
    name     text NOT NULL,
    city     text NOT NULL,
    status   text NOT NULL,
    version  bigint NOT NULL
);

CREATE TABLE hotel_manager_replica (
    hotel_id uuid NOT NULL,
    user_id  uuid NOT NULL,
    PRIMARY KEY (hotel_id, user_id)
);
CREATE INDEX hotel_manager_replica_user_idx ON hotel_manager_replica (user_id);

CREATE TABLE room_type_replica (
    room_type_id   uuid PRIMARY KEY,
    hotel_id       uuid NOT NULL,
    name           text NOT NULL,
    capacity       smallint NOT NULL,
    total_rooms    int NOT NULL,
    price_amount   bigint NOT NULL,
    price_currency char(3) NOT NULL,
    status         text NOT NULL,
    version        bigint NOT NULL
);
CREATE INDEX room_type_replica_hotel_idx ON room_type_replica (hotel_id);

-- ============ Остатки ============
-- Одна строка = одна категория номеров на одни сутки. Строки создаются
-- лениво при первой брони на эту дату. Бронь на N ночей блокирует N строк
-- в порядке возрастания даты — единый порядок исключает взаимоблокировки
-- между встречными бронями (РХД-3).
CREATE TABLE inventory (
    room_type_id uuid NOT NULL,
    day          date NOT NULL,
    booked       int NOT NULL DEFAULT 0 CHECK (booked >= 0),
    PRIMARY KEY (room_type_id, day)
);

-- ============ Брони ============
CREATE TABLE reservations (
    id             uuid PRIMARY KEY,
    hotel_id       uuid NOT NULL,
    room_type_id   uuid NOT NULL,
    customer_id    uuid NOT NULL,
    check_in       date NOT NULL,
    check_out      date NOT NULL,
    guests         smallint NOT NULL CHECK (guests BETWEEN 1 AND 10),
    nights         int GENERATED ALWAYS AS (check_out - check_in) STORED,
    status         text NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
    total_amount   bigint NOT NULL CHECK (total_amount > 0),
    currency       char(3) NOT NULL,
    hotel_name     text NOT NULL,
    room_type_name text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    cancelled_at   timestamptz,
    cancelled_by   uuid,
    cancel_reason  text,
    version        bigint NOT NULL DEFAULT 1,
    CONSTRAINT reservations_dates CHECK (check_out > check_in AND check_out - check_in <= 30),
    CONSTRAINT reservations_cancel_consistency CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);
CREATE INDEX reservations_customer_idx ON reservations (customer_id, check_in DESC);
CREATE INDEX reservations_hotel_idx ON reservations (hotel_id, check_in);
CREATE INDEX reservations_future_confirmed ON reservations (hotel_id, check_in) WHERE status = 'confirmed';

-- ============ Идемпотентность (РХД-3) ============
-- Повтор POST с тем же Idempotency-Key возвращает ту же бронь. request_hash
-- ловит повтор ключа с другим телом — это ошибка клиента, а не повтор.
CREATE TABLE idempotency_keys (
    customer_id    uuid NOT NULL,
    idem_key       text NOT NULL CHECK (length(idem_key) BETWEEN 8 AND 128),
    request_hash   bytea NOT NULL,
    reservation_id uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, idem_key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- ============ Журнал аудита (РХД-2) ============
CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    hotel_id   uuid NOT NULL,
    actor_id   uuid,
    actor_role text NOT NULL,
    action     text NOT NULL,
    entity_id  uuid NOT NULL,
    details    jsonb NOT NULL DEFAULT '{}',
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_hotel_idx ON audit_log (hotel_id, id DESC);

-- Запись аудита нельзя изменить или удалить: триггер отклоняет UPDATE,
-- DELETE и TRUNCATE даже у владельца таблицы. Владелец может удалить сам
-- триггер — поэтому запросы пользователей идут под отдельной ролью данных,
-- у которой на это нет прав (002_hardening.sql, TRADEOFFS T41, T111).
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only (% blocked)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();

-- ============ Row Level Security (РХД-2) ============
-- Вторая линия обороны: даже если в коде забыли WHERE customer_id = ...,
-- база не отдаст чужие брони. Контекст ставится через set_config(..., true)
-- в каждой транзакции. Не выставили контекст — не видно ничего (fail-closed).
CREATE FUNCTION app_role() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.role', true), '') $$;
CREATE FUNCTION app_user() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.user_id', true), '') $$;

CREATE FUNCTION app_can_see_hotel(h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT app_role() IN ('admin', 'system')
        OR (app_role() = 'hotel_manager' AND EXISTS (
            SELECT 1 FROM hotel_manager_replica m
            WHERE m.hotel_id = h AND m.user_id::text = app_user()))
$$;

ALTER TABLE reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY reservations_access ON reservations
    USING (customer_id::text = app_user() OR app_can_see_hotel(hotel_id))
    WITH CHECK (customer_id::text = app_user() OR app_can_see_hotel(hotel_id));

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_read ON audit_log FOR SELECT USING (app_can_see_hotel(hotel_id));
CREATE POLICY audit_insert ON audit_log FOR INSERT WITH CHECK (app_role() <> '');

-- ============ Outbox / inbox ============
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    topic        text NOT NULL,
    msg_key      text NOT NULL,
    envelope     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts     int NOT NULL DEFAULT 0,
    last_error   text
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

CREATE TABLE processed_events (
    event_id     uuid PRIMARY KEY,
    event_type   text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);
