# ADR-0016: Per-user logins and portals

## Status

Accepted, amended by [ADR-0017](0017-line-members-and-dial-rooms.md): the
connector sets a portal's parent space itself, under the portal's receiver.

Supersedes [ADR-0002](0002-relay-mode-instead-of-per-user-logins.md). Amends
[ADR-0003](0003-portal-room-per-e164-number.md). The portal ID of
[ADR-0014](0014-a-portal-id-is-scoped-to-a-line.md) is unchanged; the portal
key gains a receiver.

## Context

One shared `sip` login and one room per number made every Matrix user see every
conversation, and relay mode was needed to let anyone but the login's owner
send. The SIP server can name the person a call or text is for, and Asterisk
already rings per person, so the bridge can be per person too.

## Decision

- One `UserLogin` per Matrix user, with the MXID as its ID. The first INVITE or
  MESSAGE whose `sip.recipient_header` names a user creates it, if
  `bridge.permissions` gives that user `login`; otherwise the request is
  refused (403, or 404 for a value that is not an MXID). No sign-up step.
- The receiver is set explicitly: portals are `{<line>-<number>, <login ID>}`,
  and a line's space inherits its child's receiver. `bridge.split_portals` is
  not used: the flag cannot be turned off again, and its migration guesses
  receivers and deletes rooms it cannot assign.
- A request without the header is for the default recipient, the owner of the
  old shared login, kept in bridgev2's KV store.
- At startup, before the SIP endpoint listens, every receiver-less portal is
  re-keyed onto that owner's login with `ReIDPortal`, spaces first (the
  parent foreign key cascades to the children) and never into a portal that
  already has a room; such a portal is skipped with a warning and the old login
  kept. Then the old login is removed without cleaning up its rooms.
- Calls ring one leg per recipient, each in that person's room, all under the
  same conference. The bridge answers at most one leg per conference, so first
  answer wins is decided here and not left to the SIP server. Inbound media is
  set up after the Matrix answer and before the SIP 200, so a leg that loses
  never puts livekit-sip into the conference. The other recipients' rooms record
  `Answered by <name>`.
- A text sent on a line carries the line as the host of its `From` URI, as
  inbound ones do.

## Consequences

- Relay is unnecessary, and `bridge.relay` and `default_relays: [sip]` can go.
- Existing rooms are kept, with their history and Matrix room IDs.
- Metrics count per leg, so one ringing call is several; a leg answered by
  someone else has the `answered_elsewhere` outcome.
- Open: two users dialling the same number on the same line share one
  conference name, and so one ConfBridge.
