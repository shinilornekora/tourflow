-- Выполняется один раз при первом старте контейнера postgres.
--
-- Один экземпляр PostgreSQL, но у каждого сервиса своя база и своя роль:
-- сервис физически не может прочитать чужие таблицы. Отдельные экземпляры
-- (как было бы в бою) в учебной сборке съели бы 4× памяти без новых знаний.
--
-- Роли — НЕ суперпользователи: суперпользователь обходит RLS.

CREATE ROLE identity_svc LOGIN PASSWORD 'identity_pw';
CREATE ROLE catalog_svc LOGIN PASSWORD 'catalog_pw';
CREATE ROLE booking_svc LOGIN PASSWORD 'booking_pw';
CREATE ROLE notification_svc LOGIN PASSWORD 'notification_pw';

CREATE DATABASE identity OWNER identity_svc;
CREATE DATABASE catalog OWNER catalog_svc;
CREATE DATABASE booking OWNER booking_svc;
CREATE DATABASE notification OWNER notification_svc;

-- Чужим ролям не разрешаем даже подключаться.
REVOKE CONNECT ON DATABASE identity, catalog, booking, notification FROM PUBLIC;
GRANT CONNECT ON DATABASE identity TO identity_svc;
GRANT CONNECT ON DATABASE catalog TO catalog_svc;
GRANT CONNECT ON DATABASE booking TO booking_svc;
GRANT CONNECT ON DATABASE notification TO notification_svc;

\connect identity
CREATE EXTENSION IF NOT EXISTS citext;
\connect catalog
CREATE EXTENSION IF NOT EXISTS pg_trgm;
