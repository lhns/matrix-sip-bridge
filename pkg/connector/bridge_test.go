package connector

import (
	"context"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

// stubMatrix is the Matrix connector reduced to what NewBridge, a login and a
// portal re-key touch. Anything else panics on the nil interface, which is the
// signal that a test has outgrown it.
type stubMatrix struct {
	bridgev2.MatrixConnector
}

func (stubMatrix) Init(*bridgev2.Bridge)         {}
func (stubMatrix) BotIntent() bridgev2.MatrixAPI { return nil }
func (stubMatrix) ServerName() string            { return "example.com" }
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
		stubMatrix{}, testNetwork{sc}, func(*bridgev2.Bridge) bridgev2.CommandProcessor { return nil })
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
