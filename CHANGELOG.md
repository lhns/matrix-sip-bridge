# Changelog

All notable changes to this project are documented here. The format is based on
Keep a Changelog, and this project adheres to
Semantic Versioning.

## [Unreleased]

## [0.2.3] - 2026-10-09
### Added
- Dial room: `call <number>` (also `!call`, `dial`, `!dial`) now works there without the bridge's command prefix, and calls on that room's line. A line may be given last, as with the command (`call +15551234567 mobile`).

### Changed
- The `!dial` command is now `!call`; `!dial` still works as an alias.
- The dial room's reply to a number, its "not a usable number" reply and its topic now say how to call the number: `` `!call <number>` ``.

## [0.2.2] - 2026-10-09
### Fixed
- A room whose line space, or link to it, failed to be created is now retried on its next resync instead of staying unlinked.

### Changed
- At startup the bridge logs how many calls it closed that were left open by the previous run.
- Otherwise internal cleanup; no other behavior change.

## [0.2.1] - 2026-10-09
### Added
- `network.line_members`: per line, the Matrix users who get that line's rooms as soon as the bridge starts, instead of with their first call or text. Each gets a login, the line's space, and a `<line> ☎` dial room. Users `bridge.permissions` does not let log in are skipped with a warning; removing a user deletes nothing. See [ADR-0017](https://github.com/lhns/matrix-sip-bridge/blob/main/docs/adr/0017-line-members-and-dial-rooms.md).
- Dial room: send a number there and the bridge opens that user's room for it on the line and replies with a link; `!dial <number>` there calls on that line.

### Fixed
- After the 0.2.0 move, a user's rooms could be placed in a shared copy of their line space instead of their own. The bridge now keeps each user's rooms under their own space.

**Upgrading:** nothing to do. At startup the bridge repairs a shared line space whose rooms all belong to one user by moving it to that user; the Matrix room itself is untouched. If you are on 0.2.0, upgrade to this version.

## [0.2.0] - 2026-10-09
### Added
- Every Matrix user has their own login and their own rooms. The bridge creates a user's login the first time a call or text names them, if `bridge.permissions` lets them log in (level `user` or higher). See [ADR-0016](https://github.com/lhns/matrix-sip-bridge/blob/main/docs/adr/0016-per-user-logins-and-portals.md).
- `sip.recipient_header` (default `X-Matrix-Recipient`): the header on an inbound INVITE or MESSAGE that names the Matrix user it is for. A user without `login` permission is refused with 403, a value that is not an MXID with 404. With no header, the call or text goes to the default recipient, as before.
- Calls ring each recipient in their own room: send one INVITE leg per recipient, all with the same `X-Conference`. The first to join wins and the bridge answers only that leg; the others see `Answered by <name>`. Declining rejects only that leg (486).
- `network.line_spaces` (default `true`) switches the per-line Matrix spaces off for new rooms. It does not remove existing spaces.
- New metric outcomes `answered_elsewhere` and `refused_recipient`.

### Changed
- Outbound texts: the `From` is `messages.outbound_from` with its host replaced by the portal's line (`sip:+15551234567@home`), and the display name is the sending user's MXID.
- The bridge now sends 200 for a call only once livekit-sip has joined the conference, so a losing leg never brings it in.
- Relay mode is no longer used.

### Removed
- The `dropped_no_login` metric outcome, which nothing emitted any more.

**Upgrading:**
- On first start, before the SIP endpoint listens, the bridge moves every room of the old shared `sip` login to that login's owner (same rooms, history and spaces), makes that owner the default recipient, and removes the old login without touching any room. It is idempotent. If the owner already has a room for the same number, that one room is skipped with a warning and the old login is kept until you resolve it. See "Upgrading from a shared login" in the README.
- Update your SIP server to send one INVITE leg and one MESSAGE per recipient with the recipient header; the README has a worked Asterisk example.
- `bridge.relay` and `default_relays: [sip]` can be removed from your config.
- Known issue, fixed in 0.2.1: the first resync after the move could leave a user's rooms in a second, shared copy of their line space. Go straight to 0.2.1.

## [0.1.0] - 2026-09-17
First tagged release of the Matrix <-> SIP bridge.

### Added
- Calls in both directions. An inbound call rings Matrix alongside your other phones; a Matrix user places one from the Element Call button or `!sip dial`. Media runs through LiveKit, with livekit-sip joining the SIP conference as a participant.
- SMS in both directions over SIP MESSAGE.
- One conversation per person: a portal room is `<line>-<number>`, so a call and a text from the same person on the same line share a room, and each line gets its own Matrix space.
- Outbound permission lives on the SIP server. The bridge names the caller on the wire (`X-Matrix-Caller` on an INVITE, the From display name on a MESSAGE) and your dialplan decides.
- `callaudit`, shipped in the same image, verifies from livekit-sip's call statistics that a call carried audio.

**Requirements:** a SIP server that routes a conference name into a conference and echoes its registered Contact on inbound requests; LiveKit plus livekit-sip with a SIP trunk reachable from both; double puppeting (calls and the per-line spaces depend on it).

**Known limits:**
- Declining an outbound call notification ends it; declining an inbound one is handled separately.
- A space cannot be renamed by a user at the default power level.
- An inbound SMS cannot tell two DIDs apart on a trunk that does not echo the bridge's Contact; it falls back to that host's default line and says so in the log.

[Unreleased]: https://github.com/lhns/matrix-sip-bridge/compare/v0.2.3...HEAD
[0.2.3]: https://github.com/lhns/matrix-sip-bridge/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/lhns/matrix-sip-bridge/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/lhns/matrix-sip-bridge/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/lhns/matrix-sip-bridge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/lhns/matrix-sip-bridge/releases/tag/v0.1.0
