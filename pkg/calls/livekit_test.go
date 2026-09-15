package calls

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

const testAPISecret = "secret-for-tests-only-0123456789"

// fakeLiveKit records the twirp methods it was asked for, in order.
type fakeLiveKit struct {
	*httptest.Server

	// reply overrides the canned response body for a twirp method.
	reply map[string]string

	mu      sync.Mutex
	methods []string
	bodies  map[string]json.RawMessage
	claims  map[string]*tokenClaims
}

func newFakeLiveKit(t *testing.T) *fakeLiveKit {
	t.Helper()
	f := &fakeLiveKit{
		reply:  map[string]string{},
		bodies: map[string]json.RawMessage{},
		claims: map[string]*tokenClaims{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := filepath.Base(r.URL.Path)
		body, _ := io.ReadAll(r.Body)

		var claims tokenClaims
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
			return []byte(testAPISecret), nil
		})
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}

		f.mu.Lock()
		f.methods = append(f.methods, method)
		f.bodies[method] = json.RawMessage(body)
		f.claims[method] = &claims
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if body, ok := f.reply[method]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		switch method {
		case "CreateRoom":
			_, _ = w.Write([]byte(`{"sid":"RM_test","name":"room"}`))
		case "CreateSIPParticipant":
			_, _ = w.Write([]byte(`{"participant_id":"PA_test"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLiveKit) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *fakeLiveKit) client() *LiveKitClient {
	return NewLiveKitClient(LiveKitConfig{
		URL:       f.URL,
		APIKey:    "APItest",
		APISecret: testAPISecret,
	})
}

func TestEnsureRoomCreatesTheRoom(t *testing.T) {
	f := newFakeLiveKit(t)
	room, err := f.client().EnsureRoom(t.Context(), "qKKEsmRoomName")
	if err != nil {
		t.Fatalf("EnsureRoom: %v", err)
	}
	if room.Sid != "RM_test" {
		t.Errorf("Sid = %q, want RM_test", room.Sid)
	}
	if got := f.calls(); len(got) != 1 || got[0] != "CreateRoom" {
		t.Fatalf("methods = %v, want one CreateRoom", got)
	}

	var req map[string]any
	if err := json.Unmarshal(f.bodies["CreateRoom"], &req); err != nil {
		t.Fatalf("request body %q: %v", f.bodies["CreateRoom"], err)
	}
	if req["name"] != "qKKEsmRoomName" {
		t.Errorf("name = %v, want the room name", req["name"])
	}
	// Anything else in the request overwrites the settings the MatrixRTC
	// authorisation service gave a room it created first.
	if len(req) != 1 {
		t.Errorf("request = %v, want only the room name", req)
	}
	// LiveKit runs with room.auto_create off in a MatrixRTC deployment, so the
	// request is refused without this grant.
	if g := f.claims["CreateRoom"].Video; g == nil || !g.RoomCreate {
		t.Errorf("video grant = %+v, want roomCreate", g)
	}
}

// A portal room that nobody has ever joined from Matrix has no LiveKit room
// behind it, and livekit-sip cannot create one: CreateSIPParticipant then fails
// with "update room failed: not found" and the call never rings through.
func TestBridgeMediaCreatesTheRoomBeforeTheParticipant(t *testing.T) {
	f := newFakeLiveKit(t)
	s := &Subsystem{
		lk:   f.client(),
		db:   testDatabase(t),
		log:  zerolog.Nop(),
		seen: map[string]bool{},
	}
	trunk := "ST_test"
	s.trunkID.Store(&trunk)

	call := &database.Call{
		CallID:     newCallID(),
		PortalID:   "15551234567",
		RoomID:     "!portal:example.com",
		Direction:  database.DirectionInbound,
		Conference: "sip-15551234567",
		LKRoom:     "qKKEsmRoomName",
		LKIdentity: "identity",
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(t.Context(), call); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	if err := s.bridgeMedia(t.Context(), call); err != nil {
		t.Fatalf("bridgeMedia: %v", err)
	}

	want := []string{"CreateRoom", "CreateSIPParticipant"}
	got := f.calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("methods = %v, want %v", got, want)
	}

	var participant map[string]any
	if err := json.Unmarshal(f.bodies["CreateSIPParticipant"], &participant); err != nil {
		t.Fatalf("participant body: %v", err)
	}
	var room map[string]any
	if err := json.Unmarshal(f.bodies["CreateRoom"], &room); err != nil {
		t.Fatalf("room body: %v", err)
	}
	if room["name"] != participant["room_name"] {
		t.Errorf("created room %v but dialled into %v", room["name"], participant["room_name"])
	}
	if call.LKParticipant != "PA_test" {
		t.Errorf("LKParticipant = %q, want it recorded", call.LKParticipant)
	}
	// A call shorter than the poll interval was never observed in the room, so
	// the watcher would not end it and the number stayed out of service until
	// the membership expiry. livekit-sip answering is the observation.
	if !s.wasSeen(call.CallID) {
		t.Error("the call was not marked seen once its SIP participant existed")
	}
}

func testDatabase(t *testing.T) *database.Database {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "test.db")), "sqlite3")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	db := database.New(raw, zerolog.Nop())
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return db
}
