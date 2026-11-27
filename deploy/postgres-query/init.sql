-- Отдельный экземпляр PostgreSQL для модели чтения броней (booking-query).
-- Выполняется один раз при первом старте контейнера postgres-query.
--
-- Почему отдельный экземпляр, а не ещё одна база в общем: выборки экранов
-- (списки, сводки) не делят с транзакциями бронирования ни соединения, ни
-- shared_buffers, ни диск. Проекцию можно пересоздать переигровкой событий,
-- поэтому у неё другие требования к резервному копированию.
--
-- Роли — НЕ суперпользователи: суперпользователь обходит RLS.
--   booking_query_svc — владелец: миграции и проектор событий;
--   booking_query_app — роль API чтения: права выдаёт сервис при старте
--                       (pg.GrantApp), запись в проекцию ей запрещена.
CREATE ROLE booking_query_svc LOGIN PASSWORD 'booking_query_pw';
CREATE ROLE booking_query_app LOGIN PASSWORD 'booking_query_app_pw';
CREATE DATABASE booking_query OWNER booking_query_svc;
REVOKE CONNECT ON DATABASE booking_query FROM PUBLIC;
GRANT CONNECT ON DATABASE booking_query TO booking_query_svc, booking_query_app;
