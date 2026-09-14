# ADR-0011: Ringing is a separate notification, and the portal must permit RTC membership

## Status

Accepted

## Context

ADR-0010 publishes an `org.matrix.msc3401.call.member` event as the ghost and
describes it as what makes Element ring. It is not. Two separate things were
missing, and each of them fails silently.

### The portal offered no call button

Power levels of a room Element itself creates, versus a portal room bridgev2
creates:

| | Element's room | portal |
|---|---|---|
| `state_default` | 50 | 50 |
| `events["org.matrix.msc3401.call.member"]` | **0** | absent |
| the Matrix user's level | 100 (creator) | `users_default`, i.e. 0 |

Element's own room-creation path carries an explicit power level override for
the membership event, because Element only exposes the call affordance in a
room where the local user may send it. A portal has neither: `GetChatInfo`
lists only the ghost, so bridgev2 leaves the owning Matrix user at
`users_default`, below `state_default`. There is no error and no greyed-out
button — the call UI is simply absent.

### Nothing rang, even where a call could be joined

The RTC membership makes a session **joinable**. What makes a client **ring**
is a separate MSC4075 event, `m.rtc.notification`. The bridge sent none, so an
inbound call sat in the room until `calls.ring_timeout` expired and was
declined.

The event is `notification_type: "ring"` with a `lifetime`, and it is retracted
by nothing: there is no cancel event, and the bridgev2 Matrix API has no
redaction. Its `lifetime` is therefore the only thing that stops a ring the
callee never answered. MSC4310 adds the other direction, an
`org.matrix.msc4310.rtc.decline` referencing the notification by
`m.reference`, which is what a client sends when the user rejects the call.

No homeserver ships a push rule for `m.rtc.notification`. The only default rule
that matches it is `.m.rule.is_user_mention`, which keys on
`content["m.mentions"].user_ids` regardless of event type, so the mentions
field is what turns the notification into a push and a ring on a phone that is
not in the foreground.

## Decision

Grant the RTC membership event power level 0 in every portal, under both its
unstable and stable names, through `ChatInfo.Members.PowerLevels`. Lower the
event rather than raise the user: the portal has exactly one privileged bot and
an unbounded set of human members, and `state_default` must stay at 50 so the
room cannot be encrypted or tombstoned by a member (ADR-0010).

bridgev2 applies those overrides while creating a room and never revisits them,
so the call subsystem re-applies the connector's `ChatInfo` to an existing
portal. That is what repairs portals created before this decision. It checks
the room's power levels first and does nothing when there is nothing to
repair, because re-sending room state on every call is both a write for no
reason and a change clients render.

`ChatInfo.Members.IsFull` stays false. The SIP side has no idea who is in a
portal on the Matrix side, and bridgev2 removes every joined member a full
list omits.

Send `m.rtc.notification` as the ghost when an inbound call starts ringing,
with `notification_type: "ring"`, `m.call.intent: "audio"`, a `lifetime` equal
to `calls.ring_timeout`, `m.mentions.user_ids` naming the room's human members,
and an `m.reference` to the membership event. Subscribe to the decline event
under both names and reject the SIP leg with `486 Busy Here` when one arrives
naming a notification this process sent.

No notification is sent for an outbound call: the Matrix side started it and is
already in the session, so notifying would ring the caller's own phone.

## Consequences

- The ring stops on its own at `calls.ring_timeout`, because the notification's
  lifetime is that timeout. Changing one without the other leaves either a
  phantom incoming call or a ring that dies before the bridge gives up.
- A declined call now ends immediately with `486` instead of ringing the caller
  out to `480`.
- Matching a decline to the notification it references, rather than to the room
  it arrived in, is what stops a late decline from a second device rejecting a
  later call to the same number.
- Re-applying chat info is a room-state write, so it must stay conditional.
  Marking the member list full turns it into a kick of the owning user, with
  the reason "User is not in remote chat", once per call.
- A decline for a notification sent by a previous run of the bridge is ignored;
  the map from notification to call is in memory, and a restart ends every call
  it names anyway.
