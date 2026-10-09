package calls

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/bridgev2"
)

// Logins names the Matrix user a call or text is for. The connector owns the
// login scheme and the permission check; this package only asks.
type Logins interface {
	// Login returns the UserLogin of the Matrix user named by a recipient
	// header value, creating it if bridge.permissions allows that user to log
	// in. An empty recipient means the default recipient: the owner of the
	// bridge's shared login before per-user logins existed.
	Login(ctx context.Context, recipient string) (*bridgev2.UserLogin, error)
}

var (
	// ErrRecipientRefused is a recipient bridge.permissions gives no login.
	// The SIP answer is 403.
	ErrRecipientRefused = errors.New("recipient is not permitted to use the bridge")
	// ErrUnknownRecipient is a recipient that is not an MXID, or no recipient
	// with no default to fall back to. The SIP answer is 404.
	ErrUnknownRecipient = errors.New("no usable recipient")
)

// RecipientStatus maps a Logins error onto the SIP status that refuses the
// request, so calls and texts answer the same thing for the same refusal.
func RecipientStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrRecipientRefused):
		return 403, "Forbidden"
	case errors.Is(err, ErrUnknownRecipient):
		return 404, "Not Found"
	default:
		return 500, "Server Internal Error"
	}
}
