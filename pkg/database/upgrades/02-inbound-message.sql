-- v2 -> v3: Durable outbox for inbound texts

-- only: postgres for next 11 lines
CREATE TABLE sip_inbound_message (
    id          BIGSERIAL PRIMARY KEY,
    uid         TEXT   NOT NULL UNIQUE,
    login_id    TEXT   NOT NULL,
    portal_id   TEXT   NOT NULL,
    sender      TEXT   NOT NULL,
    recipient   TEXT   NOT NULL DEFAULT '',
    body        TEXT   NOT NULL,
    line        TEXT   NOT NULL DEFAULT '',
    received_at BIGINT NOT NULL
);
-- only: sqlite for next 11 lines
CREATE TABLE sip_inbound_message (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    uid         TEXT    NOT NULL UNIQUE,
    login_id    TEXT    NOT NULL,
    portal_id   TEXT    NOT NULL,
    sender      TEXT    NOT NULL,
    recipient   TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL,
    line        TEXT    NOT NULL DEFAULT '',
    received_at BIGINT  NOT NULL
);

-- The worker picks each portal's earliest-received text (headInboundQuery).
CREATE INDEX sip_inbound_message_head_idx ON sip_inbound_message (login_id, portal_id, received_at, id);
