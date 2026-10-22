# ADR-0018: Inbound texts are persisted before the SIP answer

## Status

Accepted

## Context

A SIP MESSAGE the bridge answers 200 is gone from the sender's point of view:
the SIP server has already acknowledged the carrier, so nothing redelivers it.
The bridge used to answer 200 as soon as bridgev2 had the event in its
in-memory portal buffer. A homeserver outage, or a restart with events still
buffered, lost texts with no trace.

## Decision

- Answering 200 means the text is stored. `handleInboundMessage` still refuses
  what can never succeed (unknown or forbidden recipient) and drops what is
  unusable (unparseable sender); everything else is persisted first.
- The text goes into the bridge's own table `sip_inbound_message` (the outbox),
  with a short insert timeout. If that fails, it is written to a spool
  directory (`messages.spool_dir`) as one JSON file, via temp file, fsync,
  atomic rename and directory fsync. If both fail, the handler returns an error
  and the SIP server sees 500. An empty `spool_dir` disables the spool.
- A drainer moves spool files into the outbox in file-name (receive) order, at
  startup and periodically. Each text carries a UID with a unique constraint,
  so a crash between insert and file removal cannot duplicate it.
- A delivery worker reads the oldest text of each portal and calls
  `QueueRemoteEvent` with the message ID `in-<uid>`. It deletes the row only
  when bridgev2 reports success; otherwise it retries with exponential backoff
  (1 s doubling to 1 min). The stable ID lets bridgev2's duplicate check absorb
  a retry of a text that did reach Matrix. Delivery is at-least-once to
  bridgev2 and exactly-once in Matrix.
- Order is kept per portal: only a portal's oldest text is in flight, so a
  failing portal is held back with its later texts behind it. Portals are
  delivered concurrently (at most 8), because the synchronous call can block
  for hours against an unreachable homeserver; a blocked portal then holds
  back only itself, and a portal with many waiting texts cannot crowd the
  others out of a batch. A text spooled during a database outage gets its row
  when the drainer runs, so it can land behind a text that arrived after the
  database came back; the Matrix timestamp is still the receive time.
- This needs bridgev2 to report real results, so the portal event buffer is
  switched off (`bridge.portal_event_buffer: 0`, mautrix-go 0.32), making
  `QueueRemoteEvent` synchronous. The bridge forces it to 0 at startup, and the
  worker treats a "queued" result as a failure, since a buffer is the loss this
  decision removes. `homeserver.retry_limit` is raised so a
  homeserver outage is waited out inside the call, and
  `MAUTRIX_EVENT_HANDLING_TIMEOUT_TICKS` lets bridgev2 wait as long before it
  assumes success.

## Consequences

- A Synapse outage, a bridge crash, or a database outage no longer loses
  inbound texts, provided the spool is on persistent storage.
- The spool needs a small persistent volume; on an emptyDir it protects
  against a database outage but not a pod restart during one.
- Texts reach Matrix a little later than before (one database round trip and a
  worker wake-up) and, after an outage, in a burst.
- A text whose login or portal can never be handled stays in the outbox and
  holds back only its own portal; `sip_bridge_inbound_outbox_depth` shows it.
