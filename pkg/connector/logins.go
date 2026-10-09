package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

// legacyLoginID is the ID of the one shared login that existed before every
// Matrix user had their own. It is only read, by the migration.
const legacyLoginID networkid.UserLoginID = "sip"

// loginIDFor is the ID of a Matrix user's login. The MXID itself, so the
// recipient header value maps to a login without a lookup table.
func loginIDFor(mxid id.UserID) networkid.UserLoginID {
	return networkid.UserLoginID(mxid)
}

// keyDefaultRecipient is the bridgev2 KV key holding the MXID that texts and
// calls with no recipient header are for.
const keyDefaultRecipient = database.Key("sip_default_recipient")

// loginResolver implements calls.Logins: it turns the recipient a SIP request
// names into that person's UserLogin, creating it the first time.
type loginResolver struct {
	br *bridgev2.Bridge
	// mu serialises creation, so two requests naming the same new user cannot
	// both pass the existence check.
	mu sync.Mutex
}

var _ calls.Logins = (*loginResolver)(nil)

func (r *loginResolver) defaultRecipient(ctx context.Context) id.UserID {
	return id.UserID(r.br.DB.KV.Get(ctx, keyDefaultRecipient))
}

// Login implements calls.Logins.
func (r *loginResolver) Login(ctx context.Context, recipient string) (*bridgev2.UserLogin, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		def := r.defaultRecipient(ctx)
		if def == "" {
			return nil, fmt.Errorf("%w: no recipient header and no default recipient", calls.ErrUnknownRecipient)
		}
		recipient = string(def)
	}
	mxid := id.UserID(recipient)
	if !isUserID(mxid) {
		return nil, fmt.Errorf("%w: %q is not a Matrix user ID", calls.ErrUnknownRecipient, recipient)
	}
	login, created, err := r.ensure(ctx, mxid, true)
	if err != nil {
		return nil, err
	}
	if created {
		// bridgev2 connects logins only at startup and from its own login
		// flow, and this one is neither.
		login.Client.Connect(login.Log.WithContext(context.WithoutCancel(ctx)))
	}
	return login, nil
}

// isUserID reports whether mxid is a full Matrix user ID, with both halves.
func isUserID(mxid id.UserID) bool {
	localpart, homeserver, err := mxid.Parse()
	return err == nil && localpart != "" && homeserver != ""
}

// ensure returns mxid's login, creating it if there is none. checkPermission
// asks bridge.permissions for Login first; the migration skips it for the
// owner of the login that existed before.
func (r *loginResolver) ensure(ctx context.Context, mxid id.UserID, checkPermission bool) (login *bridgev2.UserLogin, created bool, err error) {
	loginID := loginIDFor(mxid)
	if login, err = r.br.GetExistingUserLoginByID(ctx, loginID); err != nil || login != nil {
		return login, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if login, err = r.br.GetExistingUserLoginByID(ctx, loginID); err != nil || login != nil {
		return login, false, err
	}
	// Before GetUserByMXID, which inserts a user row: a refused recipient
	// must leave nothing behind.
	if checkPermission && !r.br.Config.Permissions.Get(mxid).Login {
		return nil, false, fmt.Errorf("%w: %s", calls.ErrRecipientRefused, mxid)
	}
	user, err := r.br.GetUserByMXID(ctx, mxid)
	if err != nil {
		return nil, false, fmt.Errorf("get user %s: %w", mxid, err)
	}
	login, err = user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: "SIP",
	}, &bridgev2.NewLoginParams{DeleteOnConflict: false})
	if err != nil {
		return nil, false, fmt.Errorf("create login for %s: %w", mxid, err)
	}
	r.br.Log.Info().Stringer("user_id", mxid).Msg("Created a login for a recipient")
	return login, true, nil
}
