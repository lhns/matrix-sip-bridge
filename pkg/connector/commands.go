package connector

import (
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
			Args:        "<_phone number_>",
		},
		RequiresLogin: true,
		Func: func(ce *commands.Event) {
			number := strings.TrimSpace(ce.RawArgs)
			if number == "" {
				ce.Reply("Usage: `$cmdprefix dial <phone number>`, in international format, e.g. `+15551234567`")
				return
			}
			if _, err := phonenum.Normalize(number); err != nil {
				ce.Reply("That is not a usable number: %v", err)
				return
			}
			call, err := sc.calls.DialNumber(ce.Ctx, number, ce.User.MXID)
			if err != nil {
				ce.Reply("Failed to place the call: %v", err)
				return
			}
			ce.Reply("Calling %s. Join the call in [the portal room](%s).",
				phonenum.NumberFromID(call.PortalID),
				call.RoomID.URI().MatrixToURL())
		},
	}
}
