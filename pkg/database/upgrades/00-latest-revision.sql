-- v0 -> v3 (compatible with v1+): Latest revision

CREATE TABLE sip_call (
    call_id        TEXT   NOT NULL,
    portal_id      TEXT   NOT NULL,
    room_id        TEXT   NOT NULL,
    receiver       TEXT   NOT NULL DEFAULT '',
    matrix_user    TEXT   NOT NULL DEFAULT '',
    direction      TEXT   NOT NULL,
    conference     TEXT   NOT NULL,
    lk_room        TEXT   NOT NULL,
    lk_identity    TEXT   NOT NULL,
    lk_participant TEXT   NOT NULL,
    state          TEXT   NOT NULL,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL,

    PRIMARY KEY (call_id)
);

-- Every user has a portal room of their own per number, so a room holds at most
-- one call at a time and an active call is looked up by room. The index is not
-- unique: ended calls are kept for a while.
CREATE INDEX sip_call_room_idx ON sip_call (room_id, state);

-- An inbound INVITE names the conference, not the call, so it is resolved back
-- to a call through this index.
CREATE INDEX sip_call_conference_idx ON sip_call (conference);

-- Inbound texts accepted from SIP and not yet in Matrix. See pkg/inbound.
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
