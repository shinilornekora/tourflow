-- ПР-4: служебные поля контракта вынесены из jsonb в колонки (event_id —
-- ключ дедупликации, aggregate_version — порядок версий агрегата), аренда
-- для ретранслятора: строки забираются короткой транзакцией, публикация
-- идёт уже вне её (T20).
ALTER TABLE outbox ADD COLUMN event_id uuid, ADD COLUMN aggregate_version bigint,
    ADD COLUMN lease_owner text, ADD COLUMN lease_until timestamptz;
UPDATE outbox SET event_id = (envelope->>'event_id')::uuid,
                  aggregate_version = (envelope->>'aggregate_version')::bigint;
ALTER TABLE outbox ALTER COLUMN event_id SET NOT NULL, ALTER COLUMN aggregate_version SET NOT NULL,
    ADD CONSTRAINT outbox_event_id_key UNIQUE (event_id);

-- Изоляция очереди (ПР-4). Слой 1 — права: роли данных выдан только INSERT
-- (Deny в cmd/*). Слой 2 — RLS с FORCE: даже владелец видит очередь,
-- только объявив транзакцию ретранслятором. Это страховка от ошибки в коде
-- владельца, а не от атаки: объявить себя ретранслятором может любой код с
-- правами владельца.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_append ON outbox FOR INSERT WITH CHECK (true);
CREATE POLICY outbox_relay_read ON outbox FOR SELECT USING (current_setting('app.relay', true) = 'on');
CREATE POLICY outbox_relay_mark ON outbox FOR UPDATE USING (current_setting('app.relay', true) = 'on');
CREATE POLICY outbox_relay_purge ON outbox FOR DELETE USING (current_setting('app.relay', true) = 'on');
ALTER TABLE outbox ADD COLUMN hotel_id uuid;
UPDATE outbox SET hotel_id = (envelope->'payload'->>'hotel_id')::uuid;

-- ПР-4: «ответ команды» сохраняется вместе с бронью и ключом. Повтор с тем
-- же Idempotency-Key возвращает именно этот ответ, а не текущее состояние
-- брони (после отмены текущее уже другое).
ALTER TABLE idempotency_keys ADD COLUMN response jsonb;
-- Ключи, созданные до этой миграции: ответ первого запроса восстанавливается
-- из брони (бронь создавалась подтверждённой, версии 1).
-- Брони закрыты RLS с FORCE и для владельца — читаем их от имени system.
SELECT set_config('app.role', 'system', true);
UPDATE idempotency_keys k SET response = jsonb_build_object(
    'ID', r.id, 'HotelID', r.hotel_id, 'RoomTypeID', r.room_type_id, 'CustomerID', r.customer_id,
    'Stay', jsonb_build_object('CheckIn', to_char(r.check_in, 'YYYY-MM-DD'), 'CheckOut', to_char(r.check_out, 'YYYY-MM-DD')),
    'Guests', r.guests, 'Status', 'confirmed',
    'Total', jsonb_build_object('amount', r.total_amount, 'currency', trim(r.currency)),
    'HotelName', r.hotel_name, 'RoomTypeName', r.room_type_name,
    'CreatedAt', to_char(r.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'CancelledAt', NULL, 'CancelledBy', NULL, 'CancelReason', '', 'Version', 1)
FROM reservations r WHERE r.id = k.reservation_id AND k.response IS NULL;

-- ПР-2: ключи идемпотентности видит только их клиент; служебная очистка —
-- под ролью system.
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_own ON idempotency_keys
    USING (customer_id::text = app_user() OR app_role() = 'system')
    WITH CHECK (customer_id::text = app_user());

-- Аудит пишется только от имени настоящего актора: клиент не может записать
-- событие от чужого имени или от имени system (раньше хватало любой роли).
DROP POLICY audit_insert ON audit_log;
CREATE POLICY audit_insert ON audit_log FOR INSERT WITH CHECK (
    actor_role = app_role()
    AND ((app_role() = 'system' AND actor_id IS NULL) OR actor_id::text = app_user()));

-- Фоновая сверка инварианта «inventory.booked = число подтверждённых броней
-- на эти сутки» (ПР-1, третий уровень защиты).
CREATE TABLE reconciliation_issues (
    id           bigserial PRIMARY KEY,
    room_type_id uuid NOT NULL,
    day          date NOT NULL,
    booked       int NOT NULL,
    confirmed    int NOT NULL,
    found_at     timestamptz NOT NULL DEFAULT now()
);
