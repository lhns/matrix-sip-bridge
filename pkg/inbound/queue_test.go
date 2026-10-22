package inbound

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func (s *memStore) Pending(_ context.Context, limit int) ([]*database.InboundMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errDown
	}
	return append([]*database.InboundMessage(nil), s.rows[:min(limit, len(s.rows))]...), nil
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

	wait := q.DeliverPass(context.Background(), f.fn)
	if got := store.uids(); len(got) != 2 {
		t.Fatalf("rows after a failed delivery = %v, want both kept", got)
	}
	if wait != q.backoffMin {
		t.Errorf("wait = %v, want the first backoff %v", wait, q.backoffMin)
	}
	if len(f.attempts) != 1 {
		t.Errorf("attempts = %v, want one: b must wait behind a", f.attempts)
	}

	// Before the backoff expires nothing is retried.
	q.DeliverPass(context.Background(), f.fn)
	if len(f.attempts) != 1 {
		t.Errorf("attempts = %v, want no retry before the backoff expires", f.attempts)
	}

	// Recovery: after the backoff the same row is retried, then its successor,
	// each deleted on success and in order.
	f.failing["p"] = false
	now = now.Add(2 * time.Second)
	q.DeliverPass(context.Background(), f.fn)
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
	q.DeliverPass(context.Background(), f.fn)
	if strings.Join(f.delivered, ",") != "b1,b2" {
		t.Errorf("delivered = %v, want b1,b2", f.delivered)
	}
	if strings.Join(f.attempts, ",") != "a1,b1,b2" {
		t.Errorf("attempts = %v, want a1 tried once and a2 never ahead of it", f.attempts)
	}
	if got := strings.Join(store.uids(), ","); got != "a1,a2" {
		t.Errorf("rows = %s, want a1,a2 kept", got)
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
