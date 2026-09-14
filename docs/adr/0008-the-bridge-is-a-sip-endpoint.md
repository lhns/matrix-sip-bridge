# ADR-0008: The bridge is a SIP endpoint, not a manager client

## Status

Accepted. Supersedes [ADR-0004](0004-ami-as-the-sole-asterisk-interface.md).

## Context

ADR-0004 chose between AMI and ARI. Both are ways to drive one particular
Asterisk from outside; neither option in that decision was "be a SIP endpoint",
which is the third possibility and the one that makes the binary portable.

AMI cost what it was supposed to save. Call policy — which endpoints ring, in
what order, for how long — moved out of the dialplan and into bridge config,
duplicating the routing the same Asterisk already does for the XMPP half of
this household. The bridge had to be told the ConfBridge naming, the originate
channel string, the context and extension, and it still could not answer a call
without a `ConfbridgeJoin` event to hang the state machine on.

`github.com/emiago/sipgo` is a UAS without a media stack, which is exactly the
shape needed: signalling only, no RTP. livekit-sip depends on it too, which is
worth something for wire compatibility.

## Decision

The bridge is a SIP user agent. The SIP server dials it like any other
endpoint, and it places outbound calls by sending INVITE.

Inbound is a control leg, not a media leg: the server dials the bridge as one
branch of a parallel `Dial()` with the conference name in a header, the bridge
answers only once a Matrix user has joined, and the dialplan's `U()` gosub sets
`GOSUB_RESULT=CONTINUE` to hang the bridge's leg up and drop the caller into
the conference. Media flows Asterisk to livekit-sip to LiveKit and never
through this process.

Text messages are SIP MESSAGE in both directions, replacing AMI `MessageSend`
and the dialplan `UserEvent`.

The considered alternative was 3PCC: answer the server with livekit-sip's own
SDP, obtained through `CreateSIPParticipant.sip_request_uri`, so that the
bridge relays an offer instead of holding a control leg. It removes the gosub
from the dialplan contract, but it needs codecs pinned to PCMU/PCMA on both
sides and is much less proven. The control leg was chosen because every step of
it is a behaviour that can be pointed at in `app_dial.c`.

## Consequences

- Call policy is back in the dialplan. The bridge is one endpoint among
  several and does not decide who rings.
- The dialplan is still a contract, but a SIP-shaped one: a header name, a
  gosub, a conference naming convention. It is written out in the README.
- Answering is irreversible. `Dial()` hangs up every other branch with
  `ANSWERED_ELSEWHERE` as soon as one answers, so the bridge must never answer
  speculatively — hence 180, then media, then answer, in that order.
- The answer must carry a real SDP. A 200 OK with no body sets
  `SIP_PENDINGBYE` and Asterisk tears the call down; `c=0.0.0.0`, `a=inactive`
  or `a=sendonly` is read as hold and the caller gets music. The bridge
  therefore writes a plausible answer but opens no socket for it.
- SIP MESSAGE is `text/plain` only and capped at `SIP_MAX_PACKET_SIZE`
  (20480 bytes). TCP is required: a multi-part message that overflows a UDP
  datagram is dropped with no error anywhere. The bridge must advertise
  `MESSAGE` in `Allow` or chan_sip will not route text to it.
- The bridge no longer has a channel it can hang up. Ending a call from the
  Matrix side means removing the LiveKit participant, and whether the far end
  then hangs up is a property of the conference configuration.
- Nothing is left that is specific to Asterisk. Another SIP server that can
  add a header and run a conference would work.
