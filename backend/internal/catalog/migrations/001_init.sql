CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE hotels (
    id          uuid PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 2 AND 200),
    city        text NOT NULL CHECK (length(city) BETWEEN 2 AND 100),
    address     text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    stars       smallint NOT NULL CHECK (stars BETWEEN 1 AND 5),
    photo_url   text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    version     bigint NOT NULL DEFAULT 1
);

-- Поиск по подстроке названия: ILIKE '%...%' не использует B-tree,
-- триграммный GIN-индекс делает его индексным (РХД, оптимизация запросов).
CREATE INDEX hotels_name_trgm ON hotels USING gin (name gin_trgm_ops);
CREATE INDEX hotels_city_idx ON hotels (lower(city)) WHERE status = 'active';
CREATE INDEX hotels_created_idx ON hotels (created_at DESC, id);

CREATE TABLE hotel_managers (
    hotel_id    uuid NOT NULL REFERENCES hotels (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hotel_id, user_id)
);
CREATE INDEX hotel_managers_user_idx ON hotel_managers (user_id);

CREATE TABLE room_types (
    id              uuid PRIMARY KEY,
    hotel_id        uuid NOT NULL REFERENCES hotels (id),
    name            text NOT NULL CHECK (length(name) BETWEEN 2 AND 120),
    description     text NOT NULL DEFAULT '',
    capacity        smallint NOT NULL CHECK (capacity BETWEEN 1 AND 10),
    total_rooms     int NOT NULL CHECK (total_rooms BETWEEN 1 AND 1000),
    price_amount    bigint NOT NULL CHECK (price_amount > 0),
    price_currency  char(3) NOT NULL DEFAULT 'RUB',
    amenities       text[] NOT NULL DEFAULT '{}',
    photo_url       text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    version         bigint NOT NULL DEFAULT 1,
    CONSTRAINT room_types_name_per_hotel UNIQUE (hotel_id, name)
);
CREATE INDEX room_types_hotel_idx ON room_types (hotel_id) WHERE status = 'active';

CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    topic        text NOT NULL,
    msg_key      text NOT NULL,
    envelope     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts     int NOT NULL DEFAULT 0,
    last_error   text
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;
