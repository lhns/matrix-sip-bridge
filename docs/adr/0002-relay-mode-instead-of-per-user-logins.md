# ADR-0002: Relay mode instead of per-user logins

## Status

Accepted

## Context

bridgev2 assumes one remote account per Matrix user: a `UserLogin` owns portals
and is the sender of outbound messages. There is no per-user account on the SIP
side. There is one SIP endpoint and one trunk, and its identity is held in the
bridge config.

## Decision

One static `UserLogin` with the fixed ID `sip`. `CreateLogin` returns a
`LoginProcess` whose `Start` immediately returns `LoginStepTypeComplete` with
that ID, so every Matrix user who logs in adopts the same login. Messages from
other users reach the network through relay mode:
`bridge.relay.enabled: true` and `bridge.relay.default_relays: [sip]`.

## Consequences

- Without those two settings the bridge appears to work but silently drops
  every message from anyone but the user who happened to log in. The connector
  logs a warning at startup for each of them.
- Relay's `message_formats` prefixes each message with the sender's name. That
  is right for a group chat and wrong for a text message with a length limit,
  so operators bridging SMS should blank those templates.
- The SIP identity is never per user, so nothing sensitive is stored in the
  `UserLogin` metadata.
