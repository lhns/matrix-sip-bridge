# ADR-0009: The SIP server is configured for the bridge

## Status

Accepted. Supersedes [ADR-0007](0007-xmpp-path-in-the-asterisk-gateway-is-kept.md).

## Context

ADR-0007 said the bridge was purely additive: an AMI login, a dialplan
`UserEvent` and a ConfBridge naming convention, changing nothing about the
gateway it observed. That premise is gone. A SIP endpoint has to be dialled,
which means the shared gateway now has a peer, a route and a gosub that exist
for this bridge.

The XMPP path on the same gateway is still not being replaced — that part of
ADR-0007 stands.

## Decision

Accept that the bridge changes the SIP server's configuration, and write the
required configuration down as a spec rather than discovering it by trial. The
README's "SIP server contract" section is authoritative: the peer definition,
the header names, the response codes, the gosub, and the message routing.

## Consequences

- Rolling the bridge back is now two changes, not one: the deployment and the
  peer plus routing on the SIP server.
- Both consumers still observe the same numbers. A call can ring Matrix and
  XMPP at once, and nothing arbitrates; whichever answers first takes it,
  because `Dial()` cancels the other branches.
- The bridge still ignores what it does not own: an INVITE whose conference
  header does not carry the configured prefix is declined 404 rather than
  turned into a portal room.
- The two sides of this interface are written by different people at different
  times, which has already produced three mismatches on this project. A written
  contract with a worked dialplan example is the mitigation.
