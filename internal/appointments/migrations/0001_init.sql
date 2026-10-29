CREATE TABLE appointments.appointments (
    id                uuid PRIMARY KEY,
    owner_id          uuid        NOT NULL,
    session_id         uuid        NOT NULL,
    session_code       text        NOT NULL,
    reference         text        NOT NULL CHECK (char_length(reference) <= 40),
    slots             integer     NOT NULL,
    fee_cents         bigint      NOT NULL,
    status            text        NOT NULL,
    reason            text,
    letter_number      text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    slots_released_at timestamptz,
    UNIQUE (session_id, reference)
);
CREATE INDEX appointments_owner_id ON appointments.appointments (owner_id);
CREATE INDEX appointments_created_at ON appointments.appointments (created_at DESC);

CREATE TABLE appointments.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX appointments_outbox_unsent ON appointments.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE appointments.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);

