# ADR-0010: Element Call filters audio by RTC membership

## Status

Accepted, amended by ADR-0012

The identity derivation below is one of two schemes, and not the one this
bridge emits by default. What has to be in the membership for a client to use
it at all is ADR-0012.

## Context

The bridge could in principle drop a SIP participant into the LiveKit room and
let Matrix clients render whatever arrives. It cannot. Two findings in
element-hq/element-call, commit `5d38a13c46299b004f65355f24ea36eb80e0531c`:

- `src/livekit/MatrixAudioRenderer.tsx` filters audio tracks to
  `validIdentities`, derived from the Matrix RTC membership list, and only the
  survivors become `<AudioTrack>` elements. An unmatched participant is
  **inaudible as well as invisible**, and there is a unit test asserting it.
- `src/e2ee/sharedKeyManagement.ts:112`: if `room.hasEncryptionStateEvent()`,
  Element Call selects `E2eeType.PER_PARTICIPANT` and enables
  `manageMediaKeys`, so media is SFrame-encrypted with keys handed out over
  to-device messages. The bridge publishes plain audio, which such a client
  receives and cannot decode.

Subscription is *not* gated: `connect()` is called with no
`RoomConnectOptions`, `autoSubscribe` stays true, and there is no
`setSubscribed` anywhere. LiveKit's server API therefore reports an unmatched
track as subscribed and healthy. **Server-side checks give a false positive
here**; the only thing deciding whether a human hears the call is the
client-side identity filter.

Three further details, from matrix-js-sdk:

- `checkRtcMembershipData` in `src/matrixrtc/membershipData/rtc.ts` errors if
  `data.member.user_id !== sender`, so the membership must be sent as the ghost.
- `computeRtcIdentityRaw` is `unpaddedBase64(sha256(JSON.stringify([userId,
  deviceId, memberId])))`. `device_id` and `member.id` are arbitrary opaque
  strings; nothing looks them up.
- `makeMembershipStateKey` builds `${userId}_${deviceId}_${application}${slotId}`
  and prefixes it with `_` unless the room version allows user-owned state keys.

## Decision

Publish an `org.matrix.msc3401.call.member` event as the ghost for every call,
invent its `device_id` and `member.id`, and pass
`sha256([userID, deviceID, memberID])` as the participant identity to
`CreateSIPParticipant` — which is also the only API that lets the identity be
chosen at all.

Keep portal rooms unencrypted, and say so in the config and the README rather
than relying on a default. The bridge logs an error when it publishes a
membership into a room that has an `m.room.encryption` event.

The membership is published when the call starts and retracted when it ends. It
is not refreshed; `expires` defaults to six hours, which no call reaches.
Memberships left behind by a crash are reconciled once at startup against the
call table, and the LiveKit participant goes with each one: retracting only the
Matrix half left livekit-sip in the room and the caller in the conference with
nobody to talk to.

## Consequences

- Getting the identity hash wrong is silent. The call connects, LiveKit is
  healthy, the server reports the track subscribed, and nobody hears anything.
  The golden vectors in `pkg/calls/identity_test.go` are the guard, together
  with a test asserting the identity is derived from the same triple the
  published event carries.
- Turning on `encryption.default` later breaks calls with no error on either
  side. This is the most dangerous setting near this bridge. The mode-selection
  code was read; the full SFrame path was not traced, so treat it as
  high-confidence rather than proven and check it against a live client.
  ADR-0015 is how such a call is told apart from a working one after the fact:
  `room.track_subscribes` is 0 while the SIP side's packet count is not.
- The state key format is branched on the room version. An unreadable version
  falls back to the underscore-prefixed form, which every room version accepts.
- Between a crash mid-call and the restart, a phantom participant sits in the
  call UI. That is the accepted cost of having no heartbeat and no MSC4140
  delayed events: one fewer unstable MSC, and a pod restarts in seconds.
