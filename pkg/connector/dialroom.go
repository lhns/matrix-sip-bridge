package connector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
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

// keyLineMembers is the bridgev2 KV key holding the line_members of the last
// start, to tell who was removed.
const keyLineMembers = database.Key("sip_line_members")

// memberEntries flattens line_members into sorted "<line> <mxid>" entries.
func memberEntries(members map[string][]id.UserID) []string {
	var out []string
	for line, users := range members {
		for _, u := range users {
			out = append(out, line+" "+string(u))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ensureLineMemberLogins gives every permitted member of a line a login,
// before bridgev2 connects the logins; their rooms follow in Connect.
//
// A member who is gone from the config keeps their login and rooms. Removing
// them would delete rooms, so it is only logged.
func (sc *SIPConnector) ensureLineMemberLogins(ctx context.Context) {
	br := sc.br
	log := br.Log.With().Str("action", "line members").Logger()
	current := memberEntries(sc.Config.LineMembers)
	if previous := br.DB.KV.Get(ctx, keyLineMembers); previous != "" {
		for _, entry := range strings.Split(previous, "\n") {
			if !slices.Contains(current, entry) {
				line, user, _ := strings.Cut(entry, " ")
				log.Info().Str("line", line).Str("user_id", user).
					Msg("No longer a member of the line; their rooms on it are kept")
			}
		}
	}
	for _, entry := range current {
		line, user, _ := strings.Cut(entry, " ")
		mxid := id.UserID(user)
		mlog := log.With().Str("line", line).Stringer("user_id", mxid).Logger()
		if _, err := phonenum.SpaceIDFor(line); err != nil {
			mlog.Warn().Err(err).Msg("Skipping a line member: the line name is not usable")
			continue
		}
		if localpart, homeserver, err := mxid.Parse(); err != nil || localpart == "" || homeserver == "" {
			mlog.Warn().Msg("Skipping a line member that is not a Matrix user ID")
			continue
		}
		if _, _, err := sc.logins.ensure(ctx, mxid, true); errors.Is(err, calls.ErrRecipientRefused) {
			mlog.Warn().Msg("Skipping a line member that bridge.permissions does not let log in")
		} else if err != nil {
			mlog.Warn().Err(err).Msg("Could not create a line member's login")
		}
	}
	br.DB.KV.Set(ctx, keyLineMembers, strings.Join(current, "\n"))
}

// linesOf lists the lines user is a member of.
func (c *Config) linesOf(user id.UserID) []string {
	var lines []string
	for line, users := range c.LineMembers {
		if slices.Contains(users, user) {
			lines = append(lines, line)
		}
	}
	slices.Sort(lines)
	return lines
}

// ensureLineRooms creates the space and dial room of every line this login's
// user is a member of, and lists the space in their personal space. Existing
// rooms are not touched beyond that: re-inviting the user to a room they left
// on every restart is what resyncPortals avoids too.
func (sc *SIPClient) ensureLineRooms(ctx context.Context) {
	// The old shared login, kept while the migration could not finish, is the
	// owner's too; its rooms would be a second set.
	if sc.UserLogin.ID == legacyLoginID {
		return
	}
	log := sc.UserLogin.Log.With().Str("action", "ensure line rooms").Logger()
	ctx = log.WithContext(ctx)
	for _, line := range sc.conn.Config.linesOf(sc.UserLogin.UserMXID) {
		if err := sc.ensureLineRoom(ctx, line); err != nil {
			log.Warn().Err(err).Str("line", line).Msg("Could not set up the line's rooms")
		}
	}
}

func (sc *SIPClient) ensureLineRoom(ctx context.Context, line string) error {
	login := sc.UserLogin
	br := login.Bridge
	if sc.conn.Config.LineSpaces {
		spaceID, err := phonenum.SpaceIDFor(line)
		if err != nil {
			return err
		}
		space, err := br.GetPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(spaceID), Receiver: login.ID})
		if err != nil {
			return fmt.Errorf("get the space: %w", err)
		}
		if space.MXID == "" {
			if err := space.CreateMatrixRoom(ctx, login, nil); err != nil {
				return fmt.Errorf("create the space: %w", err)
			}
			// A room with no member list only joins a double puppet.
			login.MarkInPortal(ctx, space)
		}
		if err := ensureInPersonalSpace(ctx, login, space, false); err != nil {
			return fmt.Errorf("list the space in the personal space: %w", err)
		}
	}
	dialID, err := phonenum.DialIDFor(line)
	if err != nil {
		return err
	}
	dial, err := br.GetPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(dialID), Receiver: login.ID})
	if err != nil {
		return fmt.Errorf("get the dial room: %w", err)
	}
	if dial.MXID == "" {
		if err := dial.CreateMatrixRoom(ctx, login, nil); err != nil {
			return fmt.Errorf("create the dial room: %w", err)
		}
		login.MarkInPortal(ctx, dial)
	}
	return nil
}
