# ADR-0014: A portal ID is scoped to a line and spells its own number

## Status

Accepted

Amends [ADR-0003](0003-portal-room-per-e164-number.md).

## Context

ADR-0003 made the portal ID the number in E.164 with the leading `+` stripped.
That assumed one trunk: a number identified a conversation because there was
only one way to reach it.

The SIP server now namespaces a conversation by *line*, and sends the bridge a
conference header of `<prefix><line>-<number>`. The part after the prefix is
the portal ID and always has been, so a portal ID became composite —
`home-+15551234567` — without anything in the bridge being told.

Every outbound path then turned that ID back into a number by prepending `+`,
which produced `sip:+home-+15551234567@…`. The SIP server's route regex refuses
it, so this failed closed rather than misdialling, but no call and no message
out of a portal created after lines arrived could connect.

The bridge must not learn which lines exist. Lines are the SIP server's
routing: a list in the bridge's config would be a second place to keep it
correct, and would be wrong the first time one is added.

## Decision

A portal ID is `<line>-<number>`, and the bridge parses **its own** identifier
with `^(.*)-(\+?[0-9]+)$`, anchored on the trailing number. Everything before
that is the line, whatever it contains, so `office-main-+15551234567` is the
line `office-main`. No list, no config key, no default line.

The number half carries its own `+`. `home-+15551234567` is E.164;
`office-1001` is a short number that only means something on its line. The
bridge therefore never has to guess whether a run of digits is a global number,
which is what prepending `+` unconditionally amounted to. `+` needs no escaping
in an MXID localpart — mautrix's `EncodeUserLocalpart` leaves it alone — so
`@sip_home-+15551234567:example.com` is a valid ghost.

An ID that does not match is used whole, exactly as before: a bare-digit ID
from before lines is stripped E.164 and gets its `+` back, and a malformed one
keeps its shape all the way to the SIP server, which refuses it.

`pkg/phonenum` owns the split, beside `ToID`: it is where the ID form is
already defined, and this is the bridge reading its own identifier rather than
anything about routing. `FromID`, which prepended `+` to whatever it was given,
is gone — the asymmetry between the two ID forms is not something a caller can
be trusted to remember.

## Consequences

- The room name carries the line (`+15551234567 (home)`); the ghost's display
  name and its `tel:` identifier do not. Two lines to the same human are two
  rooms, and only the room can say which is which — the ghost is the person.
- A ghost for a short, line-scoped number publishes no `tel:` identifier at
  all. It is not a global number, and a client offering it as a dial link would
  reach somewhere else.
- The conference name keeps the whole portal ID, so an inbound call and an
  outbound one on the same line meet in the same room. Only what goes on the
  wire as a number is split out.
- Portals are re-keyed: a number that already had a room before lines gets a
  second, line-scoped one. The old rooms keep working — they are legacy IDs and
  still resolve to the same E.164 number — but they are not where that line's
  new calls land.
- `ResolveIdentifier` and `!dial` still build a line-less portal key from a
  typed number, because a number alone does not say which line to use. Such a
  portal dials out correctly and is reachable by text, but an inbound call
  arrives on a line and therefore lands in that line's portal instead. Giving
  the bridge a default line would be exactly the routing knowledge this ADR
  keeps out of it; resolving it belongs with whatever teaches Matrix users to
  pick a line.
- An inbound SIP MESSAGE still keys its portal on the sender's number alone
  (ADR-0003's form). Text has no line in the protocol the way a call has a
  conference header.
