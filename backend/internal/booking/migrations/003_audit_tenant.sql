-- Запись аудита привязана к арендатору, а не только к автору.
--
-- Политика из 002 сверяла автора записи, но не отель: клиент или менеджер
-- чужого отеля мог дописать строку в журнал любого отеля — в том числе
-- hotel.deactivation_processed, единственный след саги отключения (T121).
-- Теперь автор должен иметь отношение к тому, о чём пишет:
--   • служебный процесс и администратор — любой отель;
--   • менеджер — только свой отель;
--   • клиент — только запись о собственной брони в отеле этой брони.
CREATE FUNCTION app_owns_reservation(r uuid, h uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM reservations x
                   WHERE x.id = r AND x.hotel_id = h AND x.customer_id::text = app_user())
$$;

DROP POLICY audit_insert ON audit_log;
CREATE POLICY audit_insert ON audit_log FOR INSERT WITH CHECK (
    actor_role = app_role()
    AND ((app_role() = 'system' AND actor_id IS NULL) OR actor_id::text = app_user())
    AND (app_can_see_hotel(hotel_id)
         OR (app_role() = 'customer' AND action LIKE 'reservation.%' AND app_owns_reservation(entity_id, hotel_id))));
