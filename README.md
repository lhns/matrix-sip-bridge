# matrix-sip-bridge

[![CI](https://github.com/lhns/matrix-sip-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/lhns/matrix-sip-bridge/actions/workflows/ci.yml)

A Matrix bridge that is a SIP endpoint. Text messages and phone calls both land
in a portal room per phone number: an inbound SIP MESSAGE appears as an
`m.room.message` from a per-number ghost, and a Matrix message in that room goes
back out as a SIP MESSAGE. A call is bridged as real media, not as a
notification — the SIP server parks the far end in a conference and
`livekit-sip` dials into it to join the LiveKit room behind the room's Element
Call, so the call rings and is answered in Matrix.

The bridge is a user agent, not a remote control for one PBX. It signals and
never carries RTP: its own call leg exists to be answered and then hung up by
the dialplan, within a few hundred milliseconds, so that call policy stays in
the SIP server's routing. See
[ADR-0008](docs/adr/0008-the-bridge-is-a-sip-endpoint.md).

The two halves share one binary and one SIP endpoint but almost no code. The
messaging half is a [bridgev2](https://mau.fi/blog/megabridge-twilio/) network
connector, forked from the mautrix-twilio skeleton. The call half is its own
subsystem, because bridgev2 has no MSC3401 or LiveKit support and does not
deliver `org.matrix.msc3401.call.member` to network connectors at all; it
borrows bridgev2's ghosts and portals as identity and room substrate and none of
its event plumbing. See [ADR-0001](docs/adr/0001-bridgev2-for-messaging-bespoke-subsystem-for-calls.md)
and [ADR-0006](docs/adr/0006-observing-inbound-rtc-membership.md).

## Portal rooms must not be encrypted

Element Call selects per-participant SFrame encryption for any room that has an
`m.room.encryption` event, and distributes the keys over to-device messages. The
bridge publishes plain audio into LiveKit. In an encrypted portal room a call
therefore **connects, reports healthy on every server-side check, and is
silent** — no error appears on either side. Leave `encryption.default` and
`encryption.require` off. The bridge logs an error if it ever publishes a call
membership into an encrypted room. See
[ADR-0010](docs/adr/0010-element-call-filters-audio-by-rtc-membership.md).

## SIP server contract

This is the authoritative spec for the other side of the interface. The
dialplan and peer configuration are written separately, against this section.

### Peer

| | |
| --- | --- |
| Identity | `sip:<sip.username>@<sip.domain>`, default user part `matrix-sip-bridge` |
| Transport | **TCP.** A SIP packet is capped at 20480 bytes (`SIP_MAX_PACKET_SIZE`) and a multi-part text message that overflows a UDP datagram is dropped with no error anywhere |
| Port | whatever `sip.listen` says; 5060 by default |
| Contact | `sip.public_address`, which must be reachable from the SIP server — inside Kubernetes that is the Service address, not the pod's |
| Registration | off by default: define the bridge as a **static peer**. With `sip.register.enabled: true` it sends REGISTER to `sip.register.server` as `sip.username` with `sip.register.password`, digest auth, re-sent every `sip.register.expiry / 2` (2m30s by default) |

The bridge answers OPTIONS with 200 and an `Allow` listing every method it
handles, so an Asterisk `qualify` works against it.

### Inbound calls

The SIP server dials the bridge as one branch of a parallel `Dial()`, with the
conference name in a custom header.

What the bridge reads from the INVITE:

| Header | Use |
| --- | --- |
| `X-Conference` (name configurable as `sip.conference_header`) | **Required.** The conference the caller will land in. It must be `calls.conference_prefix` (default `sip-`) followed by the caller's number in E.164 without the plus, e.g. `sip-15551234567`. The part after the prefix becomes the portal ID, so the portal room and the ghost are named from this header and from nothing else |
| `From` | Logging only. Its user part is not used to route |
| Request-URI / `To` | Not used. Put anything routable there |
| Body | Must be an SDP offer containing PCMU (0) or PCMA (8) |

What the bridge sends back, in order:

| Status | When |
| --- | --- |
| `100 Trying` | immediately, from the transaction layer |
| `180 Ringing` | as soon as the conference header has been accepted and the Matrix side has been rung |
| `200 OK` with SDP | **only** once a Matrix user has actually joined the RTC session |
| `404 Not Found` | the conference header is missing, or does not carry the configured prefix |
| `480 Temporarily Unavailable` | nobody joined within `calls.ring_timeout` (45s by default) |
| `486 Busy Here` | a Matrix client declined the call |
| `488 Not Acceptable Here` | the INVITE carried no SDP, or offered neither PCMU nor PCMA |
| `500` / `503` | the bridge could not set the call up, or calls are disabled |

The 200 OK carries a real, non-held SDP answer: a 200 with no body sets
`SIP_PENDINGBYE` in chan_sip and the call is torn down, and `c=0.0.0.0`,
`a=inactive` or `a=sendonly` is parsed as hold and gives the caller music. No
socket is opened on the address and port it advertises, and nothing should send
RTP there.

**The dialplan must use `U(...)` and set `GOSUB_RESULT=CONTINUE`.** Without it,
answering bridges the caller's audio to the bridge, which has none, and the call
is a silent dead end. With it, the bridge's leg is hung up the instant it
answers and the caller carries on in the dialplan into `ConfBridge()`. The
bridge never answers speculatively, because `Dial()` hangs up every other branch
with `ANSWERED_ELSEWHERE` as soon as one answers.

A channel inside `ConfBridge()` cannot also `Dial()`, so the order is fixed:
Dial first, ConfBridge after.

### Outbound calls

When a Matrix user starts a call, the bridge sends an INVITE to
`calls.outbound_uri` with `{number}` replaced by the destination in E.164
including the plus, carrying the same conference header as inbound and an SDP
offer of PCMU and PCMA. The dialplan should read the header, dial the number
into that conference, and hang the bridge's control leg up. That leg carries no
media either.

### Ending a call

The bridge has no channel to hang up: its own leg is gone seconds into every
call. It ends a call from the Matrix side by removing livekit-sip's participant
from the LiveKit room, which drops that leg out of the conference. **Whether the
far end then hangs up is the conference's configuration, not the bridge's.**
Make livekit-sip's leg the marked user and the caller `end_marked`, or a Matrix
user hanging up leaves the caller alone in a conference.

In the other direction, the bridge notices that a call ended by polling LiveKit
for that participant every `calls.participant_poll_interval` (10s by default),
so a finished call is cleaned up within about that long.

### SIP MESSAGE

Both directions are SIP MESSAGE, `text/plain` only; anything else is answered
415. The whole request must stay under 20480 bytes, and the bridge refuses to
send one that would not.

- **To the bridge**: send to `sip:<anything>@<bridge host>:<port>`. The bridge
  takes the sender from the `From` header's URI user part and normalises it to
  E.164; a `From` it cannot read as a number is logged and dropped, because a
  portal keyed on a malformed identifier could never be replied to. The body is
  the message text, raw — not base64, and not carried in a custom header.
- **From the bridge**: to `messages.outbound_to` with `{number}` replaced by the
  destination in E.164 including the plus, from `messages.outbound_from`.
- chan_sip needs `accept_outofcall_message=yes` and an
  `outofcall_message_context`, globally and on the bridge's peer, or an inbound
  MESSAGE is refused before it reaches any dialplan.
- The bridge advertises `MESSAGE` in `Allow` on every response, which is what
  chan_sip checks before routing text to a peer.

### Worked example, for Asterisk

Illustrative only, and not part of the bridge: it shows the shape the contract
above expects. The application and option names were checked against the
Asterisk reference, but the flow has never been run end to end.

```
; ---- inbound: ring the bridge alongside the desk phone -------------------
[from-trunk]
exten => _+X.,1,Set(CONF=sip-${FILTER(0-9,${EXTEN})})
 same => n,SIPAddHeader(X-Conference: ${CONF})
 same => n,Dial(SIP/deskphone&SIP/matrixbridge/${EXTEN},30,U(matrix-answered))
 same => n,GotoIf($["${MATRIX_ANSWERED}"="1"]?conf)
 same => n,Hangup()
 same => n(conf),ConfBridge(${CONF},default_bridge,matrix_caller)
 same => n,Hangup()

; Runs on whichever leg answered. CONTINUE hangs that leg up and lets the
; caller carry on in the dialplan instead of being bridged to it.
[matrix-answered]
exten => s,1,GotoIf($["${CHANNEL(peername)}"!="matrixbridge"]?done)
 same => n,Set(MASTER_CHANNEL(MATRIX_ANSWERED)=1)
 same => n,Set(GOSUB_RESULT=CONTINUE)
 same => n(done),Return()

; ---- livekit-sip dials the conference by name ----------------------------
[from-livekit]
exten => _sip-X.,1,Answer()
 same => n,ConfBridge(${EXTEN},default_bridge,matrix_marked)

; ---- outbound: the bridge INVITEs sip:+15551234567@pbx -------------------
[from-matrixbridge]
exten => _+X.,1,Answer()
 same => n,Set(CONF=${SIP_HEADER(X-Conference)})
 same => n,Originate(SIP/trunk/${EXTEN},exten,matrix-conf,${CONF},1,,a)
 same => n,Hangup()

[matrix-conf]
exten => _sip-X.,1,ConfBridge(${EXTEN},default_bridge,matrix_caller)

; ---- inbound text --------------------------------------------------------
[messages-in]
exten => _.,1,MessageSend(sip:${EXTEN}@matrix-sip-bridge.example.com:5060,${MESSAGE(from)})
 same => n,Hangup()
```

```ini
; confbridge.conf — the caller leaves when livekit-sip does.
;
; The option is "marked", not "marked_user". app_confbridge refuses to load the
; WHOLE FILE on a single unrecognised key, so one wrong name here does not
; produce one broken profile: ConfBridge() disappears from the dialplan
; entirely, taking every other context that uses it with it. Check the Asterisk
; log after editing this file.
[matrix_marked](type=user)
marked=yes

[matrix_caller](type=user)
end_marked=yes
wait_marked=no
```

`default_bridge` above is the bridge profile from Asterisk's own
`confbridge.conf.sample`; if that sample was never installed, name a profile
that exists.

## Configuration

Everything site-specific is configuration. Run `matrix-sip-bridge -c config.yaml -e`
to write a config file, `-g` to generate the appservice registration. The
bridgev2 sections (`homeserver`, `appservice`, `database`, `bridge`, `logging`)
are documented by mautrix; the `network` section is this bridge's and is
commented in [pkg/connector/example-config.yaml](pkg/connector/example-config.yaml).

Two settings outside the `network` section are not optional. There is no
per-user SIP account, so every Matrix user shares one login and reaches it
through relay mode:

```yaml
bridge:
    relay:
        enabled: true
        default_relays: [sip]
```

Without them the bridge starts, logs a warning and silently drops every message
from anyone but the one user who logged in. If the far end is SMS, blank the
`relay.message_formats` templates too: they prefix each message with the
sender's name, which wastes a length-limited message.

### Double puppeting is required for calls

A portal invite the user has not accepted makes calls unreachable rather than
untidy: the RTC membership and the ring notification are events inside the
portal room, and a non-member cannot see them. The bridge therefore joins the user itself
before every call, which needs double puppeting:

```yaml
double_puppet:
    secrets:
        example.com: as_token:<the appservice as_token>
```

The `as_token:` prefix is part of the value, not a placeholder — without it
mautrix treats the whole string as a saved access token and disables double
puppeting with no error. The generated registration does not claim the local
users, so add the namespace by hand:

```yaml
namespaces:
    users:
        - regex: ^@.*:example\.com$
          exclusive: false
```

If no double puppet is available the bridge falls back to inviting and logs a
warning saying the invite has to be accepted by hand.

### Configuration from the environment

mxmain reads config values from the environment only if `env_config_prefix` is
set in the config file. It is `null` — disabled — by default, and there is no
built-in prefix.

With `env_config_prefix: MATRIX_SIP_BRIDGE_`, a variable name is that prefix,
then the config path with `__` between segments, uppercased. Paths under the
network section start with `NETWORK`:

| Config path | Environment variable |
| --- | --- |
| `network.sip.domain` | `MATRIX_SIP_BRIDGE_NETWORK__SIP__DOMAIN` |
| `network.sip.register.password` | `MATRIX_SIP_BRIDGE_NETWORK__SIP__REGISTER__PASSWORD` |
| `network.calls.livekit.api_secret` | `MATRIX_SIP_BRIDGE_NETWORK__CALLS__LIVEKIT__API_SECRET` |
| `appservice.as_token` | `MATRIX_SIP_BRIDGE_APPSERVICE__AS_TOKEN` |

Three things worth knowing before wiring up a Deployment:

- A `_FILE` suffix reads the value from the named file instead, which is how a
  Kubernetes secret is mounted: `..._API_SECRET_FILE=/secrets/livekit`.
- A variable that does not match a config field is a **startup error**, not a
  warning. A typo stops the bridge rather than being ignored silently.
- Only strings, booleans and numbers are supported, and a duration field reaches
  Go as an integer of **nanoseconds**. Set durations in the config file.

### Image tags

CI publishes to `ghcr.io/lhns/matrix-sip-bridge`. Every push to `main` produces
a `sha-<short commit>` tag; a git tag `vX.Y.Z` produces `X.Y.Z` and `X.Y`. A
deployment that pins `:0.1.0` before `v0.1.0` has been tagged will not find an
image.

## Project layout

```
main.go                    mxmain.BridgeMain: flags, config, -g registration
pkg/connector/             bridgev2 NetworkConnector and NetworkAPI (messaging)
  connector.go               lifecycle, static login, event-processor wiring
  client.go                  inbound and outbound SIP MESSAGE
  commands.go                !dial, for numbers with no portal room yet
  config.go                  network config struct and upgrader
  example-config.yaml        shipped as the base for config upgrades
pkg/siptransport/          the SIP user agent: sipgo, no media stack
  transport.go               listener, registration, request handlers
  call.go                    inbound control leg, outbound INVITE
  message.go                 SIP MESSAGE in and out
  sdp.go                     the answer that must not read as hold
pkg/calls/                 the call subsystem, independent of bridgev2 plumbing
  subsystem.go               call lifecycle, ring, answer, teardown watch
  membership.go              publishing the ghost RTC membership and the ring
  rtc.go                     MSC3401 call.member parsing
  notification.go            MSC4075 ring notification, MSC4310 decline
  identity.go                LiveKit room-name and participant-identity derivation
  livekit.go                 twirp JSON client for the LiveKit SIP and room APIs
  trunk.go                   outbound-trunk reconciliation (Redis loses it)
pkg/phonenum/              E.164 normalisation and portal-ID round trips
pkg/database/              db.Child() tables for call state, separate from bridgev2
docs/adr/                  architecture decision records
```

## Develop

```sh
make build    # go build with version stamping
make test     # go test ./... -count=1
make lint     # go vet plus golangci-lint
```

`-tags goolm` is required everywhere — build, vet, test, Dockerfile, CI —
because the default build wants libolm headers. CGO cannot be turned off either:
bridgev2's `mxmain` imports `mattn/go-sqlite3` unconditionally. The image builds
with CGO against musl and links statically, so it still runs on
`distroless/static`.

Tests are table-driven with the standard library and no framework. Most cover
mapping and parsing: E.164 normalisation, portal-ID round trips, SDP answers,
and the LiveKit hash derivations against the MSC4195 golden vectors.
`pkg/siptransport` additionally runs real SIP conversations against a second
sipgo user agent over a loopback TCP listener, because the mistakes that matter
there — a bodyless 200, an SDP that reads as hold, a MESSAGE that is not routed
— are all wire-level. Nothing talks to a live Asterisk, LiveKit or homeserver.

## Design decisions

See [docs/adr](docs/adr/README.md) for the numbered index. The ones worth
reading before changing anything: [0008](docs/adr/0008-the-bridge-is-a-sip-endpoint.md)
on why the bridge speaks SIP and what the control leg is,
[0010](docs/adr/0010-element-call-filters-audio-by-rtc-membership.md) on why a
call membership is mandatory and why encryption breaks calls silently,
[0012](docs/adr/0012-the-rtc-membership-must-be-usable-by-the-client.md) on
what has to be in that membership and which participant-identity scheme to
configure, and
[0005](docs/adr/0005-calls-bridged-as-media-via-livekit-sip.md) on the LiveKit
room-name derivation the bridge must reproduce exactly.

If a call connects and nobody can hear the caller, read 0012 first: both
participants in the same LiveKit room with packets flowing and zero loss is
what a mismatched participant identity looks like from the server side.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
