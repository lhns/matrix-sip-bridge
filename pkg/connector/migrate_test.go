package connector

import (
	"context"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

const (
	testOwner = id.UserID("@alice:example.com")

	spaceID  = "home-space"
	dmA      = "home-+15551234567"
	dmB      = "home-+15559876543"
	dmNoRoom = "+15550001111"
)

func ownerPermissions() bridgeconfig.PermissionConfig {
	return bridgeconfig.PermissionConfig{
		string(testOwner): &bridgeconfig.Permissions{Login: true},
	}
}

func mustExec(t *testing.T, br *bridgev2.Bridge, query string, args ...any) {
	t.Helper()
	if _, err := br.DB.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func portalKeyOf(id string) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(id)}
}

// seedSharedLogin writes what a deployment before per-user logins has: the
// "sip" login of the owner with its personal space, a line space, two DMs in
// it, one portal that never got a room, a message and user_portal rows.
func seedSharedLogin(t *testing.T, br *bridgev2.Bridge) {
	t.Helper()
	ctx := context.Background()
	if err := br.DB.User.Insert(ctx, &database.User{BridgeID: br.ID, MXID: testOwner}); err != nil {
		t.Fatal(err)
	}
	if err := br.DB.UserLogin.Insert(ctx, &database.UserLogin{
		BridgeID: br.ID, UserMXID: testOwner, ID: legacyLoginID, RemoteName: "SIP",
		SpaceRoom: "!personal:example.com",
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*database.Portal{
		testPortal(portalKeyOf(spaceID), "!space:example.com", database.RoomTypeSpace),
		testPortal(portalKeyOf(dmA), "!a:example.com", database.RoomTypeDM),
		testPortal(portalKeyOf(dmB), "!b:example.com", database.RoomTypeDM),
		testPortal(portalKeyOf(dmNoRoom), "", database.RoomTypeDM),
	} {
		if p.ID == dmA || p.ID == dmB {
			p.ParentKey = portalKeyOf(spaceID)
		}
		if err := br.DB.Portal.Insert(ctx, p); err != nil {
			t.Fatalf("insert portal %s: %v", p.ID, err)
		}
	}
	mustExec(t, br, `INSERT INTO ghost (bridge_id, id, name, avatar_id, avatar_hash, avatar_mxc, name_set, avatar_set,
		contact_info_set, is_bot, identifiers, metadata) VALUES ($1, $2, '', '', '', '', false, false, false, false, '[]', '{}')`,
		br.ID, dmA)
	if err := br.DB.Message.Insert(ctx, &database.Message{
		BridgeID: br.ID, ID: "m1", PartID: "", MXID: "$event:example.com",
		Room: portalKeyOf(dmA), SenderID: dmA, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	for _, p := range []string{dmA, dmB} {
		mustExec(t, br, `INSERT INTO user_portal (bridge_id, user_mxid, login_id, portal_id, portal_receiver, in_space, preferred, last_read)
			VALUES ($1, $2, $3, $4, '', true, true, 42)`, br.ID, testOwner, legacyLoginID, p)
	}
}

func portalAt(t *testing.T, br *bridgev2.Bridge, id, receiver string) *database.Portal {
	t.Helper()
	p, err := br.DB.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: networkid.PortalID(id), Receiver: networkid.UserLoginID(receiver)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func count(t *testing.T, br *bridgev2.Bridge, query string, args ...any) int {
	t.Helper()
	var n int
	if err := br.DB.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The old shared rooms end up under their owner's own login with the same
// Matrix rooms, and the old login is gone.
func TestMigrationMovesTheSharedRoomsToTheOwner(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	ctx := context.Background()
	login := string(loginIDFor(testOwner))

	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for id, room := range map[string]id.RoomID{spaceID: "!space:example.com", dmA: "!a:example.com", dmB: "!b:example.com"} {
		p := portalAt(t, br, id, login)
		if p == nil || p.MXID != room {
			t.Errorf("portal %s under %s = %+v, want room %s kept", id, login, p, room)
		}
		if portalAt(t, br, id, "") != nil {
			t.Errorf("portal %s still has an empty receiver", id)
		}
	}
	if portalAt(t, br, dmNoRoom, "") != nil || portalAt(t, br, dmNoRoom, login) != nil {
		t.Error("the portal that never had a room should be gone, not duplicated")
	}
	for _, id := range []string{dmA, dmB} {
		if p := portalAt(t, br, id, login); p.ParentKey.Receiver != networkid.UserLoginID(login) || p.ParentKey.ID != spaceID {
			t.Errorf("parent of %s = %+v, want the space under the new login", id, p.ParentKey)
		}
	}
	if n := count(t, br, `SELECT COUNT(*) FROM message WHERE room_id=$1 AND room_receiver=$2`, dmA, login); n != 1 {
		t.Errorf("messages in the moved portal = %d, want 1", n)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM user_portal WHERE login_id=$1 AND preferred AND in_space AND last_read=42`, login); n != 2 {
		t.Errorf("user_portal rows on the new login = %d, want 2 with their state", n)
	}
	if l, _ := br.GetExistingUserLoginByID(ctx, legacyLoginID); l != nil {
		t.Error("the shared login still exists")
	}
	if n := count(t, br, `SELECT COUNT(*) FROM user_login WHERE id=$1`, legacyLoginID); n != 0 {
		t.Error("the shared login row still exists")
	}
	nl, err := br.GetExistingUserLoginByID(ctx, loginIDFor(testOwner))
	if err != nil || nl == nil {
		t.Fatalf("owner's login = %v, %v", nl, err)
	}
	if nl.SpaceRoom != "!personal:example.com" {
		t.Errorf("personal space = %q, want it handed to the new login", nl.SpaceRoom)
	}
	if got := br.DB.KV.Get(ctx, keyDefaultRecipient); got != string(testOwner) {
		t.Errorf("default recipient = %q, want %q", got, testOwner)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	ctx := context.Background()
	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatal(err)
	}
	before := count(t, br, `SELECT COUNT(*) FROM portal`)
	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := count(t, br, `SELECT COUNT(*) FROM portal`); after != before {
		t.Errorf("portals %d -> %d on the second run", before, after)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE receiver=''`); n != 0 {
		t.Errorf("%d portals without a receiver after the second run", n)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM user_login`); n != 1 {
		t.Errorf("logins = %d, want only the owner's", n)
	}
}

// A room the owner already has for the same number is never merged into: that
// would tombstone and delete the source room. The source stays where it was
// and the old login stays with it.
func TestMigrationNeverMergesIntoAnExistingRoom(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	ctx := context.Background()
	login := loginIDFor(testOwner)
	if _, _, err := sc.logins.ensure(ctx, testOwner, false); err != nil {
		t.Fatal(err)
	}
	own := testPortal(networkid.PortalKey{ID: dmA, Receiver: login}, "!own:example.com", database.RoomTypeDM)
	if err := br.DB.Portal.Insert(ctx, own); err != nil {
		t.Fatal(err)
	}

	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatal(err)
	}

	if p := portalAt(t, br, dmA, ""); p == nil || p.MXID != "!a:example.com" {
		t.Errorf("source portal = %+v, want it untouched", p)
	}
	if p := portalAt(t, br, dmA, string(login)); p == nil || p.MXID != "!own:example.com" {
		t.Errorf("target portal = %+v, want the owner's own room", p)
	}
	if p := portalAt(t, br, dmB, string(login)); p == nil || p.MXID != "!b:example.com" {
		t.Errorf("the other portal = %+v, want it moved regardless", p)
	}
	if l, _ := br.GetExistingUserLoginByID(ctx, legacyLoginID); l == nil {
		t.Error("the shared login was removed although a portal was skipped")
	}
	if nl, _ := br.GetExistingUserLoginByID(ctx, login); nl.SpaceRoom != "" {
		t.Error("the personal space moved while the shared login stays")
	}
}

// With no old login there is no owner to give receiver-less portals to, and
// the migration does not guess one.
func TestMigrationLeavesPortalsAloneWithoutAnOwner(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	mustExec(t, br, `DELETE FROM user_login`)
	if err := sc.migrateSharedLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if portalAt(t, br, dmA, "") == nil {
		t.Error("portal moved without an owner")
	}
}

func TestMigrationOnAFreshDatabaseDoesNothing(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	if err := sc.migrateSharedLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM user_login`); n != 0 {
		t.Errorf("logins = %d on a fresh database, want 0", n)
	}
}

// Every login resyncs at startup and invites its user to what it resyncs, so
// a login may only touch its own portals.
func TestResyncOnlyTouchesTheLoginsOwnPortals(t *testing.T) {
	br, sc := newTestBridge(t, bridgeconfigWith(testOwner, "@bob:example.com"))
	ctx := context.Background()
	alice, _, err := sc.logins.ensure(ctx, testOwner, true)
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := sc.logins.ensure(ctx, "@bob:example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []struct {
		receiver networkid.UserLoginID
		room     id.RoomID
	}{{alice.ID, "!alice:example.com"}, {bob.ID, "!bob:example.com"}} {
		key := networkid.PortalKey{ID: networkid.PortalID(dmA), Receiver: p.receiver}
		if err := br.DB.Portal.Insert(ctx, testPortal(key, p.room, database.RoomTypeDM)); err != nil {
			t.Fatalf("portal %d: %v", i, err)
		}
	}
	var keys []networkid.PortalKey
	sc.queueEvent = func(_ *bridgev2.UserLogin, evt bridgev2.RemoteEvent) { keys = append(keys, evt.GetPortalKey()) }

	alice.Client.(*SIPClient).resyncPortals(ctx)

	if len(keys) != 1 || keys[0].Receiver != alice.ID {
		t.Errorf("resynced %+v, want only the portal under %s", keys, alice.ID)
	}
}
