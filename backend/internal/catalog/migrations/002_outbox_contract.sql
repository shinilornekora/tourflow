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

-- ============ RLS каталога (РХД ПР-2) ============
-- Арендатор — отель. Читать каталог может кто угодно (витрина публична),
-- менять — администратор или менеджер ЭТОГО отеля. Первый уровень — правила
-- домена (Hotel.CanEdit), второй — эти политики: забытая проверка в новом
-- коде не даст менеджеру отеля А изменить цены отеля Б.
CREATE FUNCTION app_role() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.role', true), '') $$;
CREATE FUNCTION app_user() RETURNS text LANGUAGE sql STABLE AS
    $$ SELECT coalesce(current_setting('app.user_id', true), '') $$;
CREATE FUNCTION app_can_edit_hotel(h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT app_role() = 'admin'
        OR (app_role() = 'hotel_manager' AND EXISTS (
            SELECT 1 FROM hotel_managers m WHERE m.hotel_id = h AND m.user_id::text = app_user()))
$$;

ALTER TABLE hotels ENABLE ROW LEVEL SECURITY;
ALTER TABLE hotels FORCE ROW LEVEL SECURITY;
CREATE POLICY hotels_read ON hotels FOR SELECT USING (true);
CREATE POLICY hotels_create ON hotels FOR INSERT WITH CHECK (app_role() = 'admin');
CREATE POLICY hotels_edit ON hotels FOR UPDATE USING (app_can_edit_hotel(id)) WITH CHECK (app_can_edit_hotel(id));

ALTER TABLE hotel_managers ENABLE ROW LEVEL SECURITY;
ALTER TABLE hotel_managers FORCE ROW LEVEL SECURITY;
CREATE POLICY hotel_managers_read ON hotel_managers FOR SELECT USING (true);
CREATE POLICY hotel_managers_assign ON hotel_managers FOR INSERT WITH CHECK (app_can_edit_hotel(hotel_id));
CREATE POLICY hotel_managers_remove ON hotel_managers FOR DELETE USING (app_can_edit_hotel(hotel_id));

ALTER TABLE room_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE room_types FORCE ROW LEVEL SECURITY;
CREATE POLICY room_types_read ON room_types FOR SELECT USING (true);
CREATE POLICY room_types_create ON room_types FOR INSERT WITH CHECK (app_can_edit_hotel(hotel_id));
CREATE POLICY room_types_edit ON room_types FOR UPDATE USING (app_can_edit_hotel(hotel_id)) WITH CHECK (app_can_edit_hotel(hotel_id));
