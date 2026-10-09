package connector

import (
	"context"
	"errors"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	sipdb "github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// helpSectionCalls groups the bridge's own commands in the help output.
var helpSectionCalls = commands.HelpSection{Name: "Calls", Order: 25}

// dialCommand ("call", alias "dial") places an outbound call.
//
// It exists because Element's native call button cannot start a call in a room
// that does not exist yet: there is nothing to press. Once the portal room is
// there, the button works and this command is only a convenience.
func (sc *SIPConnector) dialCommand() *commands.FullHandler {
	return &commands.FullHandler{
		Name:    "call",
		Aliases: []string{"dial"},
		Help: commands.HelpMeta{
			Section:     helpSectionCalls,
			Description: "Call a phone number, creating its portal room if needed",
			Args:        "<_phone number_> [_line_]",
		},
		// Not RequiresLogin: a permitted user gets their login on first use,
		// as they would from a text, and a refused one is told so below.
		RequiresLogin: false,
		Func: func(ce *commands.Event) {
			var dialLine string
			if ce.Portal != nil {
				dialLine = phonenum.LineFromDialID(string(ce.Portal.ID))
			}
			sc.runDial(ce.Ctx, ce.User.MXID, ce.RawArgs, dialLine, func(msg string, args ...any) {
				ce.Reply(msg, args...)
			})
		},
	}
}

// runDial is the body of the dial command, shared with a dial room, where the
// command is also read without the bridge's command prefix. dialLine is the
// line of the dial room it is said in, if any. Replies may use $cmdprefix.
func (sc *SIPConnector) runDial(ctx context.Context, user id.UserID, rawArgs, dialLine string,
	reply func(msg string, args ...any)) {
	if strings.TrimSpace(rawArgs) == "" {
		reply("Usage: `$cmdprefix call <phone number> [line]`, the number in international " +
			"format, e.g. `+15551234567 home`. Without a line the call joins the room the " +
			"number already has; a line is needed for a number with rooms on several. In a " +
			"line's dial room the line is that one.")
		return
	}
	number, line := splitDialArgs(rawArgs)
	if line == "" {
		line = dialLine
	}
	if _, err := sc.logins.Login(ctx, string(user)); errors.Is(err, calls.ErrRecipientRefused) {
		reply("You are not permitted to use this bridge.")
		return
	} else if err != nil {
		reply("Failed to set up your login: %v", err)
		return
	}
	call, err := sc.dialNumber(ctx, number, line, user)
	var ambiguous *calls.AmbiguousNumberError
	switch {
	case errors.As(err, &ambiguous):
		reply("%s already has a room on the lines `%s`. Say which one: "+
			"`$cmdprefix call %s <line>`.",
			ambiguous.Number, strings.Join(ambiguous.Lines, "`, `"), ambiguous.Number)
	case errors.Is(err, phonenum.ErrBadLine):
		// Never fall back to a line-less call: that is a different
		// portal room, and the user asked for this line.
		reply("`%s` is not a usable line name. Letters, digits, dashes and "+
			"underscores only, starting with a letter or digit — no dots, which is "+
			"what tells a line from a trunk hostname.", line)
	case errors.Is(err, phonenum.ErrNotE164), errors.Is(err, phonenum.ErrEmpty):
		reply("`%s` is not a usable number: %v", number, err)
	case err != nil && line == "":
		reply("Failed to place the call: %v", err)
	case err != nil:
		reply("Failed to place the call on line `%s`: %v", line, err)
	default:
		where := phonenum.NumberFromID(call.PortalID)
		if got := phonenum.LineFromID(call.PortalID); got != "" {
			where += " on line " + got
		}
		reply("Calling %s. Join the call in [the portal room](%s).",
			where, call.RoomID.URI().MatrixToURL())
	}
}

// dialNumber is the call subsystem's DialNumber; tests replace it.
func (sc *SIPConnector) dialNumber(ctx context.Context, number, line string, caller id.UserID) (*sipdb.Call, error) {
	if sc.dialNumberFunc != nil {
		return sc.dialNumberFunc(ctx, number, line, caller)
	}
	return sc.calls.DialNumber(ctx, number, line, caller)
}

// splitDialArgs reads "<number> [line]" off the raw argument string.
//
// The line is the last word, and only when what is left is still a number:
// Normalize tolerates a number written with spaces, so "+1 555 123 4567" must
// not read its last group as a line name. The one ambiguity that leaves is an
// all-digit line name, which is swallowed into the number instead -- give a
// line a name with a letter in it.
func splitDialArgs(raw string) (number, line string) {
	fields := strings.Fields(raw)
	joined := strings.Join(fields, "")
	if _, err := phonenum.Normalize(joined); err == nil || len(fields) < 2 {
		return joined, ""
	}
	return strings.Join(fields[:len(fields)-1], ""), fields[len(fields)-1]
}
