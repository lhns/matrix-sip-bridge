package connector

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// dialRoomName is the name of a line's dial room.
func dialRoomName(line string) string {
	return line + " ☎"
}

// dialChatInfo describes a line's dial room: a plain room of the user's own,
// not a DM, since nobody is on the other end. Members stays nil, as for a
// space, so bridgev2 just invites the owner.
func dialChatInfo(line string) *bridgev2.ChatInfo {
	return &bridgev2.ChatInfo{
		Name:  ptr.Ptr(dialRoomName(line)),
		Topic: ptr.Ptr("Send a phone number to open its room on " + line + ", or `!dial <number>` to call it."),
		Type:  ptr.Ptr(database.RoomTypeDefault),
	}
}

// handleDialRoomMessage opens the user's room for the number they typed, on
// the dial room's line, and answers with a link to it. The message is
// recorded as handled either way: a reply says what went wrong.
func (sc *SIPClient) handleDialRoomMessage(ctx context.Context, msg *bridgev2.MatrixMessage, line string) (*bridgev2.MatrixMessageResponse, error) {
	typed := strings.TrimSpace(msg.Content.Body)
	var reply string
	if portalID, err := phonenum.IDFor(line, typed); err != nil {
		reply = fmt.Sprintf("`%s` is not a usable number: %v. Write it in international format, "+
			"e.g. `+15551234567`.", typed, err)
	} else if room, err := sc.conn.openPortal(ctx, portalID, sc.UserLogin.UserMXID); err != nil {
		reply = fmt.Sprintf("Failed to open the room for %s: %v", portalName(portalID), err)
	} else {
		reply = fmt.Sprintf("[%s](%s)", portalName(portalID), room.URI().MatrixToURL())
	}
	content := format.RenderMarkdown(reply, true, false)
	content.MsgType = event.MsgNotice
	if _, err := sc.UserLogin.Bridge.Bot.SendMessage(ctx, msg.Portal.MXID, event.EventMessage,
		&event.Content{Parsed: &content}, nil); err != nil {
		return nil, fmt.Errorf("reply in the dial room: %w", err)
	}
	return outboundResponse(), nil
}

// openPortal returns owner's room for a number, creating it and inviting them
// as needed. It is the call subsystem's, which already does exactly that for
// a call; tests replace it.
func (sc *SIPConnector) openPortal(ctx context.Context, portalID string, owner id.UserID) (id.RoomID, error) {
	if sc.openPortalFunc != nil {
		return sc.openPortalFunc(ctx, portalID, owner)
	}
	if sc.calls == nil {
		return "", fmt.Errorf("the bridge is not started")
	}
	portal, err := sc.calls.PortalRoom(ctx, portalID, owner)
	if err != nil {
		return "", err
	}
	return portal.MXID, nil
}

// ensureRoom returns this login's portal for a line room, creating its Matrix
// room if it has none.
func (sc *SIPClient) ensureRoom(ctx context.Context, portalID string) (*bridgev2.Portal, error) {
	login := sc.UserLogin
	portal, err := login.Bridge.GetPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID})
	if err != nil {
		return nil, fmt.Errorf("get portal: %w", err)
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ctx, login, nil); err != nil {
			return nil, fmt.Errorf("create room: %w", err)
		}
		// A room with no member list only joins a double puppet.
		login.MarkInPortal(ctx, portal)
	}
	return portal, nil
}
