# ADR-0013: A portal is a direct chat

## Status

Accepted

## Context

A portal room is one phone number and two parties. Element nevertheless
presented every call in one as a group call. The bridge never told anyone the
room was a DM.

`bridgev2.ChatInfo.Type` was not set, so the portal's room type stayed the
default. Three things follow from that type and from nothing else:

- `ReqCreateRoom.IsDirect` is `portal.RoomType == database.RoomTypeDM`, so the
  invite the Matrix user receives carries no `is_direct`.
- `updateUserLocalInfo` calls `MarkAsDM` — which writes the room into the
  user's `m.direct` account data through the double puppet — only for a DM
  portal with a known other user.
- The `m.bridge` content advertises the room type, which is what a client that
  reads bridge info rather than account data goes on.

Compared against a real 1:1 room and against a mautrix-whatsapp DM portal on
the same homeserver, the SIP portal was missing all three: absent from
`m.direct`, no `is_direct` on any invite, no room type in `m.bridge`. The
WhatsApp portal, which differs only in being created with the DM room type,
had all three.

Naming the other user is its own problem. bridgev2 will infer it from a
two-entry member list, but only from one marked `IsFull`, and a full list makes
bridgev2 remove every joined member the list omits — which is the Matrix user,
kicked out of their own room on every call. `ChatMemberList.OtherUserID` states
it outright and leaves `IsFull` false.

Room type is only ever read from `GetChatInfo`, and nothing in this bridge asks
for a portal's info again once its room exists. A portal created before this
change would therefore stay a group room forever.

## Decision

`GetChatInfo` returns `Type: database.RoomTypeDM` and
`Members.OtherUserID` set to the number's ghost. `IsFull` stays false.

`Connect` queues a chat resync for every portal that already has a room, so an
existing portal is reclassified once, at startup, rather than at the next call
or never.

## Consequences

- `matrix.sync_direct_chat_list` and a working double puppet are what turn the
  room type into `m.direct`. Without them the room type still reaches new
  rooms as `is_direct`, but an existing room gains nothing.
- `is_direct` is set at room creation and cannot be added afterwards. For rooms
  that already exist, `m.direct` is the only repair.
- `private_chat_portal_meta` is unrelated: it copies the ghost's name and
  avatar onto a DM portal, and does nothing here because the portal sets its
  own name.
