-- Три границы, найденные рецензией прямым SQL под ролью данных.

-- 1. Контекст system — только для владельца таблиц.
--
-- Политики верят настройке app.role. Роль данных могла объявить себя system
-- и, например, дописать в аудит hotel.deactivation_processed. Служебный
-- контекст в booking нужен только процессам под владельцем (проекция
-- каталога, чистка, сверка), поэтому теперь он действует только для него:
-- у любой другой роли app.role = 'system' читается как пустая роль.
-- «Владелец» определяется по праву TRUNCATE на reservations: роли данных
-- сервис выдаёт только SELECT/INSERT/UPDATE/DELETE, это право есть лишь у
-- владельца таблицы. Проверка — встроенная функция без обращения к таблицам,
-- поэтому app_role() остаётся простым выражением и не замедляет политики
-- (они вычисляются для каждой строки).
CREATE OR REPLACE FUNCTION app_role() RETURNS text LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN coalesce(current_setting('app.role', true), '') = 'system'
             AND NOT pg_catalog.has_table_privilege('public.reservations', 'TRUNCATE') THEN ''
        ELSE coalesce(current_setting('app.role', true), '')
    END
$$;

-- 2. Функции политик не должны находить подменные таблицы.
--
-- app_can_see_hotel читала hotel_manager_replica без схемы, а у PUBLIC было
-- право создавать временные таблицы: CREATE TEMP TABLE hotel_manager_replica
-- подменяла источник прав. Имена теперь со схемой, право TEMP отозвано.
CREATE OR REPLACE FUNCTION app_can_see_hotel(h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT app_role() IN ('admin', 'system')
        OR (app_role() = 'hotel_manager' AND EXISTS (
            SELECT 1 FROM public.hotel_manager_replica m
            WHERE m.hotel_id = h AND m.user_id::text = app_user()))
$$;
CREATE OR REPLACE FUNCTION app_owns_reservation(r uuid, h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM public.reservations x
                   WHERE x.id = r AND x.hotel_id = h AND x.customer_id::text = app_user())
$$;
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END $$;

-- 3. Событие в outbox — только о своей брони и только правда о ней (РХД ПР-4).
--
-- Раньше вставка в outbox ничем не ограничивалась: код под ролью данных мог
-- дописать событие о чужой брони. Теперь событие проходит, только если его
-- автор ВИДИТ бронь-агрегат (функция читает reservations под RLS автора:
-- клиент — свои брони, менеджер — брони своего отеля, администратор и
-- служебный процесс — все) и конверт согласован с самой бронью: отель
-- строки, отель и клиент в полезной нагрузке — те же, что у брони. Иначе
-- менеджер под честным контекстом мог бы записать событие с чужим клиентом
-- в payload, и notification отправил бы уведомление не тому человеку.
CREATE FUNCTION app_event_matches_reservation(env jsonb, h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM public.reservations x
                   WHERE x.id = (env->>'aggregate_id')::uuid
                     AND x.hotel_id = h
                     AND env->'payload'->>'reservation_id' = x.id::text
                     AND env->'payload'->>'hotel_id' = x.hotel_id::text
                     AND env->'payload'->>'customer_id' = x.customer_id::text)
$$;
DROP POLICY outbox_append ON outbox;
CREATE POLICY outbox_append ON outbox FOR INSERT WITH CHECK (app_event_matches_reservation(envelope, hotel_id));
