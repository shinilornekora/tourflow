-- 1. Служебный столбец для блокировки строк реплики каталога.
--
-- Бронирование читает реплику SELECT … FOR SHARE, чтобы отель или категорию
-- не отключили посреди транзакции. PostgreSQL требует для этого право UPDATE
-- хотя бы на один столбец таблицы. Раньше роль данных имела UPDATE на всю
-- таблицу и могла прямым SQL поменять цену, вместимость и статус — то, по
-- чему считается бронь и инвариант «продано ≤ номеров». Теперь право
-- выдаётся только на этот столбец (migrations/embed.go); смысла у него нет.
ALTER TABLE hotel_replica ADD COLUMN row_lock boolean NOT NULL DEFAULT false;
ALTER TABLE room_type_replica ADD COLUMN row_lock boolean NOT NULL DEFAULT false;

-- 2. Записи аудита об отеле (hotel.*) пишет только служебный процесс.
--
-- После 003 менеджер мог дописать в журнал своего отеля запись
-- hotel.deactivation_processed от своего имени: автор виден, но это всё же
-- запись о шаге саги, которого не было. Теперь действия hotel.* разрешены
-- только роли system (проекция событий каталога).
DROP POLICY audit_insert ON audit_log;
CREATE POLICY audit_insert ON audit_log FOR INSERT WITH CHECK (
    actor_role = app_role()
    AND ((app_role() = 'system' AND actor_id IS NULL) OR actor_id::text = app_user())
    AND (action NOT LIKE 'hotel.%' OR app_role() = 'system')
    AND (app_can_see_hotel(hotel_id)
         OR (app_role() = 'customer' AND action LIKE 'reservation.%' AND app_owns_reservation(entity_id, hotel_id))));
