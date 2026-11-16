-- RLS каталога различает не только «чей отель», но и «чья роль» (РХД ПР-2).
--
-- По домену назначать менеджеров и менять статус отеля может только
-- администратор (Hotel.SetStatus, AssignManager). Политики 002 проверяли
-- только отель: менеджер прямым SQL мог добавить со-менеджера в свой отель
-- или отключить его — а hotel_managers через события становится источником
-- прав RLS в booking, и отключение отеля запускает массовую отмену броней.

-- Назначение и снятие менеджеров — только администратор.
DROP POLICY hotel_managers_assign ON hotel_managers;
DROP POLICY hotel_managers_remove ON hotel_managers;
CREATE POLICY hotel_managers_assign ON hotel_managers FOR INSERT WITH CHECK (app_role() = 'admin');
CREATE POLICY hotel_managers_remove ON hotel_managers FOR DELETE USING (app_role() = 'admin');

-- Статус отеля меняет только администратор. Политика UPDATE не различает
-- столбцы, поэтому — триггер: остальные поля менеджер править может.
CREATE FUNCTION hotels_status_admin_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status AND app_role() <> 'admin' THEN
        RAISE EXCEPTION 'hotel status can be changed by admin only'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER hotels_status_admin_only BEFORE UPDATE ON hotels
    FOR EACH ROW EXECUTE FUNCTION hotels_status_admin_only();

-- Функция политики читает таблицу со схемой, право создавать временные
-- таблицы отозвано: иначе CREATE TEMP TABLE hotel_managers подменяла бы
-- источник прав (то же исправление, что в booking).
CREATE OR REPLACE FUNCTION app_can_edit_hotel(h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT app_role() = 'admin'
        OR (app_role() = 'hotel_manager' AND EXISTS (
            SELECT 1 FROM public.hotel_managers m WHERE m.hotel_id = h AND m.user_id::text = app_user()))
$$;
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END $$;
