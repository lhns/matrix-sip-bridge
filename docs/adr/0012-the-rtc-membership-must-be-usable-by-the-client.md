# ADR-0012: The RTC membership must be usable by the client that reads it

## Status

Accepted

## Context

ADR-0010 established what the ghost's `org.matrix.msc3401.call.member` event is
for. It did not establish what has to be *in* it. Three things were wrong, and
each of them fails without a single log line anywhere in the system.

### The membership did not parse at all

`SessionMembershipData` in the receiving parser has two required fields the
bridge was not sending:

- `focus_active`, e.g. `{"type": "livekit", "focus_selection": "oldest_membership"}`
- `foci_preferred[]`, whose LiveKit variant requires **both** `livekit_alias`
  and `livekit_service_url`. `{"type": "livekit"}` alone does not deserialize.

A membership that fails to deserialize is dropped whole. The client then
believes the room has no active call, so it neither rings nor renders the
caller — while the state event sits in the room looking correct to anyone
reading it by hand.

Because `focus_selection` is `oldest_membership` and the bridge publishes
before the callee joins, the bridge's membership is normally the oldest one:
the focus it advertises is the focus everyone else will use. The service URL
is therefore not decoration and has to be the real one, which makes it config.

### `expires` is a duration, not a deadline

`expires` is milliseconds **from** `created_ts` (or from the event's own
timestamp when the client sends no `created_ts`). Read as a Unix timestamp, a
real client's four-hour join lands in 1970 — so the bridge read every Matrix
user's *join* as a *leave*, hung up on them mid-answer, and then answered the
SIP leg anyway because teardown and answer shared one signal.

### The participant identity scheme is deployment-specific

Element Call derives the set of LiveKit identities it will accept from the RTC
membership list and silently discards every track it cannot match. Two schemes
exist in the wild:

| scheme | member ID | LiveKit identity |
| --- | --- | --- |
| `user_device` | `<user id>:<device id>` | the member ID, verbatim |
| `hashed` (MSC4195) | opaque | `unpaddedBase64(sha256(json([user, device, member])))` |

They cannot both be served at once, and which one applies is a property of the
Element Call build the *clients* run, not of anything the bridge can see in the
room before it publishes. Detection is not possible on an inbound call: the
bridge publishes first, into a room that is usually empty.

The LiveKit **room name** is a separate derivation and is not affected — it is
the MSC4195 hash of the room ID and the slot under both schemes, confirmed
against the room a real Element Call client was placed in.

## Decision

Publish a complete membership: `focus_active`, `foci_preferred` carrying the
room ID as `livekit_alias` and the configured `calls.livekit.jwt_service_url`,
and `expires` as a duration. Read `expires` as a duration on the way in too,
based on `created_ts` when present and the event timestamp otherwise.

Make the identity scheme a config key, `calls.identity_scheme`, defaulting to
`user_device`. Keep the hashed derivation and its golden vectors as the other
scheme rather than deleting them; `LiveKitRoomName` keeps using the hash
unconditionally.

Derive the member ID and the participant identity from one pair of functions
so that the value published in the membership and the value handed to
`CreateSIPParticipant` cannot drift apart.

## Consequences

- **The diagnostic signature of a wrong identity is: both participants in the
  same LiveKit room, thousands of RTP packets received and republished, zero
  loss, every track reported subscribed — and silence.** Server-side health is
  a false positive here. The only check that means anything is whether the
  identity string equals the member ID in the membership the client can see.
- A deployment on a newer Element Call has to set `identity_scheme: hashed`.
  There is no way to serve both, and no way to detect which is needed, so this
  is the one thing about calls that must be chosen rather than defaulted
  correctly.
- `jwt_service_url` is required for calls to work at all, even though the
  bridge never contacts it.
