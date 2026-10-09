package connector

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

// stubMatrix is the Matrix connector reduced to what NewBridge, a login, a
// portal re-key and creating a room touch. Anything else panics on the nil
// interface, which is the signal that a test has outgrown it.
type stubMatrix struct {
	bridgev2.MatrixConnector
	bot *fakeBot
}

func (stubMatrix) Init(*bridgev2.Bridge)           {}
func (m stubMatrix) BotIntent() bridgev2.MatrixAPI { return m.bot }
func (m stubMatrix) GhostIntent(networkid.UserID) bridgev2.MatrixAPI {
	return m.bot
}
func (stubMatrix) ServerName() string { return "example.com" }
func (stubMatrix) GetCapabilities() *bridgev2.MatrixCapabilities {
	return &bridgev2.MatrixCapabilities{}
}
func (stubMatrix) GenerateDeterministicRoomID(networkid.PortalKey) id.RoomID { return "" }
func (stubMatrix) FormatGhostMXID(u networkid.UserID) id.UserID {
	return id.NewUserID("sip_"+string(u), "example.com")
}
func (stubMatrix) ParseGhostMXID(id.UserID) (networkid.UserID, bool) { return "", false }

func (stubMatrix) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return &event.PowerLevelsEventContent{}, nil
}
func (stubMatrix) GetMemberInfo(context.Context, id.RoomID, id.UserID) (*event.MemberEventContent, error) {
	return nil, nil
}

// NewUserIntent is no double puppet: rooms invite their user instead.
func (stubMatrix) NewUserIntent(_ context.Context, _ id.UserID, token string) (bridgev2.MatrixAPI, string, error) {
	return nil, token, nil
}
func (stubMatrix) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	join := &event.MemberEventContent{Membership: event.MembershipJoin}
	return map[id.UserID]*event.MemberEventContent{"@alice:example.com": join, "@bob:example.com": join}, nil
}
func (stubMatrix) SendBridgeStatus(context.Context, *status.BridgeState) error { return nil }

// testNetwork is the real connector with an Init that does not need the
// command processor or the Prometheus registry.
type testNetwork struct {
	*SIPConnector
}

func (n testNetwork) Init(br *bridgev2.Bridge) {
	n.SIPConnector.br = br
	n.SIPConnector.logins = &loginResolver{br: br}
	n.SIPConnector.health = newCallHealth(calls.NoticeConfig{})
}

// newTestBridge builds a real bridgev2.Bridge over a SQLite database with
// bridgev2's schema, and the connector on top of it. Foreign keys are switched
// on in the URI: the migration relies on ON UPDATE CASCADE (a re-keyed parent
// rewrites its children) and ON DELETE CASCADE, both of which SQLite ignores
// without it. Production relies on Postgres, or on SQLite with the same
// setting.
func newTestBridge(t *testing.T, permissions bridgeconfig.PermissionConfig) (*bridgev2.Bridge, *SIPConnector) {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "bridge.db"))+"?_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	sc := &SIPConnector{}
	br := bridgev2.NewBridge("test", raw, zerolog.Nop(),
		&bridgeconfig.BridgeConfig{CommandPrefix: "!sip", Permissions: permissions},
		stubMatrix{bot: &fakeBot{}}, testNetwork{sc}, func(*bridgev2.Bridge) bridgev2.CommandProcessor { return nil })
	br.BackgroundCtx = context.Background()
	if err := br.DB.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var fk int
	if err := raw.QueryRow(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys are not enforced in the test database (%d, %v); the cascade checks would prove nothing", fk, err)
	}
	return br, sc
}

func testPortal(key networkid.PortalKey, mxid id.RoomID, roomType database.RoomType) *database.Portal {
	return &database.Portal{
		BridgeID: "test", PortalKey: key, MXID: mxid, RoomType: roomType,
		Metadata: &PortalMetadata{},
	}
}

// bridgeconfigWith lets each given user log in.
func bridgeconfigWith(users ...id.UserID) bridgeconfig.PermissionConfig {
	cfg := bridgeconfig.PermissionConfig{}
	for _, u := range users {
		cfg[string(u)] = &bridgeconfig.Permissions{Login: true}
	}
	return cfg
}

// fakeBot is the bridge bot, and every ghost, as far as creating rooms and
// linking spaces goes: it hands out room IDs and records what was sent.
type fakeBot struct {
	bridgev2.MatrixAPI
	mu       sync.Mutex
	created  []createdRoom
	state    []sentState
	messages []sentMessage
}

type createdRoom struct {
	id  id.RoomID
	req *mautrix.ReqCreateRoom
}

type sentState struct {
	room    id.RoomID
	evtType event.Type
	key     string
	content *event.Content
}

type sentMessage struct {
	room    id.RoomID
	content *event.Content
}

func botOf(br *bridgev2.Bridge) *fakeBot { return br.Bot.(*fakeBot) }

func (*fakeBot) GetMXID() id.UserID   { return "@sipbot:example.com" }
func (*fakeBot) IsDoublePuppet() bool { return false }

func (b *fakeBot) CreateRoom(_ context.Context, req *mautrix.ReqCreateRoom) (id.RoomID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := id.RoomID(fmt.Sprintf("!created%d:example.com", len(b.created)+1))
	b.created = append(b.created, createdRoom{room, req})
	return room, nil
}

func (b *fakeBot) SendState(_ context.Context, room id.RoomID, evtType event.Type, key string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = append(b.state, sentState{room, evtType, key, content})
	return &mautrix.RespSendEvent{EventID: "$state"}, nil
}

func (b *fakeBot) SendMessage(_ context.Context, room id.RoomID, _ event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, sentMessage{room, content})
	return &mautrix.RespSendEvent{EventID: "$message"}, nil
}

func (*fakeBot) EnsureInvited(context.Context, id.RoomID, id.UserID) error { return nil }
func (*fakeBot) EnsureJoined(context.Context, id.RoomID, ...bridgev2.EnsureJoinedParams) error {
	return nil
}
func (*fakeBot) SetDisplayName(context.Context, string) error            { return nil }
func (*fakeBot) SetAvatarURL(context.Context, id.ContentURIString) error { return nil }
func (*fakeBot) SetExtraProfileMeta(context.Context, any) error          { return nil }

// spaceChildren is what space lists as its children now: the last
// m.space.child per child, created rooms' initial state included.
func (b *fakeBot) spaceChildren(space id.RoomID) map[id.RoomID]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[id.RoomID]bool{}
	for _, st := range b.state {
		if st.room != space || st.evtType != event.StateSpaceChild {
			continue
		}
		child := id.RoomID(st.key)
		if c, ok := st.content.Parsed.(*event.SpaceChildEventContent); ok && len(c.Via) > 0 {
			out[child] = true
		} else {
			delete(out, child)
		}
	}
	return out
}

// createdWith returns the request a room was created with.
func (b *fakeBot) createdWith(room id.RoomID) *mautrix.ReqCreateRoom {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.created {
		if c.id == room {
			return c.req
		}
	}
	return nil
}

// spaceStateCount counts the m.space.child and m.space.parent events sent.
func (b *fakeBot) spaceStateCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, st := range b.state {
		if st.evtType == event.StateSpaceChild || st.evtType == event.StateSpaceParent {
			n++
		}
	}
	return n
}
