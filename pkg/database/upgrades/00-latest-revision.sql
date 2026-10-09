-- v0 -> v2 (compatible with v1+): Latest revision

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
