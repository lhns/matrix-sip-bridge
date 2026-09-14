# ADR-0005: Calls bridged as media via livekit-sip, not as notices

## Status

Accepted

## Context

The cheap option is to post "incoming call from +15551234567" as an
`m.notice` and let the human pick up a desk phone. That is not a bridge; the
call never reaches Matrix.

Element's call button starts a MatrixRTC session backed by LiveKit. LiveKit
ships `livekit-sip`, which can put a SIP leg into a LiveKit room. Asterisk can
park a call in a ConfBridge that livekit-sip then dials.

## Decision

Bridge calls as real media. Inbound: Asterisk parks the caller in
`ConfBridge(sip-<number>)`, the bridge publishes an RTC membership for the
caller's ghost so Matrix rings, and when a Matrix user joins the session the
bridge calls LiveKit's `CreateSIPParticipant` so livekit-sip dials the
ConfBridge and joins the room. Outbound: the bridge originates into the same
ConfBridge and does the same thing.

Two derived facts are load-bearing:

- The LiveKit room name is
  `unpadded_base64(sha256(compact_json([roomID, "m.call#ROOM"])))`, compact JSON,
  standard base64 alphabet, no padding. lk-jwt-service computes this; the bridge
  must produce the identical string or it joins an empty room of its own. Golden
  vectors from the MSC4195 appendix are in `pkg/calls/identity_test.go`.
- Inbound participants created by a LiveKit *dispatch rule* are named
  `sip_<caller>` by LiveKit and can never match a Matrix RTC membership. Only
  `CreateSIPParticipant` lets the identity be chosen, which is why the bridge
  always originates rather than accepting inbound at LiveKit.

The twirp API is called over its JSON encoding with a hand-written client rather
than vendoring `livekit/protocol`, which pulls in most of the LiveKit server SDK
for three RPCs.

## Consequences

- livekit-sip needs an outbound trunk object. It lives in Redis and is lost on a
  Redis restart, with no event to say so, so the bridge reconciles it at startup
  and on a timer (`ListSIPOutboundTrunk`, create if absent).
- Asterisk answers the inbound channel in order to park it, so an unanswered
  call would sit in silence forever. `ring_timeout` hangs it up.
- The ghost's RTC membership carries an expiry and is refreshed at half that
  interval; a bridge that dies mid-call leaves a membership that clients discard
  when it lapses.
