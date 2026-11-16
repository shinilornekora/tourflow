CREATE TABLE notifications (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL,
    kind           text NOT NULL,
    title          text NOT NULL,
    body           text NOT NULL,
    reservation_id uuid,
    source_event   uuid NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    read_at        timestamptz,
    CONSTRAINT notifications_source_unique UNIQUE (source_event, user_id)
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

CREATE TABLE processed_events (
    event_id     uuid PRIMARY KEY,
    event_type   text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);
