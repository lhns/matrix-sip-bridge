// Package inbound makes the bridge's answer to an inbound text mean something.
//
// SIP cannot redeliver a MESSAGE the bridge has answered 200, so a text is
// persisted before that answer: into the sip_inbound_message table, or, if the
// database is unavailable, into a spool directory on local disk. A drainer
// moves spool files into the table and a worker hands table rows to bridgev2,
// one portal at a time, deleting a row only once bridgev2 reports success.
// See ADR-0018.
package inbound

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// Store is the outbox table.
type Store interface {
	Insert(ctx context.Context, m *database.InboundMessage) (bool, error)
	Heads(ctx context.Context, limit int) ([]*database.InboundMessage, error)
	Delete(ctx context.Context, id int64) error
	Count(ctx context.Context) (int, error)
}

// DeliverFunc hands one message to Matrix. It returns nil only when the
// message is in Matrix, or bridgev2 recognised it as already there.
type DeliverFunc func(ctx context.Context, m *database.InboundMessage) error

// Timing defaults. They are Queue fields so tests can shorten them.
const (
	defaultInsertTimeout = 3 * time.Second
	defaultSpoolInterval = 10 * time.Second
	defaultPollInterval  = 30 * time.Second
	defaultBackoffMin    = time.Second
	defaultBackoffMax    = time.Minute
	batchSize            = 500
	maxParallel          = 8
)

// Queue is the durable path from SIP to bridgev2.
type Queue struct {
	store   Store
	spool   *Spool // nil: spool disabled
	log     zerolog.Logger
	metrics *metrics.Recorder

	insertTimeout time.Duration
	spoolInterval time.Duration
	pollInterval  time.Duration
	backoffMin    time.Duration
	backoffMax    time.Duration
	now           func() time.Time

	wake        chan struct{}
	outboxDepth atomic.Int64
	spoolDepth  atomic.Int64
	// portals is the per-portal delivery state, guarded by mu.
	mu      sync.Mutex
	portals map[string]*portalState
	sem     chan struct{} // bounds concurrent deliveries
	wg      sync.WaitGroup
}

type portalState struct {
	busy     bool // a delivery is in flight
	failures int
	retryAt  time.Time
}

// New builds a Queue. An empty spoolDir disables the spool.
func New(store Store, spoolDir string, log zerolog.Logger, m *metrics.Recorder) (*Queue, error) {
	q := &Queue{
		store: store, log: log, metrics: m,
		insertTimeout: defaultInsertTimeout, spoolInterval: defaultSpoolInterval, pollInterval: defaultPollInterval,
		backoffMin: defaultBackoffMin, backoffMax: defaultBackoffMax,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		portals: map[string]*portalState{},
		sem:     make(chan struct{}, maxParallel),
	}
	if spoolDir != "" {
		s, err := NewSpool(spoolDir)
		if err != nil {
			return nil, err
		}
		q.spool = s
	}
	return q, nil
}

// Persist makes m survive a crash and returns nil once it does; that is the
// condition for answering SIP 200. spooled reports that the database was
// unavailable and the message went to the spool instead.
func (q *Queue) Persist(ctx context.Context, m *database.InboundMessage) (spooled bool, err error) {
	ictx, cancel := context.WithTimeout(ctx, q.insertTimeout)
	defer cancel()
	inserted, dbErr := q.store.Insert(ictx, m)
	if dbErr == nil {
		if inserted {
			q.setOutbox(q.outboxDepth.Add(1))
		}
		q.notify()
		return false, nil
	}
	if q.spool == nil {
		return false, fmt.Errorf("store inbound message: %w", dbErr)
	}
	q.log.Warn().Err(dbErr).Msg("Database unavailable, spooling inbound message")
	if spErr := q.spool.Write(m); spErr != nil {
		return false, fmt.Errorf("store inbound message: %w", errors.Join(dbErr, fmt.Errorf("spool: %w", spErr)))
	}
	q.setSpool(q.spoolDepth.Add(1))
	return true, nil
}

// Drain moves spool files into the table, oldest first, and stops at the
// first database error. A file is removed only after its row exists, and the
// unique UID makes a crash between the two harmless.
func (q *Queue) Drain(ctx context.Context) error {
	if q.spool == nil {
		return nil
	}
	names, err := q.spool.List()
	if err != nil {
		return fmt.Errorf("list spool: %w", err)
	}
	q.setSpool(int64(len(names)))
	for _, name := range names {
		m, err := q.spool.Read(name)
		if err != nil {
			q.log.Error().Err(err).Str("file", name).Msg("Unreadable spool file, setting it aside")
			if qErr := q.spool.Quarantine(name); qErr != nil {
				q.log.Error().Err(qErr).Str("file", name).Msg("Failed to set the spool file aside")
			} else {
				q.setSpool(q.spoolDepth.Add(-1))
			}
			continue
		}
		ictx, cancel := context.WithTimeout(ctx, q.insertTimeout)
		inserted, err := q.store.Insert(ictx, m)
		cancel()
		if err != nil {
			return fmt.Errorf("move spooled message: %w", err)
		}
		if inserted {
			q.setOutbox(q.outboxDepth.Add(1))
		}
		if err := q.spool.Remove(name); err != nil {
			// The next pass finds the row already there and removes it then.
			q.log.Warn().Err(err).Str("file", name).Msg("Failed to remove a drained spool file")
			continue
		}
		q.setSpool(q.spoolDepth.Add(-1))
		q.notify()
	}
	return nil
}

// DeliverPass starts a delivery for the oldest waiting message of every portal
// that is idle and not backing off, then returns without waiting for them, and
// reports how long until the next backoff ends: negative means nothing is
// scheduled and the next wake or poll is enough. Each delivery wakes the loop
// when it ends.
//
// Order is kept per portal because only a portal's oldest message is ever in
// flight. A QueueRemoteEvent can block for hours against an unreachable
// homeserver, so deliveries run concurrently (at most maxParallel) and a stuck
// portal holds back only itself. A portal whose message fails backs off
// exponentially.
func (q *Queue) DeliverPass(ctx context.Context, deliver DeliverFunc) time.Duration {
	heads, err := q.store.Heads(ctx, batchSize)
	if err != nil {
		q.log.Warn().Err(err).Msg("Failed to read the inbound outbox")
		return q.backoffMin
	}
	if n, err := q.store.Count(ctx); err == nil {
		q.setOutbox(int64(n))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := map[string]bool{}
	var soonest time.Time
spawn:
	for _, row := range heads {
		if ctx.Err() != nil {
			return -1
		}
		key := row.LoginID + "\x00" + row.PortalID
		seen[key] = true
		st := q.portals[key]
		if st == nil {
			st = &portalState{}
			q.portals[key] = st
		}
		if st.busy {
			continue
		}
		if q.now().Before(st.retryAt) {
			if soonest.IsZero() || st.retryAt.Before(soonest) {
				soonest = st.retryAt
			}
			continue
		}
		select {
		case q.sem <- struct{}{}:
		default:
			break spawn // a finishing delivery wakes the loop
		}
		st.busy = true
		q.wg.Add(1)
		go q.deliverOne(ctx, deliver, key, row)
	}
	for key, st := range q.portals {
		if !seen[key] && !st.busy {
			delete(q.portals, key)
		}
	}
	if soonest.IsZero() {
		return -1
	}
	return max(soonest.Sub(q.now()), 0)
}

// deliverOne hands one message over and deletes its row on success.
func (q *Queue) deliverOne(ctx context.Context, deliver DeliverFunc, key string, row *database.InboundMessage) {
	defer q.wg.Done()
	defer func() { <-q.sem }()
	defer q.notify()
	err := deliver(ctx, row)
	if err == nil {
		if err = q.store.Delete(ctx, row.ID); err == nil {
			q.setOutbox(q.outboxDepth.Add(-1))
			q.mu.Lock()
			delete(q.portals, key)
			q.mu.Unlock()
			return
		}
		err = fmt.Errorf("delete delivered message: %w", err)
	}
	q.mu.Lock()
	st := q.portals[key]
	st.busy = false
	if ctx.Err() == nil {
		st.failures++
		st.retryAt = q.now().Add(q.backoff(st.failures))
	}
	failures := st.failures
	q.mu.Unlock()
	if ctx.Err() == nil {
		q.log.Warn().Err(err).Int64("outbox_id", row.ID).Int("failures", failures).
			Msg("Failed to deliver inbound message, will retry")
	}
}

// Wait blocks until the deliveries DeliverPass started have ended; for tests.
func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) backoff(failures int) time.Duration {
	d := q.backoffMin
	for i := 1; i < failures && d < q.backoffMax; i++ {
		d *= 2
	}
	return min(d, q.backoffMax)
}

// Run drains the spool and delivers the table until ctx ends.
func (q *Queue) Run(ctx context.Context, deliver DeliverFunc) {
	if n, err := q.store.Count(ctx); err == nil {
		q.setOutbox(int64(n))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.drainLoop(ctx)
	}()
	q.deliverLoop(ctx, deliver)
	<-done
}

func (q *Queue) drainLoop(ctx context.Context) {
	t := time.NewTicker(q.spoolInterval)
	defer t.Stop()
	for {
		if err := q.Drain(ctx); err != nil && ctx.Err() == nil {
			q.log.Warn().Err(err).Msg("Failed to drain the inbound spool")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (q *Queue) deliverLoop(ctx context.Context, deliver DeliverFunc) {
	for {
		wait := q.DeliverPass(ctx, deliver)
		if wait < 0 || wait > q.pollInterval {
			wait = q.pollInterval
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-q.wake:
			case <-timer.C:
			}
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) setOutbox(n int64) {
	n = max(n, 0)
	q.outboxDepth.Store(n)
	q.metrics.InboundOutboxDepth(int(n))
}

func (q *Queue) setSpool(n int64) {
	n = max(n, 0)
	q.spoolDepth.Store(n)
	q.metrics.InboundSpoolDepth(int(n))
}

// SetBackoff replaces the retry backoff bounds; for tests of callers that
// cannot wait out the real ones.
func (q *Queue) SetBackoff(minWait, maxWait time.Duration) {
	q.backoffMin, q.backoffMax = minWait, maxWait
}
