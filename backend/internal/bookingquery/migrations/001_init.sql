-- booking-query — модель чтения броней (CQRS). Отдельный экземпляр
-- PostgreSQL: тяжёлые выборки «карточки / списки / сводки» не конкурируют
-- за соединения и буферы с транзакциями бронирования. Проектор пишет
-- (под владельцем), API читает (под ролью данных, у которой нет прав на
-- запись — migrations.Deny).

-- Карточка брони: денормализованный снимок из событий booking.reservation.v1.
CREATE TABLE reservations_cards (
    reservation_id    uuid PRIMARY KEY,
    hotel_id          uuid NOT NULL,
    hotel_name        text NOT NULL,
    room_type_id      uuid NOT NULL,
    room_type_name    text NOT NULL,
    customer_id       uuid NOT NULL,
    check_in          date NOT NULL,
    check_out         date NOT NULL,
    nights            int GENERATED ALWAYS AS (check_out - check_in) STORED,
    guests            smallint NOT NULL,
    status            text NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
    total_amount      bigint NOT NULL,
    currency          char(3) NOT NULL,
    created_at        timestamptz NOT NULL,
    cancelled_at      timestamptz,
    cancel_reason     text,
    cancelled_by_role text,
    -- version — версия агрегата брони: устаревшее событие не перезаписывает
    -- более новое (порядок между пачками ретранслятора не гарантирован, T20).
    version           bigint NOT NULL,
    projected_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT reservations_cards_dates CHECK (check_out > check_in)
);
CREATE INDEX reservations_cards_customer_idx ON reservations_cards (customer_id, check_in DESC);
CREATE INDEX reservations_cards_hotel_idx ON reservations_cards (hotel_id, check_in DESC);

-- Назначения менеджеров — из снимка отеля (catalog.hotel.v1, manager_ids).
CREATE TABLE hotel_managers (
    hotel_id uuid NOT NULL,
    user_id  uuid NOT NULL,
    PRIMARY KEY (hotel_id, user_id)
);
CREATE INDEX hotel_managers_user_idx ON hotel_managers (user_id);

-- Последняя применённая версия отеля: отсекает устаревший снимок, иначе
-- запоздавшее событие вернуло бы снятого менеджера.
CREATE TABLE hotel_versions (
    hotel_id uuid PRIMARY KEY,
    name     text NOT NULL,
    city     text NOT NULL,
    status   text NOT NULL,
    version  bigint NOT NULL
);

-- Inbox проектора: event_id обработанных событий. Отметка и эффект —
-- одна транзакция; повторная доставка отсекается здесь.
CREATE TABLE projection_inbox (
    event_id     uuid PRIMARY KEY,
    event_type   text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX projection_inbox_processed_idx ON projection_inbox (processed_at);

-- ============ Row Level Security ============
-- Та же модель доступа, что в booking: клиент видит свои брони, менеджер —
-- брони своих отелей, администратор и проектор (system) — все. Без
-- контекста не видно ничего (fail-closed). FORCE — и для владельца.
CREATE FUNCTION app_role() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.role', true), '') $$;
CREATE FUNCTION app_user() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.user_id', true), '') $$;

CREATE FUNCTION app_can_see_hotel(h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT app_role() IN ('admin', 'system')
        OR (app_role() = 'hotel_manager' AND EXISTS (
            SELECT 1 FROM hotel_managers m
            WHERE m.hotel_id = h AND m.user_id::text = app_user()))
$$;

ALTER TABLE reservations_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE reservations_cards FORCE ROW LEVEL SECURITY;
CREATE POLICY cards_read ON reservations_cards FOR SELECT
    USING (customer_id::text = app_user() OR app_can_see_hotel(hotel_id));
-- Писать карточки может только проектор.
CREATE POLICY cards_write ON reservations_cards FOR ALL
    USING (app_role() = 'system') WITH CHECK (app_role() = 'system');

-- Роли данных временные таблицы не нужны (как в остальных сервисах).
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END $$;
