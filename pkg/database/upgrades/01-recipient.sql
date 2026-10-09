-- v1 -> v2: Per-user portal rooms

ALTER TABLE sip_call ADD COLUMN receiver TEXT NOT NULL DEFAULT '';
ALTER TABLE sip_call ADD COLUMN matrix_user TEXT NOT NULL DEFAULT '';

DROP INDEX sip_call_portal_idx;
CREATE INDEX sip_call_room_idx ON sip_call (room_id, state);
