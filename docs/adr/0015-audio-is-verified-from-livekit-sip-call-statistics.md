# ADR-0015: Audio is verified from livekit-sip's call statistics, not from the bridge

## Status

Accepted

## Context

A call on this bridge can connect, hold, and end cleanly while both ends hear
silence. Nothing in the bridge, in Kubernetes, or in `sip_bridge_calls_total`
distinguishes such a call from a working one: signalling succeeds, the call is
counted `answered`, every pod is `1/1 Running`. The failure is reported by a
human saying "I can't hear anything".

It has more than one cause, which is why the check is defined against the
symptom rather than any of them.
[ADR-0010](0010-element-call-filters-audio-by-rtc-membership.md) records one:
the client filters audio by RTC membership, a participant identity that does not
match is inaudible, and **the server-side checks give a false positive** —
LiveKit reports the track subscribed and healthy. A blocked UDP path to the SFU
is another: the client never reaches LiveKit, publishes nothing, and the SIP leg
has nothing to subscribe to. Both produce the same `call statistics` line.

The bridge cannot fix that by watching itself. It is not in the media path
([ADR-0005](0005-calls-bridged-as-media-via-livekit-sip.md)): it carries a
control leg for a few hundred milliseconds, answers an SDP it never uses
(`pkg/siptransport/sdp.go`), and hangs up. Audio goes Asterisk <-> livekit-sip
<-> the LiveKit SFU <-> the client, entirely past it. Any bridge-side assertion
about audio would be a test of nothing.

Options considered.

- **A test in this repo.** There is no media to exercise. The SDP answer is
  already covered, and it is answered on a leg that carries no audio.
- **A media harness in CI** — Asterisk, livekit-sip, the SFU, Redis and a
  synthetic LiveKit publisher in containers, RTP in one end and PCM out the
  other. It would exercise livekit-sip against a publisher of our own making,
  which is precisely the part that is not failing; both known causes live past
  livekit-sip and have no counterpart in such a harness.
- **livekit-sip's own Prometheus endpoint** (`prometheus_port`, unset on this
  deployment) would be the first-class source if it carried the figures. It does
  not: v1.14.0 exports `invite_accepted`, `invite_error`, `calls_active`,
  `calls_terminated`, `transfers_total`, `sdp_parsed_total`, `codec_offered_total`
  and a session-duration histogram. Every one of them counts the call, not the
  audio, so all of them are green in the failing case. `input_packets` and
  `track_subscribes` appear in that binary only as JSON tags of the log blob.
- **The LiveKit server API.** `ListParticipants` can see, live, that the Matrix
  participant published no audio track — which the log line cannot, since it
  arrives at teardown. It is blind to the other half: it reported the track
  subscribed and healthy in the ADR-0010 case. A useful complement, not a
  substitute, and not built here.
- **Reading livekit-sip's log.** It emits one `call statistics` line per
  finished call, whose `stats` blob counts both ends of the media path
  separately.

`stats.port.audio_packets` is large in the broken case as well as the working
one, because the far end always sends audio. `stats.room.input_packets` and
`stats.room.track_subscribes` are zero only in the broken case. That asymmetry
is the entire signal, and the log line is the only place either end of the media
path is counted separately at all.

## Decision

Verify audio by parsing livekit-sip's `call statistics` line, in `pkg/callaudit`
and `cmd/callaudit`, and assert on **both** directions rather than on the call
having happened. The same verdict serves two modes: an exit status for a probe
run after a test call, and Prometheus series for a process tailing the log.

Four rules the implementation exists to hold:

- **A run that judged no call exits non-zero.** A probe that placed no call, or
  scraped the wrong window, otherwise reports green — which is the same class
  of bug as the silence it is looking for.
- **A `call statistics` line that cannot be read is a failure, not a skip.** The
  upstream field names are the whole contract; a rename would turn every call
  into a silent zero, indistinguishable from the fault.
- **The packet floor gates the call, not each direction.** The two counters are
  not on the same scale — an observed working call carried 956 SIP packets
  against 51 room ones — so a floor big enough to dismiss a hangup during ring is
  within a factor of two of a real call's room side. Above the floor, each
  direction is judged on zero versus non-zero.
- **No field carrying a number or a room is parsed at all.** The line holds the
  caller in `participant`, `participantName`, `toUser` and `reqUser`, and the
  Matrix room in `room`. The cardinality rule in `pkg/metrics` applies, and by
  construction rather than by redaction.

## Consequences

- The check is coupled to a log line livekit-sip does not version. That is the
  cost of it being the only evidence, and the malformed-line counter is what
  makes the coupling break loudly.
- It is a packet count, not an ear. Audio of the wrong thing, at the wrong
  level, or with unusable jitter reads as working.
- It detects; it does not diagnose. Every cause above lands on the same verdict,
  so a firing check narrows the fault to "past livekit-sip, toward the client"
  and no further.
- The two directions are livekit-sip's own receive paths. A client that never
  subscribes to the SIP participant's track is therefore **not** detected: the
  room still delivers the client's audio (`input_packets` non-zero) and
  livekit-sip still publishes into it (`published_frames` large), so the line
  reads as a working call while Matrix hears nothing. No field in it counts the
  subscribers of a published track. `ListParticipants`, listed above as a
  complement, is what would see this.
- Only finished calls are visible, because the line is emitted at teardown.
  There is no signal at all during a call, so this cannot be a readiness probe.
- A container cannot read another container's stdout, so this cannot run as a
  sidecar in the livekit-sip pod. It reads `pods/log` through the API server —
  a CronJob, a small Deployment, or a laptop after a test call.
- `direction` in the line is livekit-sip's and is the inverse of the bridge's:
  livekit-sip only ever originates, so a call from the PSTN is `outbound` to it.
  Joining it to `sip_bridge_calls_total`'s `direction` reverses every figure.
- `callaudit` ships in the bridge image rather than one of its own, so it has no
  tag of its own to keep in step.
