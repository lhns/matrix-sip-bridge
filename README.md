# matrix-sip-bridge

[![CI](https://github.com/lhns/matrix-sip-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/lhns/matrix-sip-bridge/actions/workflows/ci.yml)

A Matrix bridge to a SIP/telephony network reached through Asterisk. Text
messages and phone calls both land in a portal room per phone number: an inbound
SIP MESSAGE appears as an `m.room.message` from a per-number ghost, and a Matrix
message in that room goes back out as a SIP MESSAGE. A phone call is bridged as
real media, not as a notification — Asterisk parks the far end in a ConfBridge
and `livekit-sip` dials into it to join the LiveKit room behind the room's
Element Call, so the call rings and is answered in Matrix.

The two halves share one binary and one AMI connection but almost no code. The
messaging half is a [bridgev2](https://mau.fi/blog/megabridge-twilio/) network
connector, forked from the mautrix-twilio skeleton. The call half is its own
subsystem, because bridgev2 has no MSC3401 or LiveKit support and does not
deliver `org.matrix.msc3401.call.member` to network connectors at all; it
borrows bridgev2's ghosts and portals as identity and room substrate and none of
its event plumbing. See [ADR-0001](docs/adr/0001-bridgev2-for-messaging-bespoke-subsystem-for-calls.md)
and [ADR-0006](docs/adr/0006-observing-inbound-rtc-membership.md).

## Configuration

Everything site-specific is configuration. Run `matrix-sip-bridge -c config.yaml -e`
to write a config file, `-g` to generate the appservice registration. The
bridgev2 sections (`homeserver`, `appservice`, `database`, `bridge`, `logging`)
are documented by mautrix; the `network` section is this bridge's.

Any value can also come from the environment, which is how the secrets are best
supplied. Set `env_config_prefix` in the config file (it is `null`, meaning
disabled, by default); with `env_config_prefix: SIPBRIDGE_`, a config path
becomes that prefix plus the path uppercased with `__` between segments. A
`_FILE` suffix reads the value from the named file instead, for Kubernetes
secrets.

| YAML (under `network:`) | Environment variable | What it is |
| --- | --- | --- |
| `asterisk.address` | `SIPBRIDGE_NETWORK__ASTERISK__ADDRESS` | `host:port` of the AMI listener |
| `asterisk.username` | `SIPBRIDGE_NETWORK__ASTERISK__USERNAME` | AMI user; needs the `call`, `message`, `originate` and `system` classes |
| `asterisk.secret` | `SIPBRIDGE_NETWORK__ASTERISK__SECRET` | AMI password |
| `asterisk.tls` | `SIPBRIDGE_NETWORK__ASTERISK__TLS` | AMI sends the secret in the clear; enable unless it is a private network |
| `messages.enabled` | `SIPBRIDGE_NETWORK__MESSAGES__ENABLED` | Turn the messaging half on |
| `messages.user_event` | `SIPBRIDGE_NETWORK__MESSAGES__USER_EVENT` | Name of the dialplan `UserEvent` carrying an inbound message |
| `messages.outbound_to` | `SIPBRIDGE_NETWORK__MESSAGES__OUTBOUND_TO` | Message URI template; `{number}` is the E.164 destination |
| `messages.outbound_from` | `SIPBRIDGE_NETWORK__MESSAGES__OUTBOUND_FROM` | `From` URI on outbound messages |
| `messages.max_length` | `SIPBRIDGE_NETWORK__MESSAGES__MAX_LENGTH` | Advertised to clients as the room's text limit |
| `calls.enabled` | `SIPBRIDGE_NETWORK__CALLS__ENABLED` | Turn the call half on |
| `calls.membership_expiry` | `SIPBRIDGE_NETWORK__CALLS__MEMBERSHIP_EXPIRY` | Lifetime of the caller ghost's RTC membership |
| `calls.ring_timeout` | `SIPBRIDGE_NETWORK__CALLS__RING_TIMEOUT` | How long an unanswered inbound call is held before hangup |
| `calls.livekit.url` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__URL` | LiveKit server base URL |
| `calls.livekit.api_key` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__API_KEY` | LiveKit API key |
| `calls.livekit.api_secret` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__API_SECRET` | LiveKit API secret |
| `calls.livekit.trunk_name` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__TRUNK_NAME` | Name of the outbound trunk the bridge reconciles |
| `calls.livekit.trunk_address` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__TRUNK_ADDRESS` | SIP host livekit-sip dials to reach Asterisk |
| `calls.livekit.trunk_number` | `SIPBRIDGE_NETWORK__CALLS__LIVEKIT__TRUNK_NUMBER` | Caller number livekit-sip presents |
| `calls.livekit.trunk_auth_username` | `..._LIVEKIT__TRUNK_AUTH_USERNAME` | Optional SIP digest user for the trunk |
| `calls.livekit.trunk_auth_password` | `..._LIVEKIT__TRUNK_AUTH_PASSWORD` | Optional SIP digest password |
| `calls.livekit.trunk_reconcile_interval` | `..._LIVEKIT__TRUNK_RECONCILE_INTERVAL` | How often the trunk is re-checked; it lives in Redis and is lost on restart |
| `calls.asterisk.conference_prefix` | `..._CALLS__ASTERISK__CONFERENCE_PREFIX` | Prefix naming the ConfBridge for a number |
| `calls.asterisk.outbound_channel` | `..._CALLS__ASTERISK__OUTBOUND_CHANNEL` | Channel used to place a call; `{number}` is the destination |
| `calls.asterisk.context` / `.extension` | `..._CALLS__ASTERISK__CONTEXT` / `__EXTENSION` | Where an originated call lands in the dialplan |
| `calls.asterisk.caller_id` | `..._CALLS__ASTERISK__CALLER_ID` | Caller ID presented on outbound calls |

Two settings outside the `network` section are not optional. There is no
per-user SIP account, so every Matrix user shares one login and reaches it
through relay mode:

```yaml
bridge:
    relay:
        enabled: true
        default_relays: [asterisk]
```

Without them the bridge starts, logs a warning and silently drops every message
from anyone but the one user who logged in. If the far end is SMS, blank the
`relay.message_formats` templates too: they prefix each message with the sender's
name, which wastes a length-limited message.

The dialplan is part of the contract. It must fire the configured `UserEvent`
with `From`, `To` and `Body` headers for an inbound message, and route calls
into `ConfBridge(${CONFBRIDGE_NAME})`.

## Project layout

```
main.go                    mxmain.BridgeMain: flags, config, -g registration
pkg/connector/             bridgev2 NetworkConnector and NetworkAPI (messaging)
  connector.go               lifecycle, static login, event-processor wiring
  client.go                  inbound and outbound SIP MESSAGE
  commands.go                !dial, for numbers with no portal room yet
  config.go                  network config struct and upgrader
  example-config.yaml        shipped as the base for config upgrades
pkg/calls/                 the call subsystem, independent of bridgev2 plumbing
  subsystem.go               ConfBridge watch, ring, dial, call lifecycle
  rtc.go                     MSC3401 call.member parsing and publishing
  identity.go                MSC4195 LiveKit room-name and identity derivation
  livekit.go                 twirp JSON client for the LiveKit SIP service
  trunk.go                   outbound-trunk reconciliation (Redis loses it)
pkg/asteriskami/           AMI client: packet codec, event stream, actions
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

Tests are table-driven with the standard library and no framework, and cover the
mapping and parsing logic only: E.164 normalisation, AMI packet parsing and
escaping, portal-ID round trips, and the LiveKit hash derivations against the
MSC4195 golden vectors. Nothing talks to a live Asterisk, LiveKit or homeserver.

Note that the binary needs CGO: bridgev2's `mxmain` imports `mattn/go-sqlite3`
unconditionally, so `CGO_ENABLED=0` does not compile. The image builds with CGO
against musl and links statically, so it still runs on `distroless/static`.

## Design decisions

See [docs/adr](docs/adr/README.md) for the numbered index. The ones worth
reading before changing anything: [0001](docs/adr/0001-bridgev2-for-messaging-bespoke-subsystem-for-calls.md)
on why calls are not a bridgev2 concept, [0005](docs/adr/0005-calls-bridged-as-media-via-livekit-sip.md)
on the LiveKit room-name derivation the bridge must reproduce exactly, and
[0006](docs/adr/0006-observing-inbound-rtc-membership.md) on how a Matrix user
pressing the call button is observed at all.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
