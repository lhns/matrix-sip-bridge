# ADR-0004: AMI as the sole Asterisk interface, not ARI

## Status

Superseded by [ADR-0008](0008-the-bridge-is-a-sip-endpoint.md)

## Context

Asterisk offers two programmable interfaces. ARI is a REST plus websocket API
that takes ownership of a channel through a Stasis application. AMI is an older
line-based protocol with actions and an event stream.

Everything the bridge needs exists in AMI: `MessageSend` for outbound SIP
MESSAGE (verified present), a dialplan `UserEvent` for inbound, `Originate` for
outbound calls, and `ConfbridgeJoin`/`ConfbridgeLeave` for call state.

## Decision

AMI only, over one connection shared by both halves. ARI is not used.

## Consequences

- Media never passes through the bridge. Asterisk parks the call in a
  ConfBridge and livekit-sip dials in; the bridge only issues instructions.
- The dialplan is part of the contract, not an implementation detail: it must
  fire the configured `UserEvent` with `From`, `To` and `Body` headers, and it
  must route into `ConfBridge(${CONFBRIDGE_NAME})`.
- AMI sends its secret in the clear, so the config has a `tls` switch.
- AMI has no request/response framing beyond `ActionID`, and values are
  newline-terminated. Every value written to the socket is scrubbed of CR and
  LF, or a message body could inject further AMI headers.
- Should the bridge ever need to manipulate a live channel's media, ARI is the
  fallback and this decision is revisited.
