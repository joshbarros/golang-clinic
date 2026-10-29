CREATE TABLE scheduling.sessions (
    id          uuid PRIMARY KEY,
    code        text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    provider     text        NOT NULL,
    specialty     text        NOT NULL,
    location     text        NOT NULL,
    starts_at     timestamptz NOT NULL,
    capacity_slots integer     NOT NULL,
    remaining_slots  integer     NOT NULL,
    created_at  timestamptz NOT NULL,
    CONSTRAINT sessions_remaining CHECK (remaining_slots >= 0 AND remaining_slots <= capacity_slots)
);

CREATE TABLE scheduling.slot_holds (
    appointment_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    session_id        uuid        NOT NULL REFERENCES scheduling.sessions (id),
    slots          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE scheduling.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX sessions_outbox_unsent ON scheduling.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE scheduling.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
