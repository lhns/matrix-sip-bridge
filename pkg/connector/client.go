package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
	"github.com/lhns/matrix-sip-bridge/pkg/siptransport"
)

// SIPClient is the NetworkAPI for the one shared SIP login.
type SIPClient struct {
	UserLogin *bridgev2.UserLogin
	conn      *SIPConnector
}

var (
	_ bridgev2.NetworkAPI                    = (*SIPClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*SIPClient)(nil)
	_ bridgev2.RoomNameHandlingNetworkAPI    = (*SIPClient)(nil)
)

// Connect reports the SIP endpoint's state to Matrix. The endpoint itself is
// owned by the connector, not by the login, because it is shared.
func (sc *SIPClient) Connect(ctx context.Context) {
	// The startup context is cancelled once Connect returns, and the resync
	// outlives it.
	go sc.resyncPortals(context.WithoutCancel(ctx))
	// Unconditionally, unlike every later report: this is the first state the
	// login ever has, so there is nothing for it to differ from. watchSIPHealth
	// keeps it current from here on.
	state, _ := sc.conn.health.SIP(sc.conn.sipReady())
	sc.UserLogin.BridgeState.Send(state)
}

// resyncPortals re-reads the chat info of every portal that already has a room.
//
// A portal's room type is only ever set from GetChatInfo, and nothing else in
// this bridge asks for it again after the room exists. Without this, a portal
// created before the bridge described itself as a DM stays a group room --
// and its calls stay group calls -- forever.
func (sc *SIPClient) resyncPortals(ctx context.Context) {
	br := sc.UserLogin.Bridge
	portals, err := br.GetAllPortalsWithMXID(ctx)
	if err != nil {
		br.Log.Warn().Err(err).Msg("Could not list portals to resync; existing rooms keep their old room type")
		return
	}
	log := br.Log.With().Str("action", "resync portals").Logger()
	for _, portal := range portals {
		// A resync is a remote event, and bridgev2 re-invites the login to
		// every portal a remote event touches (MarkInPortal); its "already in
		// there" cache is in memory, so every restart re-invites the user to
		// every room they have left. A room they are not in needs no room-type
		// repair anyway.
		if !shouldResyncPortal(ctx, br.Matrix, portal, sc.UserLogin.UserMXID, &log) {
			continue
		}
		br.QueueRemoteEvent(sc.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: portal.PortalKey,
			},
			GetChatInfoFunc: sc.GetChatInfo,
		})
	}
}

// memberLister is the one call shouldResyncPortal needs of the Matrix
// connector.
type memberLister interface {
	GetMembers(ctx context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error)
}

// shouldResyncPortal reports whether a portal room may be resynced at startup,
// which is true unless the user has demonstrably left it.
//
// This covers the restart path only. A real inbound call or text still
// re-invites the user to a room they left, which is wanted: otherwise they
// would silently miss it.
func shouldResyncPortal(ctx context.Context, mx memberLister, portal *bridgev2.Portal, userID id.UserID, log *zerolog.Logger) bool {
	// A line's space has no room type to repair -- bridgev2 refuses to change
	// one into or out of a space anyway -- and no members to sync. Its
	// children name it as their parent on their own resync.
	if portal.RoomType == database.RoomTypeSpace {
		return false
	}
	members, err := mx.GetMembers(ctx, portal.MXID)
	if err != nil {
		// Resync anyway. Skipping on a failed lookup would quietly stop the
		// room-type repair this function exists to do.
		log.Warn().Err(err).Stringer("room_id", portal.MXID).
			Msg("Could not read portal membership; resyncing it regardless")
		return true
	}
	member, ok := members[userID]
	return ok && member.Membership.IsInviteOrJoin()
}

func (sc *SIPClient) Disconnect() {}

func (sc *SIPClient) IsLoggedIn() bool { return true }

func (sc *SIPClient) LogoutRemote(_ context.Context) {}

// IsThisUser is never true: the bridge has no phone number of its own that
// appears as a ghost. The trunk's own number is config, not an identity.
func (sc *SIPClient) IsThisUser(_ context.Context, _ networkid.UserID) bool { return false }

func (sc *SIPClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	// A line's space holds rooms and carries no messages of its own.
	// HandleMatrixMessage refuses one regardless; this is what a client is
	// told beforehand.
	if phonenum.IsSpaceID(string(portal.ID)) {
		return &event.RoomFeatures{}
	}
	return &event.RoomFeatures{
		MaxTextLength: sc.conn.Config.Messages.MaxLength,
	}
}

// GetChatInfo describes a portal: the Matrix user and the one phone number,
// or -- for the one portal per line that is a space -- the line itself.
func (sc *SIPClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if line := phonenum.LineFromSpaceID(string(portal.ID)); line != "" {
		return keepUserName(portal, spaceChatInfo(line)), nil
	}
	info := &bridgev2.ChatInfo{
		Name: ptr.Ptr(portalName(string(portal.ID))),
		// A portal is one phone number and is inherently two-party. The room
		// type is what puts is_direct on the invite and the room in the user's
		// m.direct; a client that reads neither treats every call in it as a
		// group call.
		Type: ptr.Ptr(database.RoomTypeDM),
		Members: &bridgev2.ChatMemberList{
			// Not full, and saying otherwise kicks people. A portal's Matrix
			// side has members the SIP side knows nothing about -- the owning
			// user above all -- and bridgev2 removes every joined member a
			// full list omits, with the reason "User is not in remote chat".
			IsFull: false,
			// Names the DM partner outright. The alternative bridgev2 offers
			// is inferring it from a two-entry member list, which requires
			// IsFull.
			OtherUserID: networkid.UserID(portal.ID),
			Members: []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{Sender: networkid.UserID(portal.ID)},
				Membership:  event.MembershipJoin,
				PowerLevel:  ptr.Ptr(50),
			}},
			// The Matrix user is not in this list — bridgev2 leaves it at
			// users_default — so the call button has to be unlocked by
			// lowering the event instead of raising the user.
			PowerLevels: &bridgev2.PowerLevelOverrides{
				Events: portalEventPowerLevels(),
			},
		},
	}
	// The line's space, which bridgev2 creates as a parent portal if it does
	// not exist yet. A portal with no line stays parentless: there is no
	// default line, and a nil ParentID leaves the parent alone where an empty
	// one would unparent the room.
	if space, err := phonenum.SpaceIDFor(phonenum.LineFromID(string(portal.ID))); err == nil {
		info.ParentID = ptr.Ptr(networkid.PortalID(space))
	}
	return keepUserName(portal, info), nil
}

// portalEventPowerLevels is what a portal room lets a plain user send.
//
// Deliberately not folded into calls.MembershipPowerLevels: that set is the
// authoritative list of call-related events and is iterated elsewhere to
// decide whether a room's call power levels are already applied.
func portalEventPowerLevels() map[event.Type]int {
	levels := calls.MembershipPowerLevels()
	// Without this the room has state_default 50 and the user users_default 0,
	// so Element offers no rename at all.
	levels[event.StateRoomName] = 0
	return levels
}

// keepUserName drops the bridge's name from a description whose room a user
// has renamed.
//
// nil is the only value that means "leave the name alone".
// bridgev2.DefaultChatName instead clears it and hands naming to the ghost
// profile, which under private_chat_portal_meta renames the room to the bare
// number.
//
// The flag outlives a tombstone but NameSet does not, so the replacement room
// starts with no name at all rather than with the bridge's.
func keepUserName(portal *bridgev2.Portal, info *bridgev2.ChatInfo) *bridgev2.ChatInfo {
	if portalMeta(portal).NameSetByUser {
		info.Name = nil
	}
	return info
}

// HandleMatrixRoomName records that a Matrix user named this room, so that
// nothing the bridge later asserts -- a changed portalName format, a resync
// after a tombstone reset NameSet, or revert_failed_state_changes -- silently
// takes their name away again.
//
// Only a third party's rename reaches this: the Matrix connector drops state
// from the bridge bot and from ghosts, and anything carrying the double-puppet
// marker. Returning true is what makes bridgev2 persist the portal, so nothing
// here saves it.
func (sc *SIPClient) HandleMatrixRoomName(_ context.Context, msg *bridgev2.MatrixRoomName) (bool, error) {
	meta := portalMetaFor(msg.Portal)
	if msg.Content.Name == "" {
		// Element's rename dialog can submit an empty name, and it means
		// "give me the bridge's name back" rather than a room with no name.
		// NameSet false is what makes the resync below re-send a name the
		// portal already records; updateName early-returns on an equal name
		// that is already set.
		meta.NameSetByUser = false
		msg.Portal.Name = bridgeRoomName(string(msg.Portal.ID))
		msg.Portal.NameSet = false
		// In a goroutine: with PortalEventBuffer 0 a portal handles its events
		// inline under a lock this handler is already inside.
		go sc.resyncPortalName(msg.Portal)
		return true, nil
	}
	meta.NameSetByUser = true
	msg.Portal.Name = msg.Content.Name
	msg.Portal.NameSet = true
	return true, nil
}

// resyncPortalName re-asserts the bridge's name over a room that was reset.
// Without it the room keeps the empty name until something else resyncs the
// portal, which may be the next restart.
func (sc *SIPClient) resyncPortalName(portal *bridgev2.Portal) {
	if portal.MXID == "" {
		return
	}
	sc.UserLogin.Bridge.QueueRemoteEvent(sc.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatResync,
			PortalKey: portal.PortalKey,
		},
		GetChatInfoFunc: sc.GetChatInfo,
	})
}

// bridgeRoomName is the name the bridge gives a portal: the line for a line's
// space, the number for everything else.
func bridgeRoomName(portalID string) string {
	if line := phonenum.LineFromSpaceID(portalID); line != "" {
		return line
	}
	return portalName(portalID)
}

// spaceChatInfo describes a line's space, the parent every portal on that
// line names. A space is not a conversation: no ghost member, and not a DM,
// which would put is_direct on a room with nobody in it.
func spaceChatInfo(line string) *bridgev2.ChatInfo {
	return &bridgev2.ChatInfo{
		Name: ptr.Ptr(line),
		Type: ptr.Ptr(database.RoomTypeSpace),
		// Members stays nil: bridgev2 reads that as "the network said
		// nothing" and joins the owning user's double puppet, where a list
		// would be a membership sync over a room the SIP side knows nothing
		// about.
	}
}

// portalName names the room after the number, and after the line too when the
// portal ID carries one: the same person on two lines is two portal rooms, and
// with only the number in the name they are indistinguishable in the room
// list. The line is deliberately not in the ghost's display name -- a ghost is
// the person, who is not per-line.
func portalName(portalID string) string {
	line, _ := phonenum.SplitID(portalID)
	number := phonenum.NumberFromID(portalID)
	if line == "" {
		return number
	}
	return number + " (" + line + ")"
}

func (sc *SIPClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	number := phonenum.NumberFromID(string(ghost.ID))
	info := &bridgev2.UserInfo{Name: ptr.Ptr(number)}
	// A "tel:" URI is a global number. A short number that only means
	// something on its own line is not one, and publishing it as such offers
	// every client a dial link to somewhere else entirely.
	if phonenum.IsE164(number) {
		info.Identifiers = []string{"tel:" + number}
	}
	return info, nil
}

// ResolveIdentifier turns a typed phone number into a ghost and a portal.
func (sc *SIPClient) ResolveIdentifier(ctx context.Context, identifier string, _ bool) (*bridgev2.ResolveIdentifierResponse, error) {
	portalID, err := phonenum.NormalizeToID(identifier)
	if err != nil {
		return nil, err
	}
	userID := networkid.UserID(portalID)
	// Receiver is left empty: portals are shared between all Matrix users,
	// because there is only one trunk and one conversation per number.
	portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID)}

	ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get ghost: %w", err)
	}
	portal, err := sc.UserLogin.Bridge.GetPortalByKey(ctx, portalKey)
	if err != nil {
		return nil, fmt.Errorf("get portal: %w", err)
	}
	ghostInfo, _ := sc.GetUserInfo(ctx, ghost)
	portalInfo, _ := sc.GetChatInfo(ctx, portal)
	return &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: ghostInfo,
		Chat: &bridgev2.CreateChatResponse{
			Portal:     portal,
			PortalKey:  portalKey,
			PortalInfo: portalInfo,
		},
	}, nil
}

// HandleMatrixMessage sends a Matrix message out as a SIP MESSAGE.
func (sc *SIPClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	// A line's space is room organisation: its ID has no number half, so
	// NumberFromID would address the text to "+<line>-space".
	if phonenum.IsSpaceID(string(msg.Portal.ID)) {
		return nil, fmt.Errorf("a line's space carries no messages")
	}
	if !sc.conn.Config.Messages.Enabled {
		return nil, fmt.Errorf("messaging is disabled")
	}
	cfg := sc.conn.Config.Messages
	if cfg.OutboundTo == "" {
		return nil, fmt.Errorf("messages.outbound_to is not configured")
	}
	body := msg.Content.Body
	if body == "" {
		return nil, fmt.Errorf("only text messages can be sent as SIP MESSAGE")
	}
	number := phonenum.NumberFromID(string(msg.Portal.ID))
	to := strings.ReplaceAll(cfg.OutboundTo, "{number}", number)

	if sc.conn.sip == nil {
		return nil, fmt.Errorf("the SIP endpoint is not running")
	}
	// msg.Event.Sender, not the login: relay swaps the UserLogin a message is sent
	// through and leaves the event alone, so this is the real person in both modes.
	if err := sc.conn.sip.SendMessage(ctx, to, cfg.OutboundFrom, body, msg.Event.Sender); err != nil {
		return nil, err
	}
	// A 200 to a SIP MESSAGE carries no message identifier, so the bridge
	// mints one. It is only ever used for local deduplication: there is no
	// delivery report to correlate it with.
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:       networkid.MessageID("out-" + strconv.FormatInt(time.Now().UnixNano(), 36)),
			SenderID: networkid.UserID(LoginID),
		},
	}, nil
}

// inboundMessage carries the parsed fields of an inbound SIP MESSAGE.
type inboundMessage struct {
	From string
	To   string
	Body string
	// Line is the line the SIP server resolved the message to, empty for
	// none. From stays E.164 either way; everything downstream of it expects a
	// number, not a URI.
	Line string
}

// parseInboundMessage normalises the addresses of an inbound message.
//
// It returns an error rather than guessing when From is not a usable number:
// creating a portal keyed on a malformed identifier would strand the
// conversation in a room that can never be replied to.
func parseInboundMessage(raw inboundMessage) (*inboundMessage, error) {
	from, err := phonenum.Normalize(raw.From)
	if err != nil {
		return nil, fmt.Errorf("sender %q: %w", raw.From, err)
	}
	// The sender's host is the LINE, not the carrier's host: the SIP server
	// rewrites it on the call path and the message path alike, so this is what
	// puts a call and a text from the same person in one portal. Read before
	// Normalize, which keeps only the digits.
	msg := &inboundMessage{From: from, Body: raw.Body, Line: phonenum.LineFromURI(raw.From)}
	// The recipient is informational; a trunk that presents it oddly must not
	// stop the message being bridged.
	if to, err := phonenum.Normalize(raw.To); err == nil {
		msg.To = to
	}
	if msg.Body == "" {
		return nil, fmt.Errorf("message from %s has an empty body", from)
	}
	return msg, nil
}

// handleInboundMessage turns an inbound SIP MESSAGE into a Matrix message.
//
// Returning an error makes the transport answer the MESSAGE with a failure
// status, so the far end knows the text did not land.
func (sc *SIPConnector) handleInboundMessage(_ context.Context, in siptransport.InboundMessage) error {
	log := sc.br.Log.With().Str("component", "inbound sms").Logger()
	msg, err := parseInboundMessage(inboundMessage{From: in.From, To: in.To, Body: in.Body})
	if err != nil {
		log.Warn().Err(err).Msg("Ignoring unusable inbound message")
		// The message is unusable, not undelivered; answering with a failure
		// would only make the far end retry it. The 200 is also why this drop
		// is invisible from the SIP side and has to be counted here.
		sc.metrics.Message(metrics.DirectionInbound, metrics.MessageDroppedBadSender)
		return nil
	}
	login := sc.br.GetCachedUserLoginByID(networkid.UserLoginID(LoginID))
	if login == nil {
		log.Warn().Msg("Dropping inbound message: nobody has logged in yet")
		sc.metrics.Message(metrics.DirectionInbound, metrics.MessageDroppedNoLogin)
		return fmt.Errorf("no login yet")
	}
	// The same key the conference header gives a call from this person on this
	// line, so both land in one portal with one ghost. Without a line it is
	// ADR-0003's line-less key, as before.
	portalID, err := phonenum.IDFor(msg.Line, msg.From)
	if err != nil {
		log.Warn().Err(err).Str("line", msg.Line).Msg("Ignoring unusable inbound message")
		sc.metrics.Message(metrics.DirectionInbound, metrics.MessageDroppedBadSender)
		return nil
	}
	sc.br.QueueRemoteEvent(login, &simplevent.Message[*inboundMessage]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("from", msg.From)
			},
			PortalKey:    networkid.PortalKey{ID: networkid.PortalID(portalID)},
			CreatePortal: true,
			Sender:       bridgev2.EventSender{Sender: networkid.UserID(portalID)},
			Timestamp:    time.Now(),
		},
		Data: msg,
		// SIP MESSAGE has no message ID the far end would reuse, so the
		// timestamp is the only thing available to deduplicate on.
		ID:                 networkid.MessageID(fmt.Sprintf("in-%s-%d", portalID, time.Now().UnixNano())),
		ConvertMessageFunc: convertInboundMessage,
	})
	sc.metrics.Message(metrics.DirectionInbound, metrics.MessageBridged)
	return nil
}

func convertInboundMessage(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, data *inboundMessage) (*bridgev2.ConvertedMessage, error) {
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    data.Body,
			},
		}},
	}, nil
}
