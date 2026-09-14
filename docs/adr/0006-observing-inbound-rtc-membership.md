# ADR-0006: Observing inbound RTC membership

## Status

Accepted

## Context

Nothing in Matrix says "dial this number". The only signal that a Matrix user
wants a call is their `org.matrix.msc3401.call.member` state event appearing in
the portal room — including when they press Element X's native call button,
which is the normal path once a portal room exists.

bridgev2 does not deliver that event (see ADR-0001), so the question was whether
`br.Matrix.EventProcessor.On()` can be used directly for an arbitrary state
type, and whether it also fires for events that arrived encrypted.

What the mautrix-go v0.30.0 source says:

- `appservice.EventProcessor.Dispatch` looks handlers up as
  `ep.handlers[evt.Type]`, and `event.Type` is a struct of name plus class. Any
  type can be registered; there is no whitelist in the processor itself.
- `appservice.handleEvents` sets `Type.Class = event.StateEventType` whenever
  the transaction event has a state key, so the registered type must carry that
  class or the lookup misses.
- `Connector.postDecrypt` ends with `br.EventProcessor.Dispatch(ctx, decrypted)`,
  so a decrypted event goes through the same handler map. Encrypted delivery is
  therefore covered — and moot in practice, since Matrix state events are never
  encrypted.
- `Content.ParseRaw` returns `ErrUnsupportedContentType` for a type absent from
  `event.TypeMap`, which this one is, so `Content.Parsed` stays nil.

The fallback, had this not worked, was a plain `mautrix.Client` running `/sync`
as the bot alongside the appservice. It was not needed.

## Decision

Register `EventProcessor.On(event.Type{Type: "org.matrix.msc3401.call.member",
Class: event.StateEventType}, ...)` from the subsystem's `Start`, and resolve the
room with `Bridge.GetPortalByMXID`. Outbound memberships go out through
`MatrixAPI.SendState` with the same type.

## Consequences

- The content must be decoded from `Content.VeryRaw` by hand; `Content.Parsed`
  is always nil for this type.
- The registration reaches for the concrete `*matrix.Connector`, because
  bridgev2 exposes no interface for subscribing to arbitrary types. A different
  Matrix connector degrades to a no-op registrar and logs, rather than failing
  the bridge.
- The handler must ignore the bridge's own ghosts, or it answers itself.
- An empty content object is how a client leaves; there is no state deletion in
  Matrix. A membership past its `expires` is also treated as gone, because a
  crashed client never sends the leave.
