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

	// Everything below is touched by the HTTP handler on the server's own
	// goroutine as well as by the test, including the staged replies: a test
	// that stages one while a call is in flight -- which is what the watcher
	// tests do -- otherwise races the handler reading it.
	mu sync.Mutex
	// reply overrides the canned response body for a twirp method, and status
	// the HTTP status it comes back with, so a twirp error can be staged.
	reply   map[string]string
	status  map[string]int
	methods []string
	bodies  map[string]json.RawMessage
	claims  map[string]*tokenClaims
}

// stage overrides the canned reply for a twirp method.
func (f *fakeLiveKit) stage(method, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply[method] = body
}

// stageError stages a twirp error: a non-200 status and the JSON error body
// that goes with it.
func (f *fakeLiveKit) stageError(method string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[method] = status
	f.reply[method] = body
}

// clearError undoes stageError's status, leaving any staged body in place.
func (f *fakeLiveKit) clearError(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.status, method)
}

// staged returns the reply and status for a method.
func (f *fakeLiveKit) staged(method string) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reply[method], f.status[method]
}

func newFakeLiveKit(t *testing.T) *fakeLiveKit {
	t.Helper()
	f := &fakeLiveKit{
		reply:  map[string]string{},
		status: map[string]int{},
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
		reply, code := f.reply[method], f.status[method]
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			w.WriteHeader(code)
		}
		if reply != "" {
			_, _ = w.Write([]byte(reply))
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
	return testDatabaseLogged(t, dbutil.NoopLogger)
}

// testDatabaseLogged is testDatabase with a query logger. The logger goes on
// the parent: a dbutil child copies the parent's LoggingDB, which keeps
// pointing at the parent, so setting Log on the child is not observed.
func testDatabaseLogged(t *testing.T, log dbutil.DatabaseLogger) *database.Database {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "test.db")), "sqlite3")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	raw.Log = log
	db := database.New(raw, zerolog.Nop())
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return db
}

// The participant watcher is the only signal the bridge has that a call ended,
// and an error from it is retried for as long as the call row lives. A room
// LiveKit no longer has is not an error to retry: nobody is in it.
func TestParticipantPresentOnARoomThatIsGone(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		present bool
		wantErr bool
	}{
		{
			// What livekit-server 1.13 with the redis store actually answers.
			name: "an unknown room lists nobody",
			body: `{"participants":[]}`,
		},
		{
			name:   "an unknown room reported as twirp not_found",
			status: http.StatusNotFound,
			body:   `{"code":"not_found","msg":"room not found"}`,
		},
		{
			name:    "any other failure is still an error",
			status:  http.StatusInternalServerError,
			body:    `{"code":"internal","msg":"redis is down"}`,
			wantErr: true,
		},
		{
			name:    "the participant is in the room",
			body:    `{"participants":[{"identity":"identity"}]}`,
			present: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLiveKit(t)
			if tt.status != 0 {
				f.stageError("ListParticipants", tt.status, tt.body)
			} else {
				f.stage("ListParticipants", tt.body)
			}
			present, err := f.client().ParticipantPresent(t.Context(), "qKKEsmRoomName", "identity")
			if tt.wantErr && err == nil {
				t.Fatal("ParticipantPresent returned no error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ParticipantPresent: %v", err)
			}
			if present != tt.present {
				t.Errorf("present = %v, want %v", present, tt.present)
			}
		})
	}
}

// A call that ends before its media was bridged still asks LiveKit to remove
// the participant, because the identity is reserved when the row is created.
// LiveKit has nothing to remove then, which is the state being asked for.
func TestRemoveParticipantThatIsAlreadyGone(t *testing.T) {
	f := newFakeLiveKit(t)
	f.stageError("RemoveParticipant", http.StatusNotFound, `{"code":"not_found","msg":"participant does not exist"}`)

	if err := f.client().RemoveParticipant(t.Context(), "qKKEsmRoomName", "identity"); err != nil {
		t.Errorf("RemoveParticipant: %v", err)
	}

	f.stageError("RemoveParticipant", http.StatusInternalServerError, `{"code":"internal","msg":"redis is down"}`)
	if err := f.client().RemoveParticipant(t.Context(), "qKKEsmRoomName", "identity"); err == nil {
		t.Error("a real failure was reported as success")
	}
}

// The handler read the staged reply and status without the lock, so staging
// one while a request was in flight -- which is what a test that adjusts what
// LiveKit says between polls does -- was a data race on the maps themselves.
func TestStagingWhileARequestIsInFlight(t *testing.T) {
	f := newFakeLiveKit(t)
	client := f.client()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_, _ = client.ParticipantPresent(context.Background(), "qKKEsmRoomName", "identity")
		}
	}()
	// Staged for as long as there are requests in flight, not a fixed number
	// of times: a staging loop that finishes first never overlaps a handler.
	for {
		select {
		case <-done:
			f.stage("ListParticipants", `{"participants":[]}`)
			goto settled
		default:
		}
		f.stageError("ListParticipants", http.StatusInternalServerError, `{"code":"internal","msg":"gone"}`)
		f.clearError("ListParticipants")
		f.stage("ListParticipants", `{"participants":[]}`)
	}
settled:

	if reply, status := f.staged("ListParticipants"); reply == "" || status != 0 {
		t.Errorf("staged = %q, %d; want the last staged body and no status", reply, status)
	}
}
