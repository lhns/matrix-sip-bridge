# ADR-0001: bridgev2 for messaging, a bespoke subsystem for calls

## Status

Accepted

## Context

The bridge has two halves with almost nothing in common. Text is a per-number
conversation with ghosts, portals and message dedup — exactly what
`maunium.net/go/mautrix/bridgev2` exists for. Calls are MSC3401 RTC memberships
and LiveKit media, which bridgev2 does not model at all.

bridgev2 was checked, not assumed:

- its inbound event whitelist (`bridgev2/matrix/connector.go:140`) lists sixteen
  event types and `org.matrix.msc3401.call.member` is not among them;
- there is no `HandleMatrixStateEvent` for a network connector to implement;
- there is no MSC3401, MatrixRTC or LiveKit code anywhere in mautrix-go v0.30.0,
  and the type is absent from `event.TypeMap`, so its content never parses.

## Decision

Use bridgev2 for messaging, forking `github.com/mautrix/twilio` as the skeleton.
Build the call half as a subsystem started from `NetworkConnector.Start()`. It
borrows bridgev2's ghosts and portals as identity and room substrate and none of
its event plumbing.

## Consequences

- Messaging gets portals, dedup, relay mode, provisioning and `-g` for free.
- Calls carry their own Matrix event subscription, their own state table and
  their own lifecycle; nothing about them appears in bridgev2's database.
- The two halves share one SIP endpoint, because a second registration of the
  same identity would be a competing contact for the same calls and messages.
- A future bridgev2 that grows MatrixRTC support would make the subsystem
  redundant, not wrong.
