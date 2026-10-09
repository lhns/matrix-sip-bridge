package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// resync applies what a ChatResync for the portal would: its GetChatInfo,
// through bridgev2's UpdateInfo. The member list is left out; it has nothing
// to do with the parent and would need the ghost's whole Matrix side.
func resync(t *testing.T, br *bridgev2.Bridge, login *bridgev2.UserLogin, key networkid.PortalKey) {
	t.Helper()
	ctx := context.Background()
	portal, err := br.GetPortalByKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := login.Client.GetChatInfo(ctx, portal)
	if err != nil {
		t.Fatal(err)
	}
	info.Members = nil
	portal.UpdateInfo(ctx, info, login, nil, time.Time{})
}

func parentOf(t *testing.T, br *bridgev2.Bridge, id, receiver string) networkid.PortalKey {
	t.Helper()
	p := portalAt(t, br, id, receiver)
	if p == nil {
		t.Fatalf("portal %s/%s is gone", id, receiver)
	}
	return p.ParentKey
}

// A migrated space keeps its rooms through the next resync. Before lineParent,
// the resync named the parent receiver-less and bridgev2 created a second,
// empty space for it and moved the rooms there.
func TestResyncKeepsTheReceiversSpace(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	sc.Config.LineSpaces = true
	ctx := context.Background()
	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatal(err)
	}
	login, err := br.GetExistingUserLoginByID(ctx, loginIDFor(testOwner))
	if err != nil || login == nil {
		t.Fatalf("login = %v, %v", login, err)
	}
	receiver := string(login.ID)

	resync(t, br, login, networkid.PortalKey{ID: dmA, Receiver: login.ID})

	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE id=$1`, spaceID); n != 1 {
		t.Errorf("%d portals for the space, want the one", n)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE receiver=''`); n != 0 {
		t.Errorf("%d receiver-less portals after a resync", n)
	}
	for _, child := range []string{dmA, dmB} {
		if got := parentOf(t, br, child, receiver); got != (networkid.PortalKey{ID: spaceID, Receiver: login.ID}) {
			t.Errorf("parent of %s = %v, want the receiver's space", child, got)
		}
	}
	if len(botOf(br).created) != 0 {
		t.Errorf("rooms were created: %+v", botOf(br).created)
	}
}

// seedSplitSpace writes the state ChatInfo.ParentID left behind after the
// migration: the receiver's rooms in a receiver-less space created on a
// resync, and the receiver's own space, which its owner has since deleted,
// still keyed but empty.
func seedSplitSpace(t *testing.T, br *bridgev2.Bridge) *bridgev2.UserLogin {
	t.Helper()
	ctx := context.Background()
	if err := br.DB.User.Insert(ctx, &database.User{BridgeID: br.ID, MXID: testOwner}); err != nil {
		t.Fatal(err)
	}
	login := loginIDFor(testOwner)
	if err := br.DB.UserLogin.Insert(ctx, &database.UserLogin{
		BridgeID: br.ID, UserMXID: testOwner, ID: login, RemoteName: "SIP",
		SpaceRoom: "!personal:example.com",
	}); err != nil {
		t.Fatal(err)
	}
	br.DB.KV.Set(ctx, keyDefaultRecipient, string(testOwner))
	for _, p := range []*database.Portal{
		testPortal(portalKeyOf(spaceID), "!live:example.com", database.RoomTypeSpace),
		testPortal(networkid.PortalKey{ID: spaceID, Receiver: login}, "!dead:example.com", database.RoomTypeSpace),
	} {
		if err := br.DB.Portal.Insert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, number := range []string{"home-+15551230001", "home-+15551230002", "home-+15551230003", "home-+15551230004"} {
		p := testPortal(networkid.PortalKey{ID: networkid.PortalID(number), Receiver: login},
			id.RoomID("!"+strings.TrimPrefix(number, "home-+")+":example.com"), database.RoomTypeDM)
		p.ParentKey = portalKeyOf(spaceID)
		p.InSpace = true
		if err := br.DB.Portal.Insert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, br, `INSERT INTO user_portal (bridge_id, user_mxid, login_id, portal_id, portal_receiver, in_space, preferred, last_read)
		VALUES ($1, $2, $3, $4, $3, true, true, 0)`, br.ID, testOwner, login, spaceID)
	l, err := br.GetExistingUserLoginByID(ctx, login)
	if err != nil || l == nil {
		t.Fatalf("login = %v, %v", l, err)
	}
	return l
}

// The repair turns that state into the one the migration should have left:
// one space under the receiver, the room that holds the rooms, listed in the
// personal space in place of the deleted one.
func TestRepairMovesTheReceiverlessSpaceToItsReceiver(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	sc.Config.LineSpaces = true
	login := seedSplitSpace(t, br)
	ctx := context.Background()
	receiver := string(login.ID)
	bot := botOf(br)

	// As Start runs them.
	if err := sc.repairLineSpaces(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if err := sc.migrateSharedLogin(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if p := portalAt(t, br, spaceID, receiver); p == nil || p.MXID != "!live:example.com" {
		t.Errorf("the receiver's space = %+v, want the room that holds the rooms", p)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE id=$1`, spaceID); n != 1 {
		t.Errorf("%d portals for the space, want one", n)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE parent_id=$1 AND parent_receiver=$2`, spaceID, receiver); n != 4 {
		t.Errorf("%d rooms in the receiver's space, want 4", n)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE receiver='' OR (parent_id IS NOT NULL AND parent_receiver='')`); n != 0 {
		t.Errorf("%d portals still receiver-less or under a receiver-less parent", n)
	}
	children := bot.spaceChildren("!personal:example.com")
	if !children["!live:example.com"] {
		t.Error("the space is not listed in the personal space")
	}
	if children["!dead:example.com"] {
		t.Error("the deleted space is still listed in the personal space")
	}
	var removed bool
	for _, st := range bot.state {
		if st.room == "!personal:example.com" && st.key == "!dead:example.com" {
			removed = true
		}
	}
	if !removed {
		t.Error("the deleted space's entry was not removed from the personal space")
	}
	if n := count(t, br, `SELECT COUNT(*) FROM user_portal WHERE portal_id=$1 AND portal_receiver=$2 AND in_space`, spaceID, receiver); n != 1 {
		t.Errorf("user_portal rows for the space in the personal space = %d, want 1", n)
	}

	// A resync afterwards moves nothing.
	sent := bot.spaceStateCount()
	resync(t, br, login, networkid.PortalKey{ID: "home-+15551230001", Receiver: login.ID})
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE id=$1`, spaceID); n != 1 {
		t.Errorf("the resync made %d spaces", n)
	}
	if got := bot.spaceStateCount(); got != sent {
		t.Errorf("the resync sent %d space events", got-sent)
	}

	// And so does a second start.
	before := count(t, br, `SELECT COUNT(*) FROM portal`)
	if err := sc.repairLineSpaces(ctx); err != nil {
		t.Fatal(err)
	}
	if after := count(t, br, `SELECT COUNT(*) FROM portal`); after != before {
		t.Errorf("portals %d -> %d on the second run", before, after)
	}
	if got := bot.spaceStateCount(); got != sent {
		t.Errorf("the second run sent %d space events", got-sent)
	}
}

// Rooms of several receivers in one receiver-less space cannot be given to
// any one of them.
func TestRepairLeavesASharedSpaceAlone(t *testing.T) {
	br, sc := newTestBridge(t, bridgeconfigWith(testOwner, "@bob:example.com"))
	login := seedSplitSpace(t, br)
	ctx := context.Background()
	bob, _, err := sc.logins.ensure(ctx, "@bob:example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	p := testPortal(networkid.PortalKey{ID: dmA, Receiver: bob.ID}, "!bob:example.com", database.RoomTypeDM)
	p.ParentKey = portalKeyOf(spaceID)
	if err := br.DB.Portal.Insert(ctx, p); err != nil {
		t.Fatal(err)
	}

	if err := sc.repairLineSpaces(ctx); err != nil {
		t.Fatal(err)
	}

	if p := portalAt(t, br, spaceID, ""); p == nil {
		t.Error("the shared space was moved")
	}
	if p := portalAt(t, br, spaceID, string(login.ID)); p == nil || p.MXID != "!dead:example.com" {
		t.Errorf("the receiver's own space = %+v, want it untouched", p)
	}
}

// A line member's line space goes into their personal space, and the dial
// room and every new room on the line into the line space.
func TestNewRoomsLandInThePersonalSpace(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	br.Config.PersonalFilteringSpaces = true
	sc.Config.LineSpaces = true
	sc.Config.LineMembers = map[string][]id.UserID{"home": {testOwner}}
	ctx := context.Background()
	bot := botOf(br)

	sc.ensureLineMemberLogins(ctx)
	login, err := br.GetExistingUserLoginByID(ctx, loginIDFor(testOwner))
	if err != nil || login == nil {
		t.Fatalf("the member has no login: %v, %v", login, err)
	}
	client := login.Client.(*SIPClient)
	client.ensureLineRooms(ctx)

	space := portalAt(t, br, "home-space", string(login.ID))
	dial := portalAt(t, br, "home-dial", string(login.ID))
	if space == nil || space.MXID == "" || dial == nil || dial.MXID == "" {
		t.Fatalf("space %+v, dial room %+v, want both created", space, dial)
	}
	personal := login.SpaceRoom
	if personal == "" {
		t.Fatal("no personal space")
	}
	if !bot.spaceChildren(personal)[space.MXID] {
		t.Error("the line space is not in the personal space")
	}
	if !bot.spaceChildren(space.MXID)[dial.MXID] {
		t.Error("the dial room is not in the line space")
	}
	if bot.spaceChildren(personal)[dial.MXID] {
		t.Error("the dial room is in the personal space directly; it belongs in the line space")
	}
	if dial.ParentKey != (networkid.PortalKey{ID: "home-space", Receiver: login.ID}) {
		t.Errorf("dial room parent = %v", dial.ParentKey)
	}
	if req := bot.createdWith(dial.MXID); req == nil || req.IsDirect || dial.Name != "home ☎" || dial.RoomType != database.RoomTypeDefault {
		t.Errorf("dial room %q (%q) created with %+v, want a plain room named after the line", dial.Name, dial.RoomType, req)
	}

	// A new number's room on the line.
	created := len(bot.created)
	numberKey := networkid.PortalKey{ID: "home-+15551234567", Receiver: login.ID}
	portal, err := br.GetPortalByKey(ctx, numberKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := portal.CreateMatrixRoom(ctx, login, nil); err != nil {
		t.Fatal(err)
	}
	if !bot.spaceChildren(space.MXID)[portal.MXID] {
		t.Error("the new room is not in the line space")
	}
	if len(bot.created) != created+1 {
		t.Errorf("%d rooms created for one number, want 1 (no second space)", len(bot.created)-created)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE id='home-space'`); n != 1 {
		t.Errorf("%d home spaces", n)
	}

	// Again, at the next start: nothing new.
	created = len(bot.created)
	client.ensureLineRooms(ctx)
	if len(bot.created) != created {
		t.Errorf("a second start created %d rooms", len(bot.created)-created)
	}
}

// Who may not log in gets no login and no rooms, and someone dropped from the
// list keeps theirs.
func TestLineMembersSkipTheUnpermittedAndKeepTheRemoved(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	ctx := context.Background()
	sc.Config.LineMembers = map[string][]id.UserID{"home": {testOwner, "@mallory:example.com"}, "not a line": {testOwner}}

	sc.ensureLineMemberLogins(ctx)

	if l, _ := br.GetExistingUserLoginByID(ctx, loginIDFor(testOwner)); l == nil {
		t.Error("the permitted member has no login")
	}
	if n := count(t, br, `SELECT COUNT(*) FROM "user" WHERE mxid='@mallory:example.com'`); n != 0 {
		t.Error("the refused member left a user row")
	}

	sc.Config.LineMembers = nil
	sc.ensureLineMemberLogins(ctx)
	if l, _ := br.GetExistingUserLoginByID(ctx, loginIDFor(testOwner)); l == nil {
		t.Error("removing the member removed their login")
	}
	if got := br.DB.KV.Get(ctx, keyLineMembers); got != "" {
		t.Errorf("recorded members = %q, want none", got)
	}
}

// A number typed into a dial room opens that number's room on the room's
// line, and the reply links to it. It is never sent as a text.
func TestDialRoomOpensTheNumbersRoom(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	ctx := context.Background()
	login, _, err := sc.logins.ensure(ctx, testOwner, true)
	if err != nil {
		t.Fatal(err)
	}
	type opened struct {
		portalID string
		owner    id.UserID
	}
	var got []opened
	sc.openPortalFunc = func(_ context.Context, portalID string, owner id.UserID) (id.RoomID, error) {
		got = append(got, opened{portalID, owner})
		return "!opened:example.com", nil
	}
	dial := &bridgev2.Portal{Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: "home-dial", Receiver: login.ID},
		MXID:      "!dial:example.com",
	}}
	send := func(body string) string {
		t.Helper()
		bot := botOf(br)
		before := len(bot.messages)
		resp, err := login.Client.HandleMatrixMessage(ctx, &bridgev2.MatrixMessage{
			MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
				Event:   &event.Event{Sender: testOwner},
				Portal:  dial,
				Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
			},
		})
		if err != nil || resp == nil || resp.DB == nil {
			t.Fatalf("HandleMatrixMessage(%q) = %+v, %v", body, resp, err)
		}
		if len(bot.messages) != before+1 || bot.messages[before].room != "!dial:example.com" {
			t.Fatalf("no reply in the dial room for %q", body)
		}
		return bot.messages[before].content.Parsed.(*event.MessageEventContent).Body
	}

	reply := send("+1 555 123 4567")
	if len(got) != 1 || got[0] != (opened{"home-+15551234567", testOwner}) {
		t.Fatalf("opened %+v, want the number on the dial room's line for its owner", got)
	}
	if !strings.Contains(reply, "matrix.to") || !strings.Contains(reply, "opened:example.com") {
		t.Errorf("reply %q has no link to the room", reply)
	}

	reply = send("hello")
	if len(got) != 1 {
		t.Errorf("a non-number opened %+v", got[1:])
	}
	if !strings.Contains(reply, "not a usable number") {
		t.Errorf("reply %q does not say the number is unusable", reply)
	}
}

// The dial room is a plain room of the user's, not a DM with anyone.
func TestChatInfoOfADialRoom(t *testing.T) {
	sc := testClient(true)
	info, err := sc.GetChatInfo(context.Background(), portalFor("home-dial"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Name == nil || *info.Name != "home ☎" {
		t.Errorf("name = %v", info.Name)
	}
	if info.Type == nil || *info.Type != database.RoomTypeDefault {
		t.Errorf("room type = %v, want a plain room", info.Type)
	}
	if info.Members != nil {
		t.Errorf("the dial room has a member list: %+v", info.Members)
	}
	if got := bridgeRoomName("home-dial"); got != "home ☎" {
		t.Errorf("bridgeRoomName = %q", got)
	}
}

// The old shared login, kept when the migration had to skip a portal, belongs
// to the same user and must not grow a second set of line rooms.
func TestTheSharedLoginGetsNoLineRooms(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	seedSharedLogin(t, br)
	sc.Config.LineSpaces = true
	sc.Config.LineMembers = map[string][]id.UserID{"home": {testOwner}}
	ctx := context.Background()
	legacy, err := br.GetExistingUserLoginByID(ctx, legacyLoginID)
	if err != nil || legacy == nil {
		t.Fatalf("shared login = %v, %v", legacy, err)
	}

	legacy.Client.(*SIPClient).ensureLineRooms(ctx)

	if n := count(t, br, `SELECT COUNT(*) FROM portal WHERE receiver=$1`, legacyLoginID); n != 0 {
		t.Errorf("%d portals under the shared login", n)
	}
}

// A room that names the right space but was never listed in it, because
// creating the space or the link failed, is linked by the next resync; one
// already linked sends nothing.
func TestResyncRetriesAFailedSpaceLink(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	sc.Config.LineSpaces = true
	ctx := context.Background()
	if err := br.DB.User.Insert(ctx, &database.User{BridgeID: br.ID, MXID: testOwner}); err != nil {
		t.Fatal(err)
	}
	loginID := loginIDFor(testOwner)
	if err := br.DB.UserLogin.Insert(ctx, &database.UserLogin{
		BridgeID: br.ID, UserMXID: testOwner, ID: loginID, RemoteName: "SIP",
	}); err != nil {
		t.Fatal(err)
	}
	login, err := br.GetExistingUserLoginByID(ctx, loginID)
	if err != nil || login == nil {
		t.Fatalf("login = %v, %v", login, err)
	}
	spaceKey := networkid.PortalKey{ID: spaceID, Receiver: loginID}
	room := networkid.PortalKey{ID: "home-+15551230001", Receiver: loginID}
	child := testPortal(room, "!child:example.com", database.RoomTypeDM)
	child.ParentKey = spaceKey
	for _, p := range []*database.Portal{
		testPortal(spaceKey, "!space:example.com", database.RoomTypeSpace), child,
	} {
		if err := br.DB.Portal.Insert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	bot := botOf(br)

	resync(t, br, login, room)
	if p := portalAt(t, br, string(room.ID), string(loginID)); p == nil || !p.InSpace {
		t.Fatalf("the room is not linked after the resync: %+v", p)
	}
	if !bot.spaceChildren("!space:example.com")["!child:example.com"] {
		t.Error("the room is not listed in the space")
	}
	sent := bot.spaceStateCount()
	if sent == 0 {
		t.Fatal("no link events were sent")
	}

	resync(t, br, login, room)
	if got := bot.spaceStateCount(); got != sent {
		t.Errorf("a linked room got %d more space events", got-sent)
	}
}
