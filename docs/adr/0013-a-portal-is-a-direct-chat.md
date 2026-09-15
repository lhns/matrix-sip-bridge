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

### The room type also decides the camera and the audio route

Element X builds the Element Call URL itself — it never reads a widget state
event — and the only call parameter it sets is `intent`. Its choice is

```
direct && hasActiveCall -> isAudioCall ? JOIN_EXISTING_DM_VOICE : JOIN_EXISTING_DM
hasActiveCall           -> JOIN_EXISTING
direct                  -> isAudioCall ? START_CALL_DM_VOICE : START_CALL_DM
else                    -> START_CALL
```

where `direct` is the room being a DM and `isAudioCall` is the consensus of
`m.call.intent` over the room's active RTC memberships. **There is no non-DM
voice intent**, so in a room that is not a DM the `m.call.intent: "audio"` the
bridge already publishes is discarded before it reaches Element Call.

Element Call then derives everything from the intent:
`videoEnabled = callIntent != "audio"`, so a non-voice intent turns the camera
on; and on mobile the host controls the output devices, where the initial route
is the earpiece only for an audio intent and the speaker otherwise.

Element X reads the DM-ness from `m.direct` plus a member count that discounts
`io.element.functional_members`, so a portal absent from `m.direct` fails the
test however few people are in it.

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
- `private_chat_portal_meta` is a no-op for this bridge, not merely unrelated.
  It copies the ghost's name and avatar onto a DM portal, and bridgev2 skips
  that whenever the portal already has a name of its own — which `GetChatInfo`
  always gives it, the number. Setting it changes nothing.
- A call the Matrix side *starts* still opens with the camera on. Element X
  only offers a voice intent for a call that already exists, and the bridge
  cannot publish a membership before the user's own membership tells it to
  dial. Answering an inbound call is unaffected.
- On Android the audio route is a race, not a setting the intent decides
  ([element-x-android#6315](https://github.com/element-hq/element-x-android/issues/6315)):
  a call that connects quickly can land on the speaker even with
  `m.call.intent: audio`, and the same call on the same build can land on the
  earpiece. Nothing in the bridge can influence it — the intent is already
  published — so a report of "it came out of the speaker" from an Android
  client is not evidence that the membership is wrong. iOS and web are not
  affected.
- The bridge must not publish a video track. Element Call renders and counts
  video publications regardless of the intent.
