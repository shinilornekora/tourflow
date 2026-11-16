-- ПР-2: уведомления видит только адресат. Контекст — app.user_id в
-- транзакции; обработчик событий пишет от имени system.
CREATE FUNCTION app_role() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.role', true), '') $$;
CREATE FUNCTION app_user() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.user_id', true), '') $$;

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY notifications_own ON notifications
    USING (user_id::text = app_user() OR app_role() = 'system')
    WITH CHECK (user_id::text = app_user() OR app_role() = 'system');

-- Порядок доставки между репликами ретранслятора не гарантирован (T20):
-- запоминаем последнюю версию брони, по которой уже было уведомление, и
-- устаревшее событие (например, «подтверждена» после «отменена») не
-- порождает уведомления.
CREATE TABLE reservation_versions (
    reservation_id uuid PRIMARY KEY,
    version        bigint NOT NULL
);
