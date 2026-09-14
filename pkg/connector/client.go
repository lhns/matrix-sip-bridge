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
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/lhns/matrix-sip-bridge/pkg/asteriskami"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// SIPClient is the NetworkAPI for the one shared Asterisk login.
type SIPClient struct {
	UserLogin *bridgev2.UserLogin
	conn      *SIPConnector
}

var (
	_ bridgev2.NetworkAPI                    = (*SIPClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*SIPClient)(nil)
)

// Connect reports the AMI state to Matrix. The connection itself is owned by
// the connector, not by the login, because it is shared.
func (sc *SIPClient) Connect(_ context.Context) {
	if sc.conn.ami.Connected() {
		sc.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		return
	}
	sc.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnecting,
		Message:    "Connecting to Asterisk",
	})
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
	number := phonenum.FromID(string(portal.ID))
	return &bridgev2.ChatInfo{
		Name: ptr.Ptr(number),
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{Sender: networkid.UserID(portal.ID)},
				Membership:  event.MembershipJoin,
				PowerLevel:  ptr.Ptr(50),
			}},
		},
	}, nil
}

func (sc *SIPClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	number := phonenum.FromID(string(ghost.ID))
	return &bridgev2.UserInfo{
		Identifiers: []string{"tel:" + number},
		Name:        ptr.Ptr(number),
	}, nil
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
	number := phonenum.FromID(string(msg.Portal.ID))
	to := strings.ReplaceAll(cfg.OutboundTo, "{number}", number)

	if err := sc.conn.ami.MessageSend(ctx, to, cfg.OutboundFrom, body); err != nil {
		return nil, err
	}
	// AMI's MessageSend reply carries no message identifier, so the bridge
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

// parseInboundMessage reads the dialplan UserEvent that carries an inbound SIP
// MESSAGE and normalises the sender.
//
// It returns an error rather than guessing when From is not a usable number:
// creating a portal keyed on a malformed identifier would strand the
// conversation in a room that can never be replied to.
func parseInboundMessage(pkt *asteriskami.Packet) (*inboundMessage, error) {
	from, err := phonenum.Normalize(pkt.Get("From"))
	if err != nil {
		return nil, fmt.Errorf("sender %q: %w", pkt.Get("From"), err)
	}
	msg := &inboundMessage{From: from, Body: pkt.Get("Body")}
	// The recipient is informational; a trunk that presents it oddly must not
	// stop the message being bridged.
	if to, err := phonenum.Normalize(pkt.Get("To")); err == nil {
		msg.To = to
	}
	if msg.Body == "" {
		return nil, fmt.Errorf("message from %s has an empty body", from)
	}
	return msg, nil
}

// handleAMIMessage turns a dialplan UserEvent into a Matrix message.
func (sc *SIPConnector) handleAMIMessage(_ context.Context, pkt *asteriskami.Packet) {
	if !strings.EqualFold(pkt.UserEventName(), sc.Config.Messages.UserEvent) {
		return
	}
	log := sc.br.Log.With().Str("component", "inbound sms").Logger()
	msg, err := parseInboundMessage(pkt)
	if err != nil {
		log.Warn().Err(err).Msg("Ignoring unusable inbound message")
		return
	}
	login := sc.br.GetCachedUserLoginByID(networkid.UserLoginID(LoginID))
	if login == nil {
		log.Warn().Msg("Dropping inbound message: nobody has logged in yet")
		return
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
