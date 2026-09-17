package connector

import (
	"errors"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// helpSectionCalls groups the bridge's own commands in the help output.
var helpSectionCalls = commands.HelpSection{Name: "Calls", Order: 25}

// dialCommand places an outbound call.
//
// It exists because Element's native call button cannot start a call in a room
// that does not exist yet: there is nothing to press. Once the portal room is
// there, the button works and this command is only a convenience.
func (sc *SIPConnector) dialCommand() *commands.FullHandler {
	return &commands.FullHandler{
		Name:    "dial",
		Aliases: []string{"call"},
		Help: commands.HelpMeta{
			Section:     helpSectionCalls,
			Description: "Call a phone number, creating its portal room if needed",
			Args:        "<_phone number_> [_line_]",
		},
		RequiresLogin: true,
		Func: func(ce *commands.Event) {
			if strings.TrimSpace(ce.RawArgs) == "" {
				ce.Reply("Usage: `$cmdprefix dial <phone number> [line]`, the number in international " +
					"format, e.g. `+15551234567 home`. Without a line the call gets the number's " +
					"line-less room, not the one that line's inbound calls land in.")
				return
			}
			number, line := splitDialArgs(ce.RawArgs)
			call, err := sc.calls.DialNumber(ce.Ctx, number, line, ce.User.MXID)
			switch {
			case errors.Is(err, phonenum.ErrBadLine):
				// Never fall back to a line-less call: that is a different
				// portal room, and the user asked for this line.
				ce.Reply("`%s` is not a usable line name. Letters, digits, dashes and "+
					"underscores only, starting with a letter or digit — no dots, which is "+
					"what tells a line from a trunk hostname.", line)
			case errors.Is(err, phonenum.ErrNotE164), errors.Is(err, phonenum.ErrEmpty):
				ce.Reply("`%s` is not a usable number: %v", number, err)
			case err != nil && line == "":
				ce.Reply("Failed to place the call: %v", err)
			case err != nil:
				ce.Reply("Failed to place the call on line `%s`: %v", line, err)
			default:
				where := phonenum.NumberFromID(call.PortalID)
				if got := phonenum.LineFromID(call.PortalID); got != "" {
					where += " on line " + got
				}
				ce.Reply("Calling %s. Join the call in [the portal room](%s).",
					where, call.RoomID.URI().MatrixToURL())
			}
		},
	}
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
