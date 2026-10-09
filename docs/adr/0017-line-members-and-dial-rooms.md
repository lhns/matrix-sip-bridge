# ADR-0017: Line members get a line's rooms, and a dial room to open more

## Status

Accepted

Amends [ADR-0014](0014-a-portal-id-is-scoped-to-a-line.md) and
[ADR-0016](0016-per-user-logins-and-portals.md).

## Context

A user's rooms on a line appear only once a call or text names them, so a
person who should be able to call out on a line has nowhere to start until
someone calls them. Opening a room for a number meant `!dial` (which also
places a call) or the management room's `start-chat`, which keys the room
line-less.

ADR-0014 keeps knowledge of lines out of the bridge: which line rings whom is
the SIP server's routing. Who should *have rooms* on a line is a different,
smaller fact, and the bridge cannot learn it from the wire before the first
call.

## Decision

- `network.line_members: {<line>: [<mxid>, …]}` names who has a line's rooms.
  At startup each member who `bridge.permissions` lets log in gets a login and
  a dial room, and with `line_spaces` on the line's space in their personal
  space, holding the dial room.
  Others are skipped with a warning. Removing a member deletes nothing; it is
  logged. The deployment generates the list from its routing, so it is not a
  second place to keep routing correct: the bridge still never decides who is
  rung or texted.
- A line's dial room, `<line> ☎`, is a portal with the ID `<line>-dial`. Like
  `<line>-space` it ends in a letter, so it can never be read as a number, and
  every path that dials or texts refuses it. A number typed there opens the
  user's room for it on that line, invites them, and replies with a link;
  `!dial <number>` there calls on that line. It accepts what `!dial` accepts:
  international format, no national numbers, since a dialling plan is routing.
- The connector puts a portal into its line's space itself, keyed under the
  portal's own receiver, instead of through `ChatInfo.ParentID`, which bridgev2
  keys receiver-less while `bridge.split_portals` is off. At startup a
  receiver-less space whose rooms all belong to one receiver is re-keyed onto
  that receiver, replacing that receiver's own space only if it holds no rooms.

## Consequences

- A member sees the line's space and dial room before any call reaches them.
- Config is read at startup, so a changed `line_members` takes effect on the
  next restart.
- Who is a member and who is rung are kept apart: a member need not be rung,
  and texts still go to whoever the SIP server names.
