package inbound

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// memStore is an in-memory outbox whose availability the test controls.
type memStore struct {
	mu     sync.Mutex
	down   bool
	nextID int64
	rows   []*database.InboundMessage
}

var errDown = errors.New("database is down")

func (s *memStore) Insert(_ context.Context, m *database.InboundMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return false, errDown
	}
	for _, r := range s.rows {
		if r.UID == m.UID {
			return false, nil
		}
	}
	s.nextID++
	c := *m
	c.ID = s.nextID
	s.rows = append(s.rows, &c)
	return true, nil
}

func (s *memStore) Heads(_ context.Context, limit int) ([]*database.InboundMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errDown
	}
	before := func(a, b *database.InboundMessage) bool {
		return a.ReceivedAt.Before(b.ReceivedAt) || a.ReceivedAt.Equal(b.ReceivedAt) && a.ID < b.ID
	}
	heads := map[string]*database.InboundMessage{}
	for _, r := range s.rows {
		if key := r.LoginID + r.PortalID; heads[key] == nil || before(r, heads[key]) {
			heads[key] = r
		}
	}
	var out []*database.InboundMessage
	for _, r := range heads {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *database.InboundMessage) int {
		if before(a, b) {
			return -1
		}
		return 1
	})
	return out[:min(limit, len(out))], nil
}

func (s *memStore) Delete(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errDown
	}
	for i, r := range s.rows {
		if r.ID == id {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			break
		}
	}
	return nil
}

func (s *memStore) Count(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows), nil
}

func (s *memStore) setDown(d bool) {
	s.mu.Lock()
	s.down = d
	s.mu.Unlock()
}

func (s *memStore) uids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.rows {
		out = append(out, r.UID)
	}
	return out
}

func msg(uid, portal string, at int) *database.InboundMessage {
	return &database.InboundMessage{
		UID: uid, LoginID: "@alice:example.com", PortalID: portal, Sender: "+15551234567",
		Body: "hi " + uid, ReceivedAt: time.Unix(1_700_000_000, int64(at)),
	}
}

func newQueue(t *testing.T, store Store, spoolDir string) *Queue {
	t.Helper()
	q, err := New(store, spoolDir, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := (&Spool{dir: dir}).List()
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestPersistGoesToTheDatabaseWhenItIsUp(t *testing.T) {
	store, dir := &memStore{}, t.TempDir()
	q := newQueue(t, store, dir)
	spooled, err := q.Persist(context.Background(), msg("a", "p", 1))
	if err != nil || spooled {
		t.Fatalf("Persist = spooled %v, %v; want stored in the database", spooled, err)
	}
	if got := store.uids(); len(got) != 1 || got[0] != "a" {
		t.Errorf("rows = %v, want [a]", got)
	}
	if n := len(spoolFiles(t, dir)); n != 0 {
		t.Errorf("%d spool files, want none", n)
	}
}

func TestPersistSpoolsWhenTheDatabaseIsDown(t *testing.T) {
	store, dir := &memStore{down: true}, t.TempDir()
	q := newQueue(t, store, dir)
	spooled, err := q.Persist(context.Background(), msg("a", "p", 1))
	if err != nil || !spooled {
		t.Fatalf("Persist = spooled %v, %v; want spooled", spooled, err)
	}
	files := spoolFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("spool files = %v, want one", files)
	}
	got, err := (&Spool{dir: dir}).Read(files[0])
	if err != nil || got.UID != "a" || got.Body != "hi a" || !got.ReceivedAt.Equal(msg("a", "p", 1).ReceivedAt) {
		t.Errorf("spool file read back as %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("spool dir has %d entries, want just the message (no temp file left)", len(entries))
	}
}

func TestPersistFailsWhenNothingTakesTheMessage(t *testing.T) {
	t.Run("database down and spool broken", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spool")
		q := newQueue(t, &memStore{down: true}, dir)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Persist(context.Background(), msg("a", "p", 1)); err == nil {
			t.Fatal("Persist succeeded with the database down and the spool unwritable")
		}
	})
	t.Run("database down and no spool", func(t *testing.T) {
		q := newQueue(t, &memStore{down: true}, "")
		if _, err := q.Persist(context.Background(), msg("a", "p", 1)); err == nil {
			t.Fatal("Persist succeeded with the database down and no spool configured")
		}
	})
}

func TestSpoolListsInReceiveOrder(t *testing.T) {
	dir := t.TempDir()
	q := newQueue(t, &memStore{down: true}, dir)
	// Persisted out of order on purpose: the name sorts by receive time.
	for _, m := range []*database.InboundMessage{msg("late", "p", 30), msg("early", "p", 10), msg("mid", "p", 20)} {
		if _, err := q.Persist(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	var uids []string
	for _, f := range spoolFiles(t, dir) {
		m, err := q.spool.Read(f)
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, m.UID)
	}
	if strings.Join(uids, ",") != "early,mid,late" {
		t.Errorf("order = %v, want early,mid,late", uids)
	}
}

func TestDrainMovesTheSpoolIntoTheDatabaseInOrder(t *testing.T) {
	store, dir := &memStore{down: true}, t.TempDir()
	q := newQueue(t, store, dir)
	for i, uid := range []string{"a", "b", "c"} {
		if _, err := q.Persist(context.Background(), msg(uid, "p", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Drain(context.Background()); err == nil {
		t.Fatal("Drain succeeded with the database down")
	}
	if n := len(spoolFiles(t, dir)); n != 3 {
		t.Fatalf("%d spool files after a failed drain, want all 3 kept", n)
	}
	store.setDown(false)
	if err := q.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(store.uids(), ","); got != "a,b,c" {
		t.Errorf("rows = %s, want a,b,c", got)
	}
	if n := len(spoolFiles(t, dir)); n != 0 {
		t.Errorf("%d spool files left after draining", n)
	}
}

// A crash between the insert and the file removal leaves both; the next pass
// must not duplicate the row.
func TestDrainIsIdempotentAfterACrashBetweenInsertAndRemove(t *testing.T) {
	store, dir := &memStore{}, t.TempDir()
	q := newQueue(t, store, dir)
	m := msg("a", "p", 1)
	if err := q.spool.Write(m); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := q.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.uids(); len(got) != 1 {
		t.Errorf("rows = %v, want exactly one", got)
	}
	if n := len(spoolFiles(t, dir)); n != 0 {
		t.Errorf("%d spool files left, want none", n)
	}
}

func TestDrainSetsAsideAnUnreadableFile(t *testing.T) {
	store, dir := &memStore{}, t.TempDir()
	q := newQueue(t, store, dir)
	if err := os.WriteFile(filepath.Join(dir, "0001-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001-bad.json.bad")); err != nil {
		t.Errorf("bad file was not set aside: %v", err)
	}
	if n := len(spoolFiles(t, dir)); n != 0 {
		t.Errorf("%d spool files still listed, want none", n)
	}
}

// fakeDeliver records deliveries and fails for the portals the test lists.
type fakeDeliver struct {
	mu        sync.Mutex
	failing   map[string]bool
	delivered []string
	attempts  []string
}

func (f *fakeDeliver) fn(_ context.Context, m *database.InboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, m.UID)
	if f.failing[m.PortalID] {
		return errors.New("matrix is unreachable")
	}
	f.delivered = append(f.delivered, m.UID)
	return nil
}

// settle runs passes until the outbox stops changing or only backoffs remain.
func settle(q *Queue, deliver DeliverFunc) {
	for range 20 {
		q.DeliverPass(context.Background(), deliver)
		q.Wait()
	}
}

func TestWorkerDeletesRowsOnlyOnSuccess(t *testing.T) {
	store := &memStore{}
	q := newQueue(t, store, "")
	now := time.Unix(1_800_000_000, 0)
	q.now = func() time.Time { return now }
	for i, uid := range []string{"a", "b"} {
		if _, err := q.Persist(context.Background(), msg(uid, "p", i)); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeDeliver{failing: map[string]bool{"p": true}}

	q.DeliverPass(context.Background(), f.fn)
	q.Wait()
	if got := store.uids(); len(got) != 2 {
		t.Fatalf("rows after a failed delivery = %v, want both kept", got)
	}
	if wait := q.DeliverPass(context.Background(), f.fn); wait != q.backoffMin {
		t.Errorf("wait = %v, want the first backoff %v", wait, q.backoffMin)
	}
	q.Wait()
	if len(f.attempts) != 1 {
		t.Errorf("attempts = %v, want one: b must wait behind a, and a is backing off", f.attempts)
	}

	// Recovery: after the backoff the same row is retried, then its successor,
	// each deleted on success and in order.
	f.failing["p"] = false
	now = now.Add(2 * time.Second)
	settle(q, f.fn)
	if strings.Join(f.delivered, ",") != "a,b" {
		t.Errorf("delivered = %v, want a,b in order", f.delivered)
	}
	if got := store.uids(); len(got) != 0 {
		t.Errorf("rows = %v, want all deleted", got)
	}
}

func TestWorkerBackoffDoublesToACap(t *testing.T) {
	q := newQueue(t, &memStore{}, "")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := q.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestWorkerDoesNotLetOnePortalBlockOthers(t *testing.T) {
	store := &memStore{}
	q := newQueue(t, store, "")
	for _, m := range []*database.InboundMessage{msg("a1", "stuck", 1), msg("b1", "fine", 2), msg("a2", "stuck", 3), msg("b2", "fine", 4)} {
		if _, err := q.Persist(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeDeliver{failing: map[string]bool{"stuck": true}}
	settle(q, f.fn)
	if strings.Join(f.delivered, ",") != "b1,b2" {
		t.Errorf("delivered = %v, want b1,b2", f.delivered)
	}
	if n := strings.Count(strings.Join(f.attempts, ","), "a"); n != 1 {
		t.Errorf("attempts = %v, want a1 tried once and a2 never ahead of it", f.attempts)
	}
	if got := strings.Join(store.uids(), ","); got != "a1,a2" {
		t.Errorf("rows = %s, want a1,a2 kept", got)
	}
}

// A text drained from the spool after a later one reached the database is
// still delivered first: delivery follows receive time, not row id.
func TestWorkerDeliversInReceiveOrderAfterASpoolDrain(t *testing.T) {
	store, dir := &memStore{down: true}, t.TempDir()
	q := newQueue(t, store, dir)
	if _, err := q.Persist(context.Background(), msg("early", "p", 1)); err != nil { // spooled
		t.Fatal(err)
	}
	store.setDown(false)
	if _, err := q.Persist(context.Background(), msg("late", "p", 2)); err != nil { // row 1
		t.Fatal(err)
	}
	if err := q.Drain(context.Background()); err != nil { // row 2
		t.Fatal(err)
	}
	f := &fakeDeliver{}
	settle(q, f.fn)
	if got := strings.Join(f.delivered, ","); got != "early,late" {
		t.Errorf("delivered = %s, want early,late", got)
	}
}

// A delivery that blocks (QueueRemoteEvent against a down homeserver) must not
// hold up other portals, and must not be started twice.
func TestWorkerDeliversOtherPortalsWhileOneIsBlocked(t *testing.T) {
	store := &memStore{}
	q := newQueue(t, store, "")
	for _, m := range []*database.InboundMessage{msg("a1", "slow", 1), msg("b1", "fast", 2)} {
		if _, err := q.Persist(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	started := make(chan string, 4)
	deliver := func(_ context.Context, m *database.InboundMessage) error {
		started <- m.UID
		if m.PortalID == "slow" {
			<-release
		}
		return nil
	}
	for range 3 {
		q.DeliverPass(context.Background(), deliver)
		time.Sleep(20 * time.Millisecond)
	}
	if got := strings.Join(store.uids(), ","); got != "a1" {
		t.Errorf("rows while slow is blocked = %s, want only a1", got)
	}
	if len(started) != 2 {
		t.Errorf("%d deliveries started, want 2 (a1 once, b1 once)", len(started))
	}
	close(release)
	q.Wait()
	if got := store.uids(); len(got) != 0 {
		t.Errorf("rows after release = %v, want none", got)
	}
}

// One portal with more stuck messages than a batch must not hide the others.
func TestWorkerIsNotStarvedByAPortalWithManyMessages(t *testing.T) {
	store := &memStore{}
	q := newQueue(t, store, "")
	for i := range batchSize + 10 {
		if _, err := q.Persist(context.Background(), msg(fmt.Sprintf("s%d", i), "stuck", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Persist(context.Background(), msg("late", "fine", 9999)); err != nil {
		t.Fatal(err)
	}
	f := &fakeDeliver{failing: map[string]bool{"stuck": true}}
	settle(q, f.fn)
	if strings.Join(f.delivered, ",") != "late" {
		t.Errorf("delivered = %v, want late", f.delivered)
	}
}

func TestRunMovesTheSpoolAndDeliversThenStops(t *testing.T) {
	store := &memStore{down: true}
	q := newQueue(t, store, t.TempDir())
	q.spoolInterval = 10 * time.Millisecond
	f := &fakeDeliver{}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := q.Persist(ctx, msg("a", "p", 1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { q.Run(ctx, f.fn); close(done) }()
	store.setDown(false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.delivered)
		f.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delivered) != 1 || len(store.uids()) != 0 {
		t.Errorf("delivered %v, rows %v; want a delivered and no rows", f.delivered, store.uids())
	}
}
