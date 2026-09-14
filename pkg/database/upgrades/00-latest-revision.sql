-- v0 -> v1 (compatible with v1+): Latest revision

CREATE TABLE sip_call (
    call_id        TEXT   NOT NULL,
    portal_id      TEXT   NOT NULL,
    room_id        TEXT   NOT NULL,
    direction      TEXT   NOT NULL,
    conference     TEXT   NOT NULL,
    channel        TEXT   NOT NULL,
    lk_room        TEXT   NOT NULL,
    lk_identity    TEXT   NOT NULL,
    lk_participant TEXT   NOT NULL,
    state          TEXT   NOT NULL,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL,

    PRIMARY KEY (call_id)
);

-- A number can only be in one call at a time, so an active call is looked up
-- by portal. The index is not unique: ended calls are kept for a while.
CREATE INDEX sip_call_portal_idx ON sip_call (portal_id, state);

-- ConfbridgeJoin/Leave name the conference, not the call, so events are
-- resolved back to a call through this index.
CREATE INDEX sip_call_conference_idx ON sip_call (conference);
