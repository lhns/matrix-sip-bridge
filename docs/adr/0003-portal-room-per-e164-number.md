# ADR-0003: A portal room per E.164 number

## Status

Amended by [ADR-0014](0014-a-portal-id-is-scoped-to-a-line.md), which scopes
the ID to a line and has the number carry its own `+`.

## Context

A phone number is the only identifier the SIP side offers. There is no account,
no contact list and no thread ID. Both halves of the bridge need to agree on
what a conversation is: a text and a call to the same number belong together.

## Decision

`PortalID` and ghost `UserID` are the number in E.164 with the leading `+`
stripped, e.g. `15551234567`. `Receiver` is left empty and
`bridge.split_portals` stays `false`, so a number has one room shared by all
Matrix users.

## Consequences

- `username_template: sip_{{.}}` yields `@sip_15551234567:example.com`, and the
  same string names the Asterisk ConfBridge (`sip-15551234567`) and the portal.
- Numbers must be normalised before they are used as an ID, or the same phone
  reaches two rooms. `pkg/phonenum` rejects anything without an international
  prefix rather than guessing a dialling plan.
- Shared portals mean any Matrix user in the room sees the whole conversation.
  That matches a shared trunk; it would be wrong for per-user numbers.
