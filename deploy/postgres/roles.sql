-- Роли данных сервисов (РХД ПР-2, TRADEOFFS T111). Выполняется при КАЖДОМ
-- запуске compose службой dbinit: файл идемпотентен, поэтому роли появятся
-- и в базе, созданной до их введения.
--
--   <svc>_svc — владелец базы и таблиц: миграции, ретранслятор outbox,
--               служебные задачи. Только он может снять RLS или триггер аудита.
--   <svc>_app — роль запросов пользователей: только SELECT/INSERT/UPDATE/
--               DELETE; права на таблицы выдаёт сервис при старте (pg.GrantApp).
DO $$
DECLARE s text;
BEGIN
  FOREACH s IN ARRAY ARRAY['identity', 'catalog', 'booking', 'notification'] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = s || '_app') THEN
      EXECUTE format('CREATE ROLE %I LOGIN PASSWORD %L', s || '_app', s || '_app_pw');
    END IF;
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I', s, s || '_app');
  END LOOP;
END $$;
