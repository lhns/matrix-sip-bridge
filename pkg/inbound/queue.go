// Package inbound makes the bridge's answer to an inbound text mean something.
//
// SIP cannot redeliver a MESSAGE the bridge has answered 200, so a text is
// persisted before that answer: into the sip_inbound_message table, or, if the
// database is unavailable, into a spool directory on local disk. A drainer
// moves spool files into the table and a worker hands table rows to bridgev2,
// deleting a row only once bridgev2 reports success. See ADR-0018.
package inbound

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// Store is the outbox table.
type Store interface {
	Insert(ctx context.Context, m *database.InboundMessage) (bool, error)
	Pending(ctx context.Context, limit int) ([]*database.InboundMessage, error)
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
	// held is the per-portal retry state; only the delivery loop touches it.
	held map[string]*portalState
}

type portalState struct {
	failures int
	retryAt  time.Time
}

// New builds a Queue. An empty spoolDir disables the spool.
func New(store Store, spoolDir string, log zerolog.Logger, m *metrics.Recorder) (*Queue, error) {
	q := &Queue{
		store: store, log: log, metrics: m,
		insertTimeout: defaultInsertTimeout, spoolInterval: defaultSpoolInterval, pollInterval: defaultPollInterval,
		backoffMin: defaultBackoffMin, backoffMax: defaultBackoffMax,
		now:  time.Now,
		wake: make(chan struct{}, 1),
		held: map[string]*portalState{},
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
			}
			q.setSpool(q.spoolDepth.Add(-1))
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

// DeliverPass makes one pass over the table and returns how long to wait
// before the next: zero means go again now, negative means nothing is
// scheduled and the next wake or poll is enough.
//
// Order is kept per portal. A portal whose message fails is held back with
// exponential backoff and its later messages are skipped, while other portals
// carry on, so one stuck room does not stop the rest.
func (q *Queue) DeliverPass(ctx context.Context, deliver DeliverFunc) time.Duration {
	rows, err := q.store.Pending(ctx, batchSize)
	if err != nil {
		q.log.Warn().Err(err).Msg("Failed to read the inbound outbox")
		return q.backoffMin
	}
	if len(rows) < batchSize {
		q.setOutbox(int64(len(rows)))
	}
	blocked := map[string]bool{}
	var soonest time.Time
	note := func(t time.Time) {
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return -1
		}
		key := row.LoginID + "\x00" + row.PortalID
		if blocked[key] {
			continue
		}
		st := q.held[key]
		if st != nil && q.now().Before(st.retryAt) {
			blocked[key] = true
			note(st.retryAt)
			continue
		}
		err := deliver(ctx, row)
		if err == nil {
			if err = q.store.Delete(ctx, row.ID); err == nil {
				delete(q.held, key)
				q.setOutbox(q.outboxDepth.Add(-1))
				continue
			}
			err = fmt.Errorf("delete delivered message: %w", err)
		}
		if st == nil {
			st = &portalState{}
			q.held[key] = st
		}
		st.failures++
		st.retryAt = q.now().Add(q.backoff(st.failures))
		blocked[key] = true
		note(st.retryAt)
		q.log.Warn().Err(err).Int64("outbox_id", row.ID).Int("failures", st.failures).
			Msg("Failed to deliver inbound message, will retry")
	}
	for key := range q.held {
		if !blocked[key] {
			delete(q.held, key)
		}
	}
	if !soonest.IsZero() {
		return max(soonest.Sub(q.now()), 0)
	}
	if len(rows) == batchSize {
		return 0
	}
	return -1
}

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
