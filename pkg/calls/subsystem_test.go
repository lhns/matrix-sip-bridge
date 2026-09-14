package calls

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"

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

// bridgev2 dereferences the source UserLogin unconditionally while creating a
// room, so a call that arrives before anyone has logged in must be refused
// here. Without this the bridge panicked inside portal creation on the first
// inbound call.
func TestPortalCreationNeedsTheStaticLogin(t *testing.T) {
	s := &Subsystem{br: &bridgev2.Bridge{}, loginID: "sip"}
	login, err := s.sourceLogin()
	if err == nil {
		t.Fatalf("sourceLogin() = %v, want an error when no login is cached", login)
	}
	if login != nil {
		t.Errorf("sourceLogin() returned %v alongside an error", login)
	}
	if !strings.Contains(err.Error(), "sip") {
		t.Errorf("err = %v, want it to name the login", err)
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
