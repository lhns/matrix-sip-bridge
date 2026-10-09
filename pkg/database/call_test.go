package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/id"
)

func openRaw(t *testing.T) *dbutil.Database {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "test.db")), "sqlite3")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func testDB(t *testing.T) *Database {
	t.Helper()
	db := New(openRaw(t), zerolog.Nop())
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return db
}

func insertLeg(t *testing.T, db *Database, callID, conference string, room id.RoomID, receiver id.UserID) *Call {
	t.Helper()
	c := &Call{
		CallID:     callID,
		PortalID:   "15551234567",
		RoomID:     room,
		Receiver:   receiver,
		Direction:  DirectionInbound,
		Conference: conference,
		State:      StateRinging,
	}
	if err := db.Call.Insert(context.Background(), c); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return c
}

// Only one leg of a conference may be answered, and the loser must not be
// touched.
func TestAnswerClaimsTheConferenceOnce(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	a := insertLeg(t, db, "a", "sip-1", "!a:example.com", "@alice:example.com")
	b := insertLeg(t, db, "b", "sip-1", "!b:example.com", "@bob:example.com")
	other := insertLeg(t, db, "c", "sip-2", "!c:example.com", "@alice:example.com")

	if won, err := db.Call.Answer(ctx, a, "@alice:example.com"); err != nil || !won {
		t.Fatalf("first Answer = %v, %v, want a win", won, err)
	}
	if a.State != StateBridged || a.MatrixUser != "@alice:example.com" {
		t.Errorf("winner = %q for %q, want bridged for alice", a.State, a.MatrixUser)
	}
	if won, err := db.Call.Answer(ctx, b, "@bob:example.com"); err != nil || won {
		t.Fatalf("second Answer = %v, %v, want a loss", won, err)
	}
	if b.State != StateRinging || b.MatrixUser != "" {
		t.Errorf("loser = %q for %q, want it untouched", b.State, b.MatrixUser)
	}
	if won, err := db.Call.Answer(ctx, other, "@alice:example.com"); err != nil || !won {
		t.Fatalf("Answer on another conference = %v, %v, want a win", won, err)
	}
	if won, err := db.Call.Answer(ctx, a, "@alice:example.com"); err != nil || won {
		t.Errorf("Answer on a bridged row = %v, %v, want no second win", won, err)
	}
}

func TestGetActiveByConferenceIsPerReceiver(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	insertLeg(t, db, "a", "sip-1", "!a:example.com", "@alice:example.com")

	if got, err := db.Call.GetActiveByConference(ctx, "sip-1", "@alice:example.com"); err != nil || got == nil || got.CallID != "a" {
		t.Errorf("alice's leg = %+v, %v, want call a", got, err)
	}
	if got, err := db.Call.GetActiveByConference(ctx, "sip-1", "@bob:example.com"); err != nil || got != nil {
		t.Errorf("bob's leg = %+v, %v, want none", got, err)
	}
	if got, err := db.Call.GetActiveByRoom(ctx, "!a:example.com"); err != nil || got == nil || got.CallID != "a" {
		t.Errorf("room a = %+v, %v, want call a", got, err)
	}
}

func TestAnsweredElsewhere(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	a := insertLeg(t, db, "a", "sip-1", "!a:example.com", "@alice:example.com")
	b := insertLeg(t, db, "b", "sip-1", "!b:example.com", "@bob:example.com")
	other := insertLeg(t, db, "c", "sip-2", "!c:example.com", "@carol:example.com")
	window := time.Minute

	if got, err := db.Call.AnsweredElsewhere(ctx, b, window); err != nil || got != nil {
		t.Fatalf("before anyone answered = %+v, %v, want none", got, err)
	}
	if _, err := db.Call.Answer(ctx, a, "@alice:example.com"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	got, err := db.Call.AnsweredElsewhere(ctx, b, window)
	if err != nil || got == nil || got.RoomID != "!a:example.com" || got.MatrixUser != "@alice:example.com" {
		t.Fatalf("with a bridged = %+v, %v, want alice's row", got, err)
	}
	if got, err := db.Call.AnsweredElsewhere(ctx, a, window); err != nil || got != nil {
		t.Errorf("a's own answer = %+v, %v, want none", got, err)
	}
	if got, err := db.Call.AnsweredElsewhere(ctx, other, window); err != nil || got != nil {
		t.Errorf("another conference = %+v, %v, want none", got, err)
	}
	// An ended winner no longer counts: one whose media failed sent no 200.
	if _, err := db.Call.End(ctx, a); err != nil {
		t.Fatalf("End: %v", err)
	}
	if got, err := db.Call.AnsweredElsewhere(ctx, b, window); err != nil || got != nil {
		t.Errorf("with a ended = %+v, %v, want none", got, err)
	}

	// An answer from long before this leg started is a previous call.
	old := insertLeg(t, db, "old", "sip-3", "!o:example.com", "@alice:example.com")
	old.CreatedAt = time.Now().Add(-time.Hour)
	if _, err := db.Exec(ctx, "UPDATE sip_call SET created_at = $2, matrix_user = '@alice:example.com', state = 'ended' WHERE call_id = $1",
		old.CallID, old.CreatedAt.UnixMilli()); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	fresh := insertLeg(t, db, "fresh", "sip-3", "!f:example.com", "@bob:example.com")
	if got, err := db.Call.AnsweredElsewhere(ctx, fresh, window); err != nil || got != nil {
		t.Errorf("an answer an hour earlier = %+v, %v, want none", got, err)
	}
}

// A database from before per-user rooms must upgrade in place and keep its
// rows readable.
func TestUpgradeFromTheFirstSchemaKeepsRows(t *testing.T) {
	ctx := context.Background()
	raw := openRaw(t)
	for _, stmt := range []string{
		`CREATE TABLE sip_bridge_version (version INTEGER, compat INTEGER)`,
		`INSERT INTO sip_bridge_version (version, compat) VALUES (1, 1)`,
		`CREATE TABLE sip_call (
			call_id TEXT NOT NULL, portal_id TEXT NOT NULL, room_id TEXT NOT NULL,
			direction TEXT NOT NULL, conference TEXT NOT NULL, lk_room TEXT NOT NULL,
			lk_identity TEXT NOT NULL, lk_participant TEXT NOT NULL, state TEXT NOT NULL,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, PRIMARY KEY (call_id))`,
		`CREATE INDEX sip_call_portal_idx ON sip_call (portal_id, state)`,
		`INSERT INTO sip_call VALUES ('old', '15551234567', '!a:example.com', 'inbound', 'sip-1', '', '', '', 'ringing', 1, 1)`,
	} {
		if _, err := raw.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	db := New(raw, zerolog.Nop())
	if err := db.Upgrade(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	got, err := db.Call.GetActiveByRoom(ctx, "!a:example.com")
	if err != nil || got == nil {
		t.Fatalf("the old row = %+v, %v, want it kept", got, err)
	}
	if got.Receiver != "" || got.MatrixUser != "" {
		t.Errorf("the old row has receiver %q and user %q, want both empty", got.Receiver, got.MatrixUser)
	}
}
