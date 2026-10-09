package connector

import (
	"context"
	"errors"
	"testing"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

func TestResolverUsesTheDefaultRecipientForNoHeader(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	ctx := context.Background()
	if _, err := sc.logins.Login(ctx, ""); !errors.Is(err, calls.ErrUnknownRecipient) {
		t.Fatalf("no default: err = %v, want ErrUnknownRecipient", err)
	}
	br.DB.KV.Set(ctx, keyDefaultRecipient, string(testOwner))
	login, err := sc.logins.Login(ctx, " ")
	if err != nil {
		t.Fatal(err)
	}
	if login.ID != loginIDFor(testOwner) || login.UserMXID != testOwner {
		t.Errorf("login = %s for %s, want the default recipient's own", login.ID, login.UserMXID)
	}
}

func TestResolverCreatesAPermittedUsersLoginOnceAndReusesIt(t *testing.T) {
	br, sc := newTestBridge(t, ownerPermissions())
	ctx := context.Background()
	first, err := sc.logins.Login(ctx, " @alice:example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if br.GetCachedUserLoginByID(loginIDFor(testOwner)) != first {
		t.Error("the new login is not the cached one")
	}
	second, err := sc.logins.Login(ctx, string(testOwner))
	if err != nil || second != first {
		t.Errorf("second lookup = %v, %v; want the same login", second, err)
	}
}

// A refused recipient must leave nothing behind: GetUserByMXID inserts a user
// row, so the permission check has to come before it.
func TestResolverRefusesWithoutLoginPermissionAndCreatesNoUser(t *testing.T) {
	br, sc := newTestBridge(t, bridgeconfig.PermissionConfig{
		"@bob:example.com": &bridgeconfig.Permissions{SendEvents: true, Commands: true},
	})
	_, err := sc.logins.Login(context.Background(), "@bob:example.com")
	if !errors.Is(err, calls.ErrRecipientRefused) {
		t.Fatalf("err = %v, want ErrRecipientRefused", err)
	}
	if n := count(t, br, `SELECT COUNT(*) FROM "user"`); n != 0 {
		t.Errorf("users = %d, want none created for a refused recipient", n)
	}
	if code, _ := calls.RecipientStatus(err); code != 403 {
		t.Errorf("status = %d, want 403", code)
	}
	// Not listed at all is the same refusal.
	if _, err := sc.logins.Login(context.Background(), "@carol:example.org"); !errors.Is(err, calls.ErrRecipientRefused) {
		t.Errorf("unlisted user: err = %v, want ErrRecipientRefused", err)
	}
}

func TestResolverRejectsWhatIsNotAnMXID(t *testing.T) {
	_, sc := newTestBridge(t, ownerPermissions())
	for _, in := range []string{"alice", "@alice", "alice:example.com", "sip:alice@example.com", "@:example.com"} {
		if _, err := sc.logins.Login(context.Background(), in); !errors.Is(err, calls.ErrUnknownRecipient) {
			t.Errorf("%q: err = %v, want ErrUnknownRecipient", in, err)
		}
	}
}
