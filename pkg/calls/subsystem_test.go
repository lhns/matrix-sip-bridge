package calls

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

func TestConferenceNameRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		portalID   string
		conference string
	}{
		{"default prefix", "sip-", "15551234567", "sip-15551234567"},
		{"underscore prefix", "matrix_", "442071234567", "matrix_442071234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subsystem{}
			s.cfg.ConferencePrefix = tt.prefix

			if got := s.conferenceFor(tt.portalID); got != tt.conference {
				t.Errorf("conferenceFor(%q) = %q, want %q", tt.portalID, got, tt.conference)
			}
			got, ok := s.portalIDFromConference(tt.conference)
			if !ok {
				t.Fatalf("portalIDFromConference(%q) did not match", tt.conference)
			}
			if got != tt.portalID {
				t.Errorf("portalIDFromConference(%q) = %q, want %q", tt.conference, got, tt.portalID)
			}
		})
	}
}

// The SIP server may route calls the bridge knows nothing about. Reacting to
// those would create portal rooms for whatever their names happen to contain.
func TestPortalIDFromConferenceRejectsForeignRooms(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		conference string
	}{
		{"different prefix", "sip-", "meeting-1234"},
		{"prefix but nothing after it", "sip-", "sip-"},
		{"prefix in the middle", "sip-", "team-sip-1234"},
		{"empty conference", "sip-", ""},
		{"no prefix configured matches nothing", "", "sip-15551234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subsystem{}
			s.cfg.ConferencePrefix = tt.prefix
			if got, ok := s.portalIDFromConference(tt.conference); ok {
				t.Errorf("portalIDFromConference(%q) matched as %q, want no match", tt.conference, got)
			}
		})
	}
}

func TestNewCallIDIsUniqueAndLongEnough(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newCallID()
		// deviceIDFor slices the first 8 characters for the device
		// ID, so anything shorter would panic at runtime.
		if len(id) != 32 {
			t.Fatalf("call ID %q has length %d, want 32", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate call ID %q", id)
		}
		seen[id] = true
	}
}

func TestWantedTrunk(t *testing.T) {
	s := &Subsystem{}
	s.cfg.LiveKit = LiveKitConfig{
		TrunkName:         "matrix-sip-bridge",
		TrunkAddress:      "pbx.example.com",
		TrunkNumber:       "+15551234567",
		TrunkAuthUsername: "bridge",
		TrunkAuthPassword: "secret",
	}
	trunk := s.wantedTrunk()
	if trunk.Name != "matrix-sip-bridge" || trunk.Address != "pbx.example.com" {
		t.Errorf("unexpected trunk identity: %+v", trunk)
	}
	if len(trunk.Numbers) != 1 || trunk.Numbers[0] != "+15551234567" {
		t.Errorf("Numbers = %v, want one entry", trunk.Numbers)
	}

	s.cfg.LiveKit.TrunkNumber = ""
	if got := s.wantedTrunk(); got.Numbers != nil {
		t.Errorf("Numbers = %v, want nil when no number is configured", got.Numbers)
	}
}

func TestCurrentTrunkIDBeforeReconcile(t *testing.T) {
	s := &Subsystem{}
	if _, err := s.currentTrunkID(); err == nil {
		t.Error("expected an error before the trunk has been reconciled")
	}
	empty := ""
	s.trunkID.Store(&empty)
	if _, err := s.currentTrunkID(); err == nil {
		t.Error("expected an error for an empty trunk ID")
	}
	id := "ST_abc123"
	s.trunkID.Store(&id)
	got, err := s.currentTrunkID()
	if err != nil || got != id {
		t.Errorf("currentTrunkID() = %q, %v; want %q, nil", got, err, id)
	}
}

// sourceArgument is the position of the *bridgev2.UserLogin in the bridgev2
// entry points that take one. Passing nil there is a panic, not an error.
var sourceArgument = map[string]int{
	"CreateMatrixRoom": 1,
	"QueueRemoteEvent": 0,
}

func TestNoBridgev2CallPassesANilSource(t *testing.T) {
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			arg, ok := sourceArgument[sel.Sel.Name]
			if !ok || arg >= len(call.Args) {
				return true
			}
			if id, ok := call.Args[arg].(*ast.Ident); ok && id.Name == "nil" {
				t.Errorf("%s:%d passes a nil source to %s; bridgev2 dereferences it",
					rel, fset.Position(call.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

// moduleRoot walks up from the test's directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

// A row left in progress by an interrupted setup blocks every later call from
// the same number, because the conference is named after the number rather
// than after the call. Nothing rings for longer than the ring timeout, so a
// row that still says so afterwards is wreckage; a bridged call has no such
// bound and is only given up on at the membership expiry.
func TestCallIsStale(t *testing.T) {
	s := &Subsystem{}
	s.cfg.RingTimeout = 45 * time.Second
	s.cfg.MembershipExpiry = 6 * time.Hour
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name  string
		call  database.Call
		stale bool
	}{
		{
			name:  "a call still ringing within the timeout",
			call:  database.Call{State: database.StateRinging, CreatedAt: now.Add(-10 * time.Second)},
			stale: false,
		},
		{
			name:  "a call still ringing just past the timeout is inside the grace",
			call:  database.Call{State: database.StateRinging, CreatedAt: now.Add(-50 * time.Second)},
			stale: false,
		},
		{
			name:  "a call still ringing minutes later cannot be",
			call:  database.Call{State: database.StateRinging, CreatedAt: now.Add(-10 * time.Minute)},
			stale: true,
		},
		{
			name:  "a long bridged call is not stale",
			call:  database.Call{State: database.StateBridged, UpdatedAt: now.Add(-time.Hour)},
			stale: false,
		},
		{
			name:  "a bridged call past the membership expiry is",
			call:  database.Call{State: database.StateBridged, UpdatedAt: now.Add(-7 * time.Hour)},
			stale: true,
		},
		{
			name:  "an ended call is never stale",
			call:  database.Call{State: database.StateEnded, UpdatedAt: now.Add(-7 * time.Hour)},
			stale: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.callIsStale(&tt.call, now); got != tt.stale {
				t.Errorf("callIsStale = %v, want %v", got, tt.stale)
			}
		})
	}
}

// The outbound INVITE blocks until the callee answers. A Matrix user hanging up
// while the phone rings has no other way to reach it, and without the
// cancellation the dialplan's Originate() runs to completion and the callee
// lands alone in the conference.
func TestInviteContextCancelsWhenTheCallEnds(t *testing.T) {
	s := &Subsystem{ended: make(map[string]chan struct{})}
	ctx, stop := s.inviteContext(context.Background(), "call-1")
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatal("the INVITE was cancelled before the call ended")
	default:
	}

	s.signalEnded("call-1")

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the INVITE was not cancelled when the call ended")
	}
}

// stop runs on every outbound call, including the ones that connect, so it has
// to release both the goroutine and the map entry. It is called from a defer as
// well as inline, so calling it twice must not panic.
func TestInviteContextStopReleasesTheCall(t *testing.T) {
	s := &Subsystem{ended: make(map[string]chan struct{})}
	ctx, stop := s.inviteContext(context.Background(), "call-1")
	if len(s.ended) != 1 {
		t.Fatalf("ended = %v, want one entry while the INVITE is out", s.ended)
	}
	stop()
	stop()
	if len(s.ended) != 0 {
		t.Errorf("ended = %v, want it emptied once the INVITE is done", s.ended)
	}
	if ctx.Err() == nil {
		t.Error("stop left the invite context live")
	}
	// A call that ends after the INVITE is done must not panic on a closed
	// channel or find anything left to signal.
	s.signalEnded("call-1")
}

// The ring notification is retracted by redacting it, so the event IDs have to
// come back exactly once: twice would redact an event that is already gone,
// and leaving them behind would let a decline arriving after the call ended
// match a call that no longer exists.
func TestTakeNotificationsIsExhaustive(t *testing.T) {
	s := &Subsystem{notifies: map[id.EventID]string{
		"$one":   "call-1",
		"$two":   "call-1",
		"$other": "call-2",
	}}

	taken := s.takeNotifications("call-1")
	slices.Sort(taken)
	want := []id.EventID{"$one", "$two"}
	if !slices.Equal(taken, want) {
		t.Errorf("takeNotifications = %v, want %v", taken, want)
	}
	if got := s.takeNotifications("call-1"); len(got) != 0 {
		t.Errorf("takeNotifications again = %v, want nothing left", got)
	}
	// Another call's notification is not collateral.
	if got := s.takeNotifications("call-2"); len(got) != 1 || got[0] != "$other" {
		t.Errorf("takeNotifications(call-2) = %v, want the one it sent", got)
	}
	// A decline naming a retracted notification must no longer match.
	if callID, ok := s.signalDecline("$one"); ok || callID != "" {
		t.Errorf("signalDecline after retraction = %q, %v; want no match", callID, ok)
	}
}

// fakeInboundLeg is the control leg of an inbound call, recording what the
// subsystem did to it. Answer closes Done, as the real one effectively does:
// the dialplan hangs the leg up the moment it is answered.
type fakeInboundLeg struct {
	mu        sync.Mutex
	answered  int
	rejects   []int
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeInboundLeg() *fakeInboundLeg {
	return &fakeInboundLeg{done: make(chan struct{})}
}

func (l *fakeInboundLeg) From() string          { return "sip:caller@example.com" }
func (l *fakeInboundLeg) Conference() string    { return "sip-15551234567" }
func (l *fakeInboundLeg) Done() <-chan struct{} { return l.done }
func (l *fakeInboundLeg) Ringing() error        { return nil }
func (l *fakeInboundLeg) finish()               { l.closeOnce.Do(func() { close(l.done) }) }

func (l *fakeInboundLeg) Answer() error {
	l.mu.Lock()
	l.answered++
	l.mu.Unlock()
	l.finish()
	return nil
}

func (l *fakeInboundLeg) Reject(code int, _ string) error {
	l.mu.Lock()
	l.rejects = append(l.rejects, code)
	l.mu.Unlock()
	l.finish()
	return nil
}

func (l *fakeInboundLeg) state() (int, []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.answered, append([]int(nil), l.rejects...)
}

// Nothing repeats a call.member join, and signalAnswer drops a signal for a
// call with no channel waiting for it. A user who answered while the ring
// notification was still going out, or while the media was being bridged, was
// therefore never heard: the call rang on and was declined at the timeout.
func TestAnswerArrivingBeforeTheWaitIsNotLost(t *testing.T) {
	s := &Subsystem{
		db:       testDatabase(t),
		log:      zerolog.Nop(),
		answered: map[string]chan struct{}{},
		declined: map[string]chan struct{}{},
		ended:    map[string]chan struct{}{},
	}
	s.cfg.RingTimeout = 5 * time.Second

	callID := newCallID()
	waiters := s.waitersFor(callID)
	defer s.forgetWaiters(callID)

	call := &database.Call{
		CallID:     callID,
		PortalID:   "15551234567",
		RoomID:     "!portal:example.com",
		Direction:  database.DirectionInbound,
		Conference: "sip-15551234567",
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(t.Context(), call); err != nil {
		t.Fatalf("insert call: %v", err)
	}

	// The answer lands while the call is still being set up.
	s.signalAnswer(callID)

	leg := newFakeInboundLeg()
	s.waitForMatrix(t.Context(), call, leg, zerolog.Nop(), waiters)

	answered, rejects := leg.state()
	if answered != 1 {
		t.Errorf("the leg was answered %d times, want once", answered)
	}
	if len(rejects) != 0 {
		t.Errorf("the leg was rejected with %v, want no rejection", rejects)
	}
	if call.State != database.StateBridged {
		t.Errorf("call state = %q, want %q", call.State, database.StateBridged)
	}
}
