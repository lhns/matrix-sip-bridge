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

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// InboundLeg is the control leg of an inbound call as the SIP transport
// presents it.
//
// It is not the call, and its end is not the call ending: the dialplan hangs
// it up moments after it is answered so the caller falls through into the
// conference.
type InboundLeg interface {
	// From is the caller's URI, for logging only. The conference is what
	// identifies the conversation.
	From() string
	// Conference is the conference the caller will land in.
	Conference() string
	Ringing() error
	Answer() error
	Reject(code int, reason string) error
	// Done is closed when the leg is gone, by BYE, CANCEL or failure.
	Done() <-chan struct{}
}

// OutboundLeg is the control leg of a call the bridge asked the SIP server to
// place.
type OutboundLeg interface {
	Hangup(ctx context.Context) error
	Done() <-chan struct{}
}

// Telephony is the SIP transport as the call subsystem uses it.
//
// Only outbound INVITE is needed here; inbound calls arrive through
// HandleInboundCall, which the transport's INVITE handler calls.
type Telephony interface {
	Invite(ctx context.Context, to, conference string) (OutboundLeg, error)
}

// Config configures the call subsystem.
type Config struct {
	Enabled bool          `yaml:"enabled"`
	LiveKit LiveKitConfig `yaml:"livekit"`

	// ConferencePrefix is prepended to the portal ID to name the conference
	// holding a call, e.g. prefix "sip-" gives "sip-15551234567". It is also
	// how the bridge recognises a conference as its own and ignores the rest.
	ConferencePrefix string `yaml:"conference_prefix"`
	// OutboundURI is the SIP URI an outbound control leg is sent to.
	// "{number}" is replaced with the E.164 destination including the plus.
	OutboundURI string `yaml:"outbound_uri"`

	// MembershipExpiry is the lifetime written into the ghost's RTC
	// membership. It is not a liveness signal and is never refreshed: the
	// membership is published when a call starts and retracted when it ends,
	// so this only bounds how long a membership left behind by a crashed
	// bridge lingers before clients discard it.
	MembershipExpiry time.Duration `yaml:"membership_expiry"`
	// RingTimeout is how long an inbound call waits for a Matrix user to join
	// the RTC session before the leg is declined.
	RingTimeout time.Duration `yaml:"ring_timeout"`
	// ParticipantPollInterval is how often LiveKit is asked whether the SIP
	// participant is still in the room. That is the only signal the bridge has
	// that a call ended; see runParticipantWatcher.
	ParticipantPollInterval time.Duration `yaml:"participant_poll_interval"`
}

// Subsystem bridges calls. It is deliberately not a bridgev2 concept: bridgev2
// has no MSC3401 or LiveKit support, and its inbound event whitelist does not
// include call.member, so the only things borrowed from it are the ghosts and
// portals used as identity and room substrate.
type Subsystem struct {
	cfg Config
	br  *bridgev2.Bridge
	sip Telephony
	lk  *LiveKitClient
	db  *database.Database
	log zerolog.Logger

	// loginID names the bridge's one static UserLogin. Every portal bridgev2
	// creates needs it as the source; there is no per-user login to take it
	// from.
	loginID networkid.UserLoginID

	trunkID atomic.Pointer[string]

	// trunkPushed is the fingerprint of the trunk spec this process last wrote
	// to LiveKit. It exists so that a deployment which does not return
	// auth_password from List is not rewritten on every reconcile tick.
	trunkPushed atomic.Pointer[string]

	// answered carries the "a Matrix user joined" signal from the call.member
	// handler to the goroutine holding an inbound leg open.
	answeredMu sync.Mutex
	answered   map[string]chan struct{}

	// declined carries the "a Matrix user rejected the call" signal, and
	// notifies names the ring notification each ringing call sent, so a
	// decline can be matched to the call it refers to rather than to whatever
	// is ringing in the room.
	declinedMu sync.Mutex
	declined   map[string]chan struct{}
	notifies   map[id.EventID]string

	// legs keeps the control leg of a call that still has one, so the bridge
	// can hang it up.
	legsMu sync.Mutex
	legs   map[string]OutboundLeg

	// seen records that a call's LiveKit participant has been observed at
	// least once, so that "not in the room" means "left" rather than "not
	// joined yet". Teardown depends on the difference.
	seenMu sync.Mutex
	seen   map[string]bool
}

// New builds the subsystem. Start does the work.
func New(cfg Config, br *bridgev2.Bridge, loginID networkid.UserLoginID, sip Telephony, db *database.Database, log zerolog.Logger) *Subsystem {
	if cfg.MembershipExpiry <= 0 {
		cfg.MembershipExpiry = 6 * time.Hour
	}
	if cfg.RingTimeout <= 0 {
		cfg.RingTimeout = 45 * time.Second
	}
	if cfg.ParticipantPollInterval <= 0 {
		cfg.ParticipantPollInterval = 10 * time.Second
	}
	return &Subsystem{
		cfg:      cfg,
		br:       br,
		loginID:  loginID,
		sip:      sip,
		lk:       NewLiveKitClient(cfg.LiveKit),
		db:       db,
		log:      log,
		answered: make(map[string]chan struct{}),
		declined: make(map[string]chan struct{}),
		notifies: make(map[id.EventID]string),
		legs:     make(map[string]OutboundLeg),
		seen:     make(map[string]bool),
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
	// The rows are read before they are cleared, because they name the RTC
	// memberships a previous run may have left behind. See CallQuery.EndAll
	// for why a restart clears them at all.
	stale, err := s.db.Call.GetAllActive(ctx)
	if err != nil {
		return fmt.Errorf("list calls from the last run: %w", err)
	}
	if err := s.db.Call.EndAll(ctx); err != nil {
		return fmt.Errorf("clear stale calls: %w", err)
	}
	s.sweepStaleMemberships(ctx, stale)

	// The whole design rests on this line. appservice.EventProcessor dispatches
	// on the exact event.Type struct including its Class, so an arbitrary state
	// type can be subscribed to even though bridgev2 does not know about it.
	// Decrypted events are re-dispatched through the same processor, so this
	// handler also sees anything that arrived encrypted.
	reg.On(CallMemberEventType, s.handleCallMember)
	// Without this the bridge cannot tell a rejected call from an unanswered
	// one, and a caller the user declined keeps ringing until RingTimeout.
	reg.On(RtcDeclineEventType, s.handleRtcDecline)
	reg.On(RtcDeclineStableEventType, s.handleRtcDecline)
	return nil
}

// Run starts the background loops and blocks until ctx is done.
func (s *Subsystem) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	go s.runParticipantWatcher(ctx)
	s.runTrunkReconciler(ctx)
}

// conferenceFor returns the conference name for a portal.
func (s *Subsystem) conferenceFor(portalID string) string {
	return s.cfg.ConferencePrefix + portalID
}

// portalIDFromConference is the inverse of conferenceFor. It returns false for
// a conference the bridge does not own, so a call routed to the bridge by
// mistake creates no portal room.
func (s *Subsystem) portalIDFromConference(conference string) (string, bool) {
	prefix := s.cfg.ConferencePrefix
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

// HandleInboundCall owns an inbound control leg for its whole life.
//
// The order is fixed: 180 so the branch stays alive, the media in place, and
// 200 only once a Matrix user has joined. See InboundCall.Answer for why the
// last of those must never happen speculatively.
func (s *Subsystem) HandleInboundCall(ctx context.Context, leg InboundLeg) {
	if !s.cfg.Enabled {
		_ = leg.Reject(503, "Service Unavailable")
		return
	}
	conference := leg.Conference()
	portalID, ok := s.portalIDFromConference(conference)
	if !ok {
		s.log.Warn().
			Str("conference", conference).
			Str("from", leg.From()).
			Msg("INVITE without a recognisable conference header, declining")
		_ = leg.Reject(404, "Not Found")
		return
	}
	log := s.log.With().Str("portal_id", portalID).Logger()

	call, err := s.beginInboundCall(ctx, portalID, conference)
	if err != nil {
		log.Err(err).Msg("Failed to start inbound call")
		_ = leg.Reject(500, "Server Internal Error")
		return
	}
	log = log.With().Str("call_id", call.CallID).Logger()

	if err := leg.Ringing(); err != nil {
		log.Err(err).Msg("Failed to send 180 Ringing")
		// A branch of a parallel Dial() that never gets a final response is
		// held until the transaction times out, so every failure here ends
		// with a definitive status rather than a dangling leg.
		_ = leg.Reject(500, "Server Internal Error")
		_ = s.endCall(ctx, call)
		return
	}
	// The media is put in place while the phone is still ringing: the LiveKit
	// participant has to be in the room with a matching identity before a
	// Matrix client will render it, and doing it on answer would add that
	// latency to the moment the call connects.
	if err := s.bridgeMedia(ctx, call); err != nil {
		log.Err(err).Msg("Failed to put the call into LiveKit")
		_ = leg.Reject(503, "Service Unavailable")
		_ = s.endCall(ctx, call)
		return
	}
	s.waitForMatrix(ctx, call, leg, log)
}

// beginInboundCall creates the portal room, the call row and the ghost's RTC
// membership, which is what makes Matrix ring.
func (s *Subsystem) beginInboundCall(ctx context.Context, portalID, conference string) (*database.Call, error) {
	if existing, err := s.db.Call.GetActiveByConference(ctx, conference); err != nil {
		return nil, fmt.Errorf("look up call: %w", err)
	} else if existing != nil {
		return nil, fmt.Errorf("conference %s already has call %s in progress", conference, existing.CallID)
	}
	portal, err := s.portalForNumber(ctx, portalID)
	if err != nil {
		return nil, err
	}
	if portal.MXID == "" {
		return nil, fmt.Errorf("portal %s has no Matrix room", portalID)
	}
	call := &database.Call{
		CallID:     newCallID(),
		PortalID:   portalID,
		RoomID:     portal.MXID,
		Direction:  database.DirectionInbound,
		Conference: conference,
		LKRoom:     LiveKitRoomName(portal.MXID.String(), SlotRoom),
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(ctx, call); err != nil {
		return nil, fmt.Errorf("insert call: %w", err)
	}
	membership, err := s.publishGhostMembership(ctx, call)
	if err != nil {
		return nil, fmt.Errorf("publish ghost RTC membership: %w", err)
	}
	// A failed notification is not a failed call: the membership alone still
	// lets someone who opens the room join it, which is better than declining
	// a caller that could have been answered.
	if notify, err := s.publishRingNotification(ctx, call, membership); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Failed to send the ring notification; Matrix clients will not ring for this call")
	} else {
		s.rememberNotification(notify, call.CallID)
	}
	s.log.Info().
		Str("call_id", call.CallID).
		Str("portal_id", portalID).
		Msg("Inbound call ringing Matrix")
	return call, nil
}

// waitForMatrix holds the control leg open until a Matrix user joins the RTC
// session, the caller gives up, or the ring times out.
func (s *Subsystem) waitForMatrix(ctx context.Context, call *database.Call, leg InboundLeg, log zerolog.Logger) {
	joined := s.answerChannel(call.CallID)
	defer s.forgetAnswerChannel(call.CallID)
	declined := s.declineChannel(call.CallID)
	defer s.forgetDeclineChannel(call.CallID)

	select {
	case <-joined:
	case <-declined:
		log.Info().Msg("Matrix declined the call")
		// 486 rather than 480: the difference is what the caller's network
		// plays them, and a rejection is not an absence.
		_ = leg.Reject(486, "Busy Here")
		_ = s.endCall(ctx, call)
		return
	case <-leg.Done():
		log.Info().Msg("Caller hung up before Matrix answered")
		_ = s.endCall(ctx, call)
		return
	case <-time.After(s.cfg.RingTimeout):
		log.Info().Msg("Nobody answered in Matrix, declining the call")
		_ = leg.Reject(480, "Temporarily Unavailable")
		_ = s.endCall(ctx, call)
		return
	case <-ctx.Done():
		_ = leg.Reject(503, "Service Unavailable")
		return
	}

	if err := leg.Answer(); err != nil {
		log.Err(err).Msg("Failed to answer the call")
		_ = s.endCall(ctx, call)
		return
	}
	call.State = database.StateBridged
	if err := s.db.Call.Update(ctx, call); err != nil {
		log.Err(err).Msg("Failed to record the answered call")
	}
	log.Info().Msg("Call answered; the dialplan now moves the caller into the conference")

	// The end of this leg is expected and says nothing about the call, which
	// from here on is watched through LiveKit.
	select {
	case <-leg.Done():
	case <-ctx.Done():
	}
}

// answerChannel returns the channel closed when a Matrix user joins this
// call's RTC session.
func (s *Subsystem) answerChannel(callID string) chan struct{} {
	s.answeredMu.Lock()
	defer s.answeredMu.Unlock()
	ch, ok := s.answered[callID]
	if !ok {
		ch = make(chan struct{})
		s.answered[callID] = ch
	}
	return ch
}

// declineChannel returns the channel closed when a Matrix user rejects this
// call, and rememberNotification/forgetNotification maintain the map from the
// ring notification to the call it announced.
func (s *Subsystem) declineChannel(callID string) chan struct{} {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	ch, ok := s.declined[callID]
	if !ok {
		ch = make(chan struct{})
		s.declined[callID] = ch
	}
	return ch
}

func (s *Subsystem) forgetDeclineChannel(callID string) {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	delete(s.declined, callID)
}

func (s *Subsystem) rememberNotification(notify id.EventID, callID string) {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	s.notifies[notify] = callID
}

func (s *Subsystem) forgetNotification(callID string) {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	for notify, owner := range s.notifies {
		if owner == callID {
			delete(s.notifies, notify)
		}
	}
}

// signalDecline releases the goroutine holding the inbound leg of the call the
// given notification announced. An unknown notification is ignored: it names a
// call from a previous run or another bridge, and rejecting whatever happens
// to be ringing now would hang up on the wrong caller.
func (s *Subsystem) signalDecline(notify id.EventID) (string, bool) {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	callID, ok := s.notifies[notify]
	if !ok {
		return "", false
	}
	ch, ok := s.declined[callID]
	if !ok {
		return callID, false
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
	return callID, true
}

func (s *Subsystem) forgetAnswerChannel(callID string) {
	s.answeredMu.Lock()
	defer s.answeredMu.Unlock()
	delete(s.answered, callID)
}

// signalAnswer releases the goroutine holding an inbound leg. It is a no-op
// for a call that has none, which is the outbound case.
func (s *Subsystem) signalAnswer(callID string) {
	s.answeredMu.Lock()
	defer s.answeredMu.Unlock()
	ch, ok := s.answered[callID]
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// endCall retracts the ghost membership and marks the call ended. It is
// idempotent, because a hangup can be observed from more than one direction.
//
// The context is detached: the usual caller is holding an inbound leg, and that
// leg's context is already cancelled by the time the leg is gone. Cleaning up
// on a cancelled context would leave the membership pinned in the room, which
// is exactly the failure this function exists to prevent.
func (s *Subsystem) endCall(ctx context.Context, call *database.Call) error {
	if call.State == database.StateEnded {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.forgetSeen(call.CallID)
	s.forgetNotification(call.CallID)
	s.signalAnswer(call.CallID)
	if leg := s.takeLeg(call.CallID); leg != nil {
		if err := leg.Hangup(ctx); err != nil {
			s.log.Debug().Err(err).Str("call_id", call.CallID).Msg("Control leg was already gone")
		}
	}
	call.State = database.StateEnded
	if err := s.db.Call.Update(ctx, call); err != nil {
		return err
	}
	return s.retractGhostMembership(ctx, call)
}

func (s *Subsystem) keepLeg(callID string, leg OutboundLeg) {
	s.legsMu.Lock()
	defer s.legsMu.Unlock()
	s.legs[callID] = leg
}

func (s *Subsystem) takeLeg(callID string) OutboundLeg {
	s.legsMu.Lock()
	defer s.legsMu.Unlock()
	leg := s.legs[callID]
	delete(s.legs, callID)
	return leg
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
	source, err := s.sourceLogin()
	if err != nil {
		return nil, err
	}
	if portal.MXID == "" {
		// The room info is left to bridgev2, which asks the login's
		// GetChatInfo for it. That is the same call the inbound SMS path makes
		// through QueueRemoteEvent, so both paths land on one portal per
		// number rather than two descriptions of it that can drift.
		if err := portal.CreateMatrixRoom(ctx, source, nil); err != nil {
			return nil, fmt.Errorf("create room for %s: %w", portalID, err)
		}
	} else {
		s.reapplyChatInfo(ctx, portal, source)
	}
	// Every call, not just the first: GetChatInfo lists only the ghost, so
	// bridgev2 leaves the Matrix user to UserLogin.MarkInPortal, which runs
	// before the room exists and caches itself as done. Nothing else ever
	// rechecks the user's membership.
	s.ensureUserInPortal(ctx, portal, source)
	return portal, nil
}

// reapplyChatInfo pushes the connector's room description into a portal that
// already exists.
//
// It is what repairs rooms created before a change to GetChatInfo: bridgev2
// applies the power level overrides while creating a room and never revisits
// them, so a portal made without them would stay uncallable forever. Running
// it per call rather than once at startup also covers a room whose power
// levels someone edited by hand.
func (s *Subsystem) reapplyChatInfo(ctx context.Context, portal *bridgev2.Portal, source *bridgev2.UserLogin) {
	info, err := source.Client.GetChatInfo(ctx, portal)
	if err != nil {
		s.log.Warn().Err(err).Str("portal_id", string(portal.ID)).
			Msg("Could not refresh the portal description")
		return
	}
	portal.UpdateInfo(ctx, info, source, nil, time.Time{})
}

// sourceLogin returns the login every portal is created on behalf of.
//
// bridgev2 dereferences the source unconditionally while creating a room, so a
// call arriving before anyone has logged in has to fail here rather than panic
// inside the portal machinery.
func (s *Subsystem) sourceLogin() (*bridgev2.UserLogin, error) {
	login := s.br.GetCachedUserLoginByID(s.loginID)
	if login == nil {
		return nil, fmt.Errorf("no %q login yet; nobody has logged in", s.loginID)
	}
	return login, nil
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

// handleRtcDecline rejects the inbound call a Matrix client declined.
//
// The decline is matched by the notification event it relates to rather than
// by the room, so a decline sent late — by a second device, or for a call that
// already ended — cannot reject a different call in the same portal.
func (s *Subsystem) handleRtcDecline(ctx context.Context, evt *event.Event) {
	if _, isGhost := s.br.Matrix.ParseGhostMXID(evt.Sender); isGhost {
		return
	}
	if evt.Sender == s.br.Bot.GetMXID() {
		return
	}
	notify, ok := declineTarget(evt.Content.VeryRaw)
	if !ok {
		return
	}
	callID, signalled := s.signalDecline(notify)
	if !signalled {
		return
	}
	s.log.Info().
		Str("call_id", callID).
		Stringer("sender", evt.Sender).
		Msg("Matrix declined the call")
}

// onMatrixJoinedCall answers a ringing inbound call, or places an outbound one.
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
	s.signalAnswer(call.CallID)
	return nil
}

// onMatrixLeftCall hangs up when the Matrix participant leaves.
//
// Only a call still in progress is acted on: a leave event for a call that
// already ended is normal, because clients retract their membership after the
// far end hangs up.
func (s *Subsystem) onMatrixLeftCall(ctx context.Context, portal *bridgev2.Portal, log zerolog.Logger) {
	call, err := s.db.Call.GetActiveByPortal(ctx, string(portal.ID))
	if err != nil || call == nil {
		return
	}
	log.Info().Str("call_id", call.CallID).Msg("Matrix side left the call, hanging up")
	// The bridge has no channel of its own to hang up any more. Removing the
	// LiveKit participant drops livekit-sip's leg out of the conference, which
	// ends the call only if the conference is configured to end when that leg
	// leaves. See the SIP server contract in the README.
	s.removeParticipant(ctx, call, log)
	if err := s.endCall(ctx, call); err != nil {
		log.Warn().Err(err).Msg("Failed to end call")
	}
}

func (s *Subsystem) removeParticipant(ctx context.Context, call *database.Call, log zerolog.Logger) {
	if call.LKIdentity == "" || call.LKRoom == "" {
		return
	}
	if err := s.lk.RemoveParticipant(ctx, call.LKRoom, call.LKIdentity); err != nil {
		log.Warn().Err(err).Msg("Failed to remove the SIP participant from LiveKit")
	}
}

// bridgeMedia asks livekit-sip to dial the conference holding the call and
// join the LiveKit room backing the portal room's Element Call.
func (s *Subsystem) bridgeMedia(ctx context.Context, call *database.Call) error {
	trunkID, err := s.currentTrunkID()
	if err != nil {
		return err
	}
	// The room has to exist before livekit-sip is asked to join it. On a fresh
	// portal the bridge is the first participant of the session, and nothing
	// else creates the room: see EnsureRoom.
	if _, err := s.lk.EnsureRoom(ctx, call.LKRoom); err != nil {
		return fmt.Errorf("ensure LiveKit room: %w", err)
	}
	participant, err := s.lk.CreateSIPParticipant(ctx, &CreateSIPParticipantRequest{
		SipTrunkID: trunkID,
		// livekit-sip dials the conference as if it were a phone number; the
		// SIP server is expected to route the conference name into the
		// conference itself.
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
	s.log.Info().
		Str("call_id", call.CallID).
		Str("lk_room", call.LKRoom).
		Str("lk_participant", participant.ParticipantID).
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
	if s.cfg.OutboundURI == "" {
		return nil, fmt.Errorf("calls.outbound_uri is not configured")
	}
	if s.sip == nil {
		return nil, fmt.Errorf("the SIP transport is not running")
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
	// The ghost membership goes out before the phone rings, so the Matrix side
	// has something to join while it does.
	// No ring notification for an outbound call: the Matrix side started it
	// and is already in the session, so notifying it would ring the caller's
	// own phone.
	if _, err := s.publishGhostMembership(ctx, call); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Failed to publish ghost RTC membership for outbound call")
	}

	uri := strings.ReplaceAll(s.cfg.OutboundURI, "{number}", phonenum.FromID(portalID))
	leg, err := s.sip.Invite(ctx, uri, conference)
	if err != nil {
		_ = s.endCall(ctx, call)
		return nil, fmt.Errorf("invite %s: %w", uri, err)
	}
	s.keepLeg(call.CallID, leg)

	if err := s.bridgeMedia(ctx, call); err != nil {
		_ = s.endCall(ctx, call)
		return nil, err
	}
	call.State = database.StateBridged
	if err := s.db.Call.Update(ctx, call); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).Msg("Failed to record the outbound call")
	}
	s.log.Info().
		Str("call_id", call.CallID).
		Str("portal_id", portalID).
		Msg("Outbound call placed")
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

// runParticipantWatcher is how the bridge learns that a call has ended.
//
// There is no other signal. The bridge's own SIP leg is hung up a moment into
// the call by design, and it has no channel on the SIP server to watch. The
// one thing that lasts as long as the call is livekit-sip's participant in the
// LiveKit room, so its disappearance is what ends the call here. A missed
// departure leaves a ghost RTC membership pinned in the portal room until its
// expiry lapses, which is the failure mode this loop exists to prevent.
func (s *Subsystem) runParticipantWatcher(ctx context.Context) {
	t := time.NewTicker(s.cfg.ParticipantPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkParticipants(ctx)
		}
	}
}

func (s *Subsystem) checkParticipants(ctx context.Context) {
	active, err := s.db.Call.GetAllActive(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn().Err(err).Msg("Failed to list active calls")
		}
		return
	}
	for _, call := range active {
		if call.State != database.StateBridged || call.LKIdentity == "" {
			continue
		}
		present, err := s.lk.ParticipantPresent(ctx, call.LKRoom, call.LKIdentity)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn().Err(err).Str("call_id", call.CallID).
					Msg("Failed to check the LiveKit participant")
			}
			continue
		}
		if present {
			s.markSeen(call.CallID)
			continue
		}
		// Absent before it was ever present means LiveKit has not caught up
		// yet, not that the call ended.
		if !s.wasSeen(call.CallID) {
			continue
		}
		s.log.Info().Str("call_id", call.CallID).
			Msg("SIP participant left the LiveKit room, ending call")
		if err := s.endCall(ctx, call); err != nil {
			s.log.Warn().Err(err).Str("call_id", call.CallID).Msg("Failed to end call")
		}
	}
}

func (s *Subsystem) markSeen(callID string) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	s.seen[callID] = true
}

func (s *Subsystem) wasSeen(callID string) bool {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.seen[callID]
}

func (s *Subsystem) forgetSeen(callID string) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	delete(s.seen, callID)
}
