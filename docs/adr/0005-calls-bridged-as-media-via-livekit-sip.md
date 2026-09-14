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

Bridge calls as real media, through a conference the bridge never enters.
Inbound: the SIP server dials the bridge with the conference name in a header,
the bridge publishes an RTC membership for the caller's ghost so Matrix rings,
and calls LiveKit's `CreateSIPParticipant` so livekit-sip dials that conference
and joins the LiveKit room. The bridge answers only once a Matrix user has
joined, and the dialplan then hangs its leg up so the caller falls into the
conference. Outbound: the bridge sends an INVITE carrying the same header and
does the same thing. See [ADR-0008](0008-the-bridge-is-a-sip-endpoint.md) for
how that replaced AMI.

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
- An unanswered call must not ring forever, so `ring_timeout` declines the
  bridge's branch with 480 and lets the other endpoints carry on.
- The bridge has no conference event and no channel of its own to watch, so the
  SIP participant leaving the LiveKit room is what ends a call. See
  [ADR-0010](0010-element-call-filters-audio-by-rtc-membership.md) for the
  membership lifecycle that hangs off it.
