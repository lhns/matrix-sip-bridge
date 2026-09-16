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
	for _, portal := range portals {
		br.QueueRemoteEvent(sc.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: portal.PortalKey,
			},
			GetChatInfoFunc: sc.GetChatInfo,
		})
	}
}

func (sc *SIPClient) Disconnect() {}

func (sc *SIPClient) IsLoggedIn() bool { return true }

func (sc *SIPClient) LogoutRemote(_ context.Context) {}

// IsThisUser is never true: the bridge has no phone number of its own that
// appears as a ghost. The trunk's own number is config, not an identity.
func (sc *SIPClient) IsThisUser(_ context.Context, _ networkid.UserID) bool { return false }

func (sc *SIPClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{
		MaxTextLength: sc.conn.Config.Messages.MaxLength,
	}
}

// GetChatInfo describes a portal: the Matrix user and the one phone number.
func (sc *SIPClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return &bridgev2.ChatInfo{
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
				Events: calls.MembershipPowerLevels(),
			},
		},
	}, nil
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
	if err := sc.conn.sip.SendMessage(ctx, to, cfg.OutboundFrom, body); err != nil {
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
	msg := &inboundMessage{From: from, Body: raw.Body}
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
	msg, err := parseInboundMessage(inboundMessage(in))
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
	portalID := phonenum.ToID(msg.From)
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
