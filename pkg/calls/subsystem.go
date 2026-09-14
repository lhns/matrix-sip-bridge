package calls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/asteriskami"
	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// AsteriskCallConfig is the dialplan contract between the bridge and Asterisk.
// Every value is site-specific.
type AsteriskCallConfig struct {
	// ConferencePrefix is prepended to the portal ID to name the ConfBridge
	// that holds a call, e.g. prefix "sip-" gives "sip-15551234567".
	ConferencePrefix string `yaml:"conference_prefix"`
	// OutboundChannel is the Asterisk channel string used to place a call.
	// "{number}" is replaced with the E.164 destination including the plus.
	OutboundChannel string `yaml:"outbound_channel"`
	// Context, Extension and CallerID are where an originated call lands in the
	// dialplan. The extension is expected to drop the leg into the ConfBridge
	// named by the CONFBRIDGE_NAME channel variable.
	Context   string `yaml:"context"`
	Extension string `yaml:"extension"`
	CallerID  string `yaml:"caller_id"`
	// OriginateTimeout is how long Asterisk waits for the far end to answer.
	OriginateTimeout int `yaml:"originate_timeout"`
}

// Config configures the call subsystem.
type Config struct {
	Enabled  bool               `yaml:"enabled"`
	LiveKit  LiveKitConfig      `yaml:"livekit"`
	Asterisk AsteriskCallConfig `yaml:"asterisk"`
	// MembershipExpiry is the lifetime written into the ghost's RTC membership.
	// Clients treat an expired membership as gone, so it is refreshed at half
	// this interval for as long as the call lasts.
	MembershipExpiry time.Duration `yaml:"membership_expiry"`
	// RingTimeout is how long an inbound call waits for a Matrix user to join
	// the RTC session before the SIP leg is hung up.
	RingTimeout time.Duration `yaml:"ring_timeout"`
}

// Subsystem bridges calls. It is deliberately not a bridgev2 concept: bridgev2
// has no MSC3401 or LiveKit support, and its inbound event whitelist does not
// include call.member, so the only things borrowed from it are the ghosts and
// portals used as identity and room substrate.
type Subsystem struct {
	cfg Config
	br  *bridgev2.Bridge
	ami *asteriskami.Client
	lk  *LiveKitClient
	db  *database.Database
	log zerolog.Logger

	trunkID atomic.Pointer[string]

	// refreshers keeps the membership-refresh cancel func per call ID.
	refreshersMu sync.Mutex
	refreshers   map[string]context.CancelFunc
}

// New builds the subsystem. Start does the work.
func New(cfg Config, br *bridgev2.Bridge, ami *asteriskami.Client, db *database.Database, log zerolog.Logger) *Subsystem {
	if cfg.MembershipExpiry <= 0 {
		cfg.MembershipExpiry = 4 * time.Hour
	}
	if cfg.RingTimeout <= 0 {
		cfg.RingTimeout = 45 * time.Second
	}
	return &Subsystem{
		cfg:        cfg,
		br:         br,
		ami:        ami,
		lk:         NewLiveKitClient(cfg.LiveKit),
		db:         db,
		log:        log,
		refreshers: make(map[string]context.CancelFunc),
	}
}

// EventRegistrar is the slice of appservice.EventProcessor the subsystem
// needs. Taking it as an interface keeps bridgev2/matrix out of this package
// and makes the dependency testable.
type EventRegistrar interface {
	On(evtType event.Type, handler func(ctx context.Context, evt *event.Event))
}

// Start wires up the subsystem. It returns once everything is registered; the
// long-running parts run on ctx via Run.
func (s *Subsystem) Start(ctx context.Context, reg EventRegistrar) error {
	if !s.cfg.Enabled {
		s.log.Info().Msg("Call bridging is disabled")
		return nil
	}
	if err := s.db.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrade call tables: %w", err)
	}
	// Call state lives in Asterisk and LiveKit; this table is only a cache of
	// it, and after a restart every channel it names is gone.
	if err := s.db.Call.EndAll(ctx); err != nil {
		return fmt.Errorf("clear stale calls: %w", err)
	}

	s.ami.OnEvent(s.handleAMIEvent)

	// The whole design rests on this line. appservice.EventProcessor dispatches
	// on the exact event.Type struct including its Class, so an arbitrary state
	// type can be subscribed to even though bridgev2 does not know about it.
	// Decrypted events are re-dispatched through the same processor, so this
	// handler also sees anything that arrived encrypted.
	reg.On(CallMemberEventType, s.handleCallMember)
	return nil
}

// Run starts the background loops and blocks until ctx is done.
func (s *Subsystem) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	s.runTrunkReconciler(ctx)
}

// conferenceFor returns the ConfBridge name for a portal.
func (s *Subsystem) conferenceFor(portalID string) string {
	return s.cfg.Asterisk.ConferencePrefix + portalID
}

// portalIDFromConference is the inverse of conferenceFor. It returns false for
// a conference the bridge does not own, so unrelated ConfBridge activity on the
// same Asterisk is ignored.
func (s *Subsystem) portalIDFromConference(conference string) (string, bool) {
	prefix := s.cfg.Asterisk.ConferencePrefix
	if prefix == "" || !strings.HasPrefix(conference, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(conference, prefix)
	if rest == "" {
		return "", false
	}
	return rest, true
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handleAMIEvent is called on the AMI reader goroutine for every event.
func (s *Subsystem) handleAMIEvent(ctx context.Context, pkt *asteriskami.Packet) {
	if !asteriskami.IsConfbridgeEvent(pkt) {
		return
	}
	conference := pkt.Get("Conference")
	portalID, ok := s.portalIDFromConference(conference)
	if !ok {
		return
	}
	// The AMI reader must not block, so everything below runs detached. ctx is
	// the AMI session context, which is cancelled on shutdown.
	go func() {
		var err error
		switch {
		case strings.EqualFold(pkt.Event(), asteriskami.EventConfbridgeJoin):
			err = s.onConfbridgeJoin(ctx, portalID, conference, pkt)
		default:
			err = s.onConfbridgeGone(ctx, conference)
		}
		if err != nil && ctx.Err() == nil {
			s.log.Err(err).
				Str("event", pkt.Event()).
				Str("conference", conference).
				Msg("Failed to handle ConfBridge event")
		}
	}()
}

// onConfbridgeJoin reacts to a SIP leg landing in one of the bridge's
// conferences. For an inbound call this is the first the bridge hears of it.
func (s *Subsystem) onConfbridgeJoin(ctx context.Context, portalID, conference string, pkt *asteriskami.Packet) error {
	existing, err := s.db.Call.GetActiveByConference(ctx, conference)
	if err != nil {
		return fmt.Errorf("look up call: %w", err)
	}
	if existing != nil {
		// The outbound case: the bridge placed this call and is only learning
		// the channel name now.
		existing.Channel = pkt.Get("Channel")
		return s.db.Call.Update(ctx, existing)
	}

	portal, err := s.portalForNumber(ctx, portalID)
	if err != nil {
		return err
	}
	if portal.MXID == "" {
		return fmt.Errorf("portal %s has no Matrix room", portalID)
	}
	call := &database.Call{
		CallID:     newCallID(),
		PortalID:   portalID,
		RoomID:     portal.MXID,
		Direction:  database.DirectionInbound,
		Conference: conference,
		Channel:    pkt.Get("Channel"),
		LKRoom:     LiveKitRoomName(portal.MXID.String(), SlotRoom),
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(ctx, call); err != nil {
		return fmt.Errorf("insert call: %w", err)
	}
	s.log.Info().
		Str("call_id", call.CallID).
		Str("portal_id", portalID).
		Msg("Inbound call parked in ConfBridge, ringing Matrix")

	if err := s.publishGhostMembership(ctx, call); err != nil {
		return fmt.Errorf("publish ghost RTC membership: %w", err)
	}
	go s.expireRing(ctx, call.CallID)
	return nil
}

// expireRing hangs up an inbound call nobody answered. Without it the caller
// sits in a silent conference indefinitely, because Asterisk has answered the
// channel in order to park it.
func (s *Subsystem) expireRing(ctx context.Context, callID string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(s.cfg.RingTimeout):
	}
	call, err := s.db.Call.GetByID(ctx, callID)
	if err != nil || call == nil || call.State != database.StateRinging {
		return
	}
	s.log.Info().Str("call_id", callID).Msg("Nobody answered, hanging up")
	if call.Channel != "" {
		if err := s.ami.Hangup(ctx, call.Channel); err != nil {
			s.log.Warn().Err(err).Str("channel", call.Channel).Msg("Failed to hang up")
		}
	}
	if err := s.endCall(ctx, call); err != nil {
		s.log.Warn().Err(err).Str("call_id", callID).Msg("Failed to end call")
	}
}

// onConfbridgeGone reacts to a leave or a conference ending.
func (s *Subsystem) onConfbridgeGone(ctx context.Context, conference string) error {
	call, err := s.db.Call.GetActiveByConference(ctx, conference)
	if err != nil || call == nil {
		return err
	}
	s.log.Info().Str("call_id", call.CallID).Msg("SIP leg left the conference, ending call")
	return s.endCall(ctx, call)
}

// endCall retracts the ghost membership and marks the call ended. It is
// idempotent, because a hangup can be observed twice (leave, then end).
func (s *Subsystem) endCall(ctx context.Context, call *database.Call) error {
	if call.State == database.StateEnded {
		return nil
	}
	s.stopRefresh(call.CallID)
	call.State = database.StateEnded
	if err := s.db.Call.Update(ctx, call); err != nil {
		return err
	}
	return s.retractGhostMembership(ctx, call)
}

// ghostFor returns the ghost representing a phone number.
func (s *Subsystem) ghostFor(ctx context.Context, portalID string) (*bridgev2.Ghost, error) {
	return s.br.GetGhostByID(ctx, networkid.UserID(portalID))
}

// portalForNumber returns the portal for a number, creating the Matrix room if
// it does not exist yet.
func (s *Subsystem) portalForNumber(ctx context.Context, portalID string) (*bridgev2.Portal, error) {
	key := networkid.PortalKey{ID: networkid.PortalID(portalID)}
	portal, err := s.br.GetPortalByKey(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get portal %s: %w", portalID, err)
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ctx, nil, nil); err != nil {
			return nil, fmt.Errorf("create room for %s: %w", portalID, err)
		}
	}
	return portal, nil
}

// publishGhostMembership announces the caller as a participant of the room's
// RTC session, which is what makes Element ring.
func (s *Subsystem) publishGhostMembership(ctx context.Context, call *database.Call) error {
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return err
	}
	deviceID := "SIP" + strings.ToUpper(call.CallID[:8])
	membershipID := call.CallID
	call.LKIdentity = LiveKitIdentity(ghost.Intent.GetMXID().String(), deviceID, membershipID)
	if err := s.db.Call.Update(ctx, call); err != nil {
		return err
	}
	_, err = ghost.Intent.SendState(ctx, call.RoomID, CallMemberEventType,
		rtcStateKey(ghost.Intent.GetMXID(), deviceID),
		ghostMembership(deviceID, membershipID, s.cfg.MembershipExpiry), time.Time{})
	if err != nil {
		return err
	}
	s.startRefresh(ctx, call, deviceID, membershipID)
	return nil
}

// retractGhostMembership publishes the empty content that ends a membership.
// Matrix has no state deletion, so leaving is an empty state event.
func (s *Subsystem) retractGhostMembership(ctx context.Context, call *database.Call) error {
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return err
	}
	deviceID := "SIP" + strings.ToUpper(call.CallID[:8])
	_, err = ghost.Intent.SendState(ctx, call.RoomID, CallMemberEventType,
		rtcStateKey(ghost.Intent.GetMXID(), deviceID), leaveMembership(), time.Time{})
	return err
}

// startRefresh keeps the ghost membership from expiring mid-call.
func (s *Subsystem) startRefresh(ctx context.Context, call *database.Call, deviceID, membershipID string) {
	refreshCtx, cancel := context.WithCancel(ctx)
	s.refreshersMu.Lock()
	if old := s.refreshers[call.CallID]; old != nil {
		old()
	}
	s.refreshers[call.CallID] = cancel
	s.refreshersMu.Unlock()

	roomID, portalID, callID := call.RoomID, call.PortalID, call.CallID
	go func() {
		t := time.NewTicker(s.cfg.MembershipExpiry / 2)
		defer t.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-t.C:
				ghost, err := s.ghostFor(refreshCtx, portalID)
				if err != nil {
					return
				}
				_, err = ghost.Intent.SendState(refreshCtx, roomID, CallMemberEventType,
					rtcStateKey(ghost.Intent.GetMXID(), deviceID),
					ghostMembership(deviceID, membershipID, s.cfg.MembershipExpiry), time.Time{})
				if err != nil {
					s.log.Warn().Err(err).Str("call_id", callID).
						Msg("Failed to refresh ghost RTC membership")
				}
			}
		}
	}()
}

func (s *Subsystem) stopRefresh(callID string) {
	s.refreshersMu.Lock()
	defer s.refreshersMu.Unlock()
	if cancel := s.refreshers[callID]; cancel != nil {
		cancel()
		delete(s.refreshers, callID)
	}
}

// handleCallMember reacts to a Matrix user joining or leaving the RTC session
// of a portal room.
//
// This is the only inbound Matrix path the call subsystem has. bridgev2 never
// sees these events: they are not in its whitelist and there is no
// HandleMatrixStateEvent for a network connector to implement.
func (s *Subsystem) handleCallMember(ctx context.Context, evt *event.Event) {
	if evt.StateKey == nil {
		return
	}
	// Ignore the bridge's own ghosts, or the bridge would answer itself.
	if _, isGhost := s.br.Matrix.ParseGhostMXID(evt.Sender); isGhost {
		return
	}
	if evt.Sender == s.br.Bot.GetMXID() {
		return
	}
	portal, err := s.br.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil || portal == nil {
		return
	}
	content, active, err := ParseCallMember(evt.Content.VeryRaw)
	if err != nil {
		s.log.Warn().Err(err).
			Stringer("event_id", evt.ID).
			Msg("Failed to parse call.member content")
		return
	}
	log := s.log.With().
		Str("portal_id", string(portal.ID)).
		Stringer("sender", evt.Sender).
		Bool("active", active).
		Logger()

	if !active {
		s.onMatrixLeftCall(ctx, portal, log)
		return
	}
	if err := s.onMatrixJoinedCall(ctx, portal, content, log); err != nil {
		log.Err(err).Msg("Failed to handle Matrix RTC join")
	}
}

// onMatrixJoinedCall is the moment the bridge has been waiting for on an
// inbound call, and the trigger for an outbound one.
//
// The outbound half exists because Element X's native call button only writes
// this state event: there is no Matrix event that says "dial the phone", so a
// membership appearing in a portal room with no call in progress is read as a
// request to place one.
func (s *Subsystem) onMatrixJoinedCall(ctx context.Context, portal *bridgev2.Portal, content *CallMemberContent, log zerolog.Logger) error {
	portalID := string(portal.ID)
	call, err := s.db.Call.GetActiveByPortal(ctx, portalID)
	if err != nil {
		return fmt.Errorf("look up call: %w", err)
	}
	if call == nil {
		log.Info().Msg("RTC membership in a portal with no call, dialling out")
		_, err = s.Dial(ctx, portal)
		return err
	}
	if call.State != database.StateRinging {
		return nil
	}
	_ = content
	return s.bridgeMedia(ctx, call)
}

// onMatrixLeftCall hangs up when the last Matrix participant leaves.
//
// Only the ringing and bridged states are acted on: a leave event for a call
// that already ended is normal, because clients retract their membership after
// the far end hangs up.
func (s *Subsystem) onMatrixLeftCall(ctx context.Context, portal *bridgev2.Portal, log zerolog.Logger) {
	call, err := s.db.Call.GetActiveByPortal(ctx, string(portal.ID))
	if err != nil || call == nil {
		return
	}
	log.Info().Str("call_id", call.CallID).Msg("Matrix side left the call, hanging up")
	if call.Channel != "" {
		if err := s.ami.Hangup(ctx, call.Channel); err != nil {
			log.Warn().Err(err).Msg("Failed to hang up SIP leg")
		}
	}
	if err := s.endCall(ctx, call); err != nil {
		log.Warn().Err(err).Msg("Failed to end call")
	}
}

// bridgeMedia asks livekit-sip to dial the ConfBridge holding the SIP leg and
// join the LiveKit room backing the portal room's Element Call.
func (s *Subsystem) bridgeMedia(ctx context.Context, call *database.Call) error {
	trunkID, err := s.currentTrunkID()
	if err != nil {
		return err
	}
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return err
	}
	// livekit-sip dials the conference as if it were a phone number; the
	// dialplan is expected to route the conference name straight into
	// ConfBridge.
	participant, err := s.lk.CreateSIPParticipant(ctx, &CreateSIPParticipantRequest{
		SipTrunkID:          trunkID,
		SipCallTo:           call.Conference,
		RoomName:            call.LKRoom,
		ParticipantIdentity: call.LKIdentity,
		ParticipantName:     phonenum.FromID(call.PortalID),
		WaitUntilAnswered:   true,
	})
	if err != nil {
		return fmt.Errorf("create SIP participant: %w", err)
	}
	call.LKParticipant = participant.ParticipantID
	call.State = database.StateBridged
	s.log.Info().
		Str("call_id", call.CallID).
		Str("lk_room", call.LKRoom).
		Str("lk_participant", participant.ParticipantID).
		Stringer("ghost", ghost.Intent.GetMXID()).
		Msg("Media bridged into LiveKit")
	return s.db.Call.Update(ctx, call)
}

// Dial places an outbound call to the number a portal represents.
func (s *Subsystem) Dial(ctx context.Context, portal *bridgev2.Portal) (*database.Call, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("call bridging is disabled")
	}
	portalID := string(portal.ID)
	if portal.MXID == "" {
		return nil, fmt.Errorf("portal %s has no Matrix room", portalID)
	}
	if existing, err := s.db.Call.GetActiveByPortal(ctx, portalID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if s.cfg.Asterisk.OutboundChannel == "" {
		return nil, fmt.Errorf("asterisk outbound_channel is not configured")
	}

	conference := s.conferenceFor(portalID)
	call := &database.Call{
		CallID:     newCallID(),
		PortalID:   portalID,
		RoomID:     portal.MXID,
		Direction:  database.DirectionOutbound,
		Conference: conference,
		LKRoom:     LiveKitRoomName(portal.MXID.String(), SlotRoom),
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(ctx, call); err != nil {
		return nil, fmt.Errorf("insert call: %w", err)
	}

	channel := strings.ReplaceAll(s.cfg.Asterisk.OutboundChannel, "{number}", phonenum.FromID(portalID))
	err := s.ami.Originate(ctx, asteriskami.OriginateRequest{
		Channel:  channel,
		Context:  s.cfg.Asterisk.Context,
		Exten:    s.cfg.Asterisk.Extension,
		CallerID: s.cfg.Asterisk.CallerID,
		Timeout:  s.cfg.Asterisk.OriginateTimeout,
		Variables: map[string]string{
			"CONFBRIDGE_NAME": conference,
			"SIP_BRIDGE_CALL": call.CallID,
		},
	})
	if err != nil {
		call.State = database.StateEnded
		_ = s.db.Call.Update(ctx, call)
		return nil, fmt.Errorf("originate: %w", err)
	}

	// The ghost membership is published straight away rather than on answer, so
	// that the Matrix side has something to join while the phone rings.
	if err := s.publishGhostMembership(ctx, call); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Failed to publish ghost RTC membership for outbound call")
	}
	s.log.Info().
		Str("call_id", call.CallID).
		Str("portal_id", portalID).
		Msg("Outbound call originated")
	return call, nil
}

// DialNumber resolves a number to its portal and calls it.
func (s *Subsystem) DialNumber(ctx context.Context, number string) (*database.Call, error) {
	portalID, err := phonenum.NormalizeToID(number)
	if err != nil {
		return nil, err
	}
	portal, err := s.portalForNumber(ctx, portalID)
	if err != nil {
		return nil, err
	}
	return s.Dial(ctx, portal)
}

// RoomOf is a convenience for logging and tests.
func (s *Subsystem) RoomOf(call *database.Call) id.RoomID { return call.RoomID }
