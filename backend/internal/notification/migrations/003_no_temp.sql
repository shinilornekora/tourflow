-- Роли данных временные таблицы не нужны (см. identity/002, booking/005).
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END $$;
