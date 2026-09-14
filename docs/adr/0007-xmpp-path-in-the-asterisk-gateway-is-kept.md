# ADR-0007: The XMPP path in the existing Asterisk gateway is kept

## Status

Accepted

## Context

The Asterisk this bridge talks to already gateways the same SIP network to XMPP.
The obvious tidy-up is to delete it once Matrix works.

## Decision

Leave it. This bridge is additive: it adds an AMI client, a dialplan contract
and a ConfBridge naming convention, and changes nothing about the existing
gateway.

## Consequences

- Two consumers observe the same Asterisk. The bridge must therefore ignore
  ConfBridge activity it does not own, which it does by requiring the configured
  `conference_prefix`.
- A number can be reachable from both Matrix and XMPP at once. Nothing
  arbitrates between them; a call answered on one side is simply in progress
  from the other's point of view.
- Rolling the bridge back is a deployment change, not a migration.
