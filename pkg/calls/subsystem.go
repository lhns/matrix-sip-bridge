package calls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// IdentityScheme selects how the caller ghost's LiveKit participant
	// identity is derived. It has to match what the Element Call build the
	// clients run derives for itself, and the two schemes cannot both be
	// served. See ADR-0012 for how to tell which one a deployment needs.
	IdentityScheme IdentityScheme `yaml:"identity_scheme"`
	// ParticipantPollInterval is how often LiveKit is asked whether the SIP
	// participant is still in the room. That is the only signal the bridge has
	// that a call ended; see runParticipantWatcher.
	ParticipantPollInterval time.Duration `yaml:"participant_poll_interval"`

	// Notices switches off a category of report that turns out to be noisy.
	// Everything in it is on by default.
	Notices NoticeConfig `yaml:"notices"`
}

// Subsystem bridges calls. It is deliberately not a bridgev2 concept: bridgev2
// has no MSC3401 or LiveKit support, and its inbound event whitelist does not
// include call.member, so the only things borrowed from it are the ghosts and
// portals used as identity and room substrate.
type Subsystem struct {
	cfg Config
	mx  matrixSide
	sip Telephony
	lk  *LiveKitClient
	db  *database.Database
	log zerolog.Logger

	trunkID atomic.Pointer[string]

	// trunkPushed is the fingerprint of the trunk spec this process last wrote
	// to LiveKit. It exists so that a deployment which does not return
	// auth_password from List is not rewritten on every reconcile tick.
	trunkPushed atomic.Pointer[string]

	// answered carries the "a Matrix user joined" signal from the call.member
	// handler to the goroutine holding an inbound leg open.
	answeredMu sync.Mutex
	answered   map[string]chan struct{}

	// ended is closed when a call is torn down, so the goroutine holding an
	// inbound leg stops waiting without mistaking teardown for an answer.
	endedMu sync.Mutex
	ended   map[string]chan struct{}

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
	//
	// It is not persisted: a restart ends every row it could describe, so the
	// map and the table cannot disagree about a call that survived one.
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
	if cfg.IdentityScheme == "" {
		cfg.IdentityScheme = IdentityUserDevice
	}
	return &Subsystem{
		cfg:      cfg,
		mx:       newBridgeSide(br, loginID, log),
		sip:      sip,
		lk:       NewLiveKitClient(cfg.LiveKit),
		db:       db,
		log:      log,
		answered: make(map[string]chan struct{}),
		declined: make(map[string]chan struct{}),
		ended:    make(map[string]chan struct{}),
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

	// The 180 goes out before the setup below, not after it: until a
	// provisional response arrives the caller hears nothing, and building the
	// portal for a number that has never called before takes seconds. A final
	// response after a 180 is still valid, so every path below can still
	// reject.
	if err := leg.Ringing(); err != nil {
		log.Err(err).Msg("Failed to send 180 Ringing")
		// A branch of a parallel Dial() that never gets a final response is
		// held until the transaction times out, so every failure here ends
		// with a definitive status rather than a dangling leg.
		_ = leg.Reject(500, "Server Internal Error")
		return
	}

	// The channels a ringing call is released by are created before it is
	// announced to Matrix. signalAnswer drops a signal for a call that has
	// none, and nothing repeats a call.member join: a user who answered while
	// the ring notification was still being sent, or while the media was being
	// bridged, was never heard and the call rang on until it timed out.
	callID := newCallID()
	waiters := s.waitersFor(callID)
	defer s.forgetWaiters(callID)

	call, err := s.beginInboundCall(ctx, callID, portalID, conference)
	if err != nil {
		log.Err(err).Msg("Failed to start inbound call")
		_ = leg.Reject(500, "Server Internal Error")
		return
	}
	log = log.With().Str("call_id", call.CallID).Logger()

	// The media is put in place while the phone is still ringing: the LiveKit
	// participant has to be in the room with a matching identity before a
	// Matrix client will render it, and doing it on answer would add that
	// latency to the moment the call connects.
	if err := s.bridgeMedia(ctx, call); err != nil {
		log.Err(err).Msg("Failed to put the call into LiveKit")
		_ = leg.Reject(503, "Service Unavailable")
		_ = s.failCall(ctx, call, mediaFailureReason(err))
		return
	}
	s.waitForMatrix(ctx, call, leg, log, waiters)
}

// callWaiters are the channels that release the goroutine holding an inbound
// leg open.
type callWaiters struct {
	joined   <-chan struct{}
	declined <-chan struct{}
	ended    <-chan struct{}
}

func (s *Subsystem) waitersFor(callID string) callWaiters {
	return callWaiters{
		joined:   s.answerChannel(callID),
		declined: s.declineChannel(callID),
		ended:    s.endedChannel(callID),
	}
}

func (s *Subsystem) forgetWaiters(callID string) {
	s.forgetAnswerChannel(callID)
	s.forgetDeclineChannel(callID)
	s.forgetEndedChannel(callID)
}

// beginInboundCall creates the portal room, the call row and the ghost's RTC
// membership, which is what makes Matrix ring.
func (s *Subsystem) beginInboundCall(ctx context.Context, callID, portalID, conference string) (call *database.Call, retErr error) {
	if existing, err := s.activeCallByConference(ctx, conference); err != nil {
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
	intent, err := s.ghostFor(ctx, portalID)
	if err != nil {
		return nil, err
	}
	call = &database.Call{
		CallID:     callID,
		PortalID:   portalID,
		RoomID:     portal.MXID,
		Direction:  database.DirectionInbound,
		Conference: conference,
		LKRoom:     LiveKitRoomName(portal.MXID.String(), SlotRoom),
		LKIdentity: s.identityFor(intent.GetMXID(), callID).participant,
		State:      database.StateRinging,
	}
	if err := s.db.Call.Insert(ctx, call); err != nil {
		return nil, fmt.Errorf("insert call: %w", err)
	}
	// Setup can be interrupted at any point -- a slow database alone is enough
	// to run it past the ring timeout and cancel it -- and a half-built row
	// left in progress blocks every later call from the same number, because
	// the conference is named after it. The row is therefore rolled back on
	// every error path out of here.
	defer func() {
		if retErr != nil {
			if err := s.failCall(context.WithoutCancel(ctx), call, "could not connect"); err != nil {
				s.log.Warn().Err(err).Str("call_id", call.CallID).
					Msg("Failed to roll back a call that could not be set up")
			}
		}
	}()
	membership, err := s.publishGhostMembership(ctx, intent, call)
	if err != nil {
		return nil, fmt.Errorf("publish ghost RTC membership: %w", err)
	}
	// A failed notification is not a failed call: the membership alone still
	// lets someone who opens the room join it, which is better than declining
	// a caller that could have been answered.
	if notify, err := s.publishRingNotification(ctx, intent, call, membership); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Failed to send the ring notification; Matrix clients will not ring for this call")
	} else {
		s.rememberNotification(notify, call.CallID)
		s.log.Info().Str("call_id", call.CallID).Stringer("notification", notify).
			Msg("Sent the ring notification")
	}
	s.log.Info().
		Str("call_id", call.CallID).
		Str("portal_id", portalID).
		Msg("Inbound call ringing Matrix")
	return call, nil
}

// waitForMatrix holds the control leg open until a Matrix user joins the RTC
// session, the caller gives up, or the ring times out.
func (s *Subsystem) waitForMatrix(ctx context.Context, call *database.Call, leg InboundLeg, log zerolog.Logger, w callWaiters) {
	select {
	case <-w.joined:
	case <-w.ended:
		// Teardown is not an answer. Answering here is what used to log
		// "left the call" and "call answered" for the same call in the same
		// breath, and rewrote the ended row back to bridged.
		log.Info().Msg("Call ended before Matrix answered")
		return
	case <-w.declined:
		log.Info().Msg("Matrix declined the call")
		// 486 rather than 480: the difference is what the caller's network
		// plays them, and a rejection is not an absence.
		_ = leg.Reject(486, "Busy Here")
		_ = s.declineCall(ctx, call)
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
		log.Info().Msg("The bridge is going away while the call rings, declining")
		_ = leg.Reject(503, "Service Unavailable")
		// endCall detaches the context it is given, so the teardown still
		// runs. Returning without it left the row ringing and the ghost's
		// membership pinned in the room.
		_ = s.failCall(ctx, call, "the bridge restarted")
		return
	}

	// The row moves to bridged before the leg is answered, not after: winning
	// the transition is what makes this path the owner of the call. Answering
	// first and recording it afterwards is how a call that teardown had
	// already finished still got reported as answered.
	// The write is detached from ctx: a call's context dies with its SIP leg,
	// and teardown cancels it, so recording the outcome on it is how an
	// answered call ended up logged as "context canceled".
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	mine, err := s.db.Call.Transition(recordCtx, call, database.StateRinging, database.StateBridged)
	cancelRecord()
	if err != nil {
		s.logSetupError(log, call, err, "Failed to record the answered call")
		_ = s.failCall(ctx, call, "could not connect")
		return
	}
	if !mine {
		log.Info().Msg("Call ended while it was being answered")
		return
	}
	if err := leg.Answer(); err != nil {
		log.Err(err).Msg("Failed to answer the call")
		_ = s.failCall(ctx, call, "could not connect")
		return
	}
	log.Info().Msg("Call answered; the dialplan now moves the caller into the conference")

	// The end of this leg is expected and says nothing about the call, which
	// from here on is watched through LiveKit.
	select {
	case <-leg.Done():
	case <-ctx.Done():
	}
}

// logSetupError reports a failure during call setup or teardown at a level
// that matches what it means.
//
// A cancellation is the expected consequence of the call going away, not a
// fault: logging it as an error trains the reader to skip the errors that are
// real.
func (s *Subsystem) logSetupError(log zerolog.Logger, call *database.Call, err error, msg string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		log.Debug().Err(err).Str("call_id", call.CallID).Msg(msg + " (the call was already going away)")
		return
	}
	log.Err(err).Str("call_id", call.CallID).Msg(msg)
}

// activeCallByPortal and activeCallByConference return the call in progress,
// ignoring -- and ending -- a row left behind by an interrupted setup.
//
// Without this a call whose setup was cut short stays "in progress" forever
// and blocks every later call from the same number, because the conference is
// named after the number rather than after the call.
func (s *Subsystem) activeCallByPortal(ctx context.Context, portalID string) (*database.Call, error) {
	call, err := s.db.Call.GetActiveByPortal(ctx, portalID)
	return s.discardIfStale(ctx, call, err)
}

func (s *Subsystem) activeCallByConference(ctx context.Context, conference string) (*database.Call, error) {
	call, err := s.db.Call.GetActiveByConference(ctx, conference)
	return s.discardIfStale(ctx, call, err)
}

func (s *Subsystem) discardIfStale(ctx context.Context, call *database.Call, err error) (*database.Call, error) {
	if err != nil || call == nil || !s.callIsStale(call, time.Now()) {
		return call, err
	}
	s.log.Warn().Str("call_id", call.CallID).Str("state", string(call.State)).
		Time("updated_at", call.UpdatedAt).
		Msg("Discarding a call that cannot still be in progress")
	if err := s.endCallAs(ctx, call, callEnd{Quiet: true}); err != nil {
		return nil, err
	}
	return nil, nil
}

// callIsStale reports whether a row claiming to be in progress cannot be.
//
// A ringing call is bounded by the ring timeout: nothing legitimately rings
// for longer, so a row that still says ringing after it is the wreckage of an
// interrupted setup. A bridged call has no such bound -- a real call can last
// as long as the two ends keep talking -- so it is only given up on at the
// membership expiry, which is when clients stop believing in it anyway.
func (s *Subsystem) callIsStale(call *database.Call, now time.Time) bool {
	switch call.State {
	case database.StateRinging:
		// The grace is for the setup work between the row being written and
		// the leg actually ringing.
		return now.Sub(call.CreatedAt) > s.cfg.RingTimeout+time.Minute
	case database.StateBridged:
		return now.Sub(call.UpdatedAt) > s.cfg.MembershipExpiry
	default:
		return false
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
// call, and rememberNotification/takeNotifications maintain the map from the
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

// endedChannel returns the channel closed when a call is torn down.
func (s *Subsystem) endedChannel(callID string) chan struct{} {
	s.endedMu.Lock()
	defer s.endedMu.Unlock()
	ch, ok := s.ended[callID]
	if !ok {
		ch = make(chan struct{})
		s.ended[callID] = ch
	}
	return ch
}

func (s *Subsystem) forgetEndedChannel(callID string) {
	s.endedMu.Lock()
	defer s.endedMu.Unlock()
	delete(s.ended, callID)
}

func (s *Subsystem) signalEnded(callID string) {
	s.endedMu.Lock()
	defer s.endedMu.Unlock()
	ch, ok := s.ended[callID]
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// ErrEndedWhileRinging reports an outbound call the Matrix side gave up on
// before the phone was answered. It is the expected outcome of a hangup during
// ringing, not a failure to place the call.
var ErrEndedWhileRinging = errors.New("the call ended while the phone was ringing")

// inviteContext returns a context cancelled when the call ends.
//
// The outbound INVITE blocks until the callee answers, and the bridge has no
// other handle on it while it rings. Without this, a Matrix hangup during
// ringing is recorded but never reaches the SIP server: the dialplan's
// Originate() keeps going, the callee answers into a conference nobody is on,
// and stays there until they hang up themselves.
//
// The returned stop function must be called once the INVITE is done or the
// watching goroutine and the call's entry in s.ended both leak. It is safe to
// call more than once.
func (s *Subsystem) inviteContext(parent context.Context, callID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ended := s.endedChannel(callID)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ended:
			cancel()
		case <-stopped:
		}
	}()
	return ctx, sync.OnceFunc(func() {
		close(stopped)
		cancel()
		s.forgetEndedChannel(callID)
	})
}

func (s *Subsystem) rememberNotification(notify id.EventID, callID string) {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	s.notifies[notify] = callID
}

// takeNotifications removes and returns the ring notifications a call sent, so
// the same call cannot be retracted twice and a late decline naming one of
// them no longer matches.
func (s *Subsystem) takeNotifications(callID string) []id.EventID {
	s.declinedMu.Lock()
	defer s.declinedMu.Unlock()
	var taken []id.EventID
	for notify, owner := range s.notifies {
		if owner == callID {
			taken = append(taken, notify)
			delete(s.notifies, notify)
		}
	}
	return taken
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
	return s.endCallAs(ctx, call, callEnd{})
}

// declineCall ends a call a Matrix user rejected, and failCall one the bridge
// itself could not carry. Both differ from endCall only in what the room is
// told; reason is the short half of "Call failed — ...".
func (s *Subsystem) declineCall(ctx context.Context, call *database.Call) error {
	return s.endCallAs(ctx, call, callEnd{Declined: true})
}

func (s *Subsystem) failCall(ctx context.Context, call *database.Call, reason string) error {
	return s.endCallAs(ctx, call, callEnd{Failure: reason})
}

func (s *Subsystem) endCallAs(ctx context.Context, call *database.Call, end callEnd) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The row as it was before it ended: End overwrites UpdatedAt and State,
	// which are what say whether the call was answered and for how long.
	before := *call
	// The compare-and-swap is the whole of the idempotence: a duplicate leave
	// event, a ring timeout and a watcher tick can all reach here for the same
	// call, and only the one that actually moved the row tears it down.
	mine, err := s.db.Call.End(ctx, call)
	if err != nil {
		return err
	}
	if !mine {
		return nil
	}
	s.forgetSeen(call.CallID)
	s.signalEnded(call.CallID)
	// Before anything slower: this is what stops the phones ringing.
	s.retractRingNotification(ctx, call)
	// livekit-sip holds the SIP leg into the conference for as long as its
	// participant exists, and nothing else removes it. Skipping this on the
	// decline and timeout paths left the conference up indefinitely, which
	// also blocks the next call to the same number.
	s.removeParticipant(ctx, call, s.log.With().Str("call_id", call.CallID).Logger())
	if leg := s.takeLeg(call.CallID); leg != nil {
		if err := leg.Hangup(ctx); err != nil {
			s.log.Debug().Err(err).Str("call_id", call.CallID).Msg("Control leg was already gone")
		}
	}
	err = s.retractGhostMembership(ctx, call)
	// Last, because it is the only step nobody is waiting on: the phones have
	// stopped ringing and the conference is down by here.
	s.postCallRecord(ctx, &before, end)
	return err
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

// ghostFor returns the intent of the ghost representing a phone number.
func (s *Subsystem) ghostFor(ctx context.Context, portalID string) (GhostIntent, error) {
	return s.mx.GhostIntent(ctx, portalID)
}

// portalForNumber returns the portal for a number, creating the Matrix room if
// it does not exist yet.
func (s *Subsystem) portalForNumber(ctx context.Context, portalID string) (Portal, error) {
	return s.mx.PortalRoom(ctx, portalID)
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
	if s.mx.IsGhost(evt.Sender) {
		return
	}
	if evt.Sender == s.mx.BotMXID() {
		return
	}
	portal, ok := s.mx.PortalByMXID(ctx, evt.RoomID)
	if !ok {
		return
	}
	content, active, err := ParseCallMember(evt.Content.VeryRaw, time.UnixMilli(evt.Timestamp))
	if err != nil {
		s.log.Warn().Err(err).
			Stringer("event_id", evt.ID).
			Msg("Failed to parse call.member content")
		return
	}
	log := s.log.With().
		Str("portal_id", portal.ID).
		Stringer("sender", evt.Sender).
		Bool("active", active).
		Logger()

	if !active {
		s.onMatrixLeftCall(ctx, portal, log)
		return
	}
	if err := s.onMatrixJoinedCall(ctx, portal, content, log); err != nil {
		// Giving up on a call while it rings is a user's decision, not a
		// fault: logging it as an error trains the reader to skip the real
		// ones.
		if errors.Is(err, ErrEndedWhileRinging) {
			log.Info().Msg("Matrix side hung up before the phone was answered")
			return
		}
		log.Err(err).Msg("Failed to handle Matrix RTC join")
	}
}

// handleRtcDecline rejects the inbound call a Matrix client declined.
//
// The decline is matched by the notification event it relates to rather than
// by the room, so a decline sent late — by a second device, or for a call that
// already ended — cannot reject a different call in the same portal.
func (s *Subsystem) handleRtcDecline(ctx context.Context, evt *event.Event) {
	if s.mx.IsGhost(evt.Sender) {
		return
	}
	if evt.Sender == s.mx.BotMXID() {
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
func (s *Subsystem) onMatrixJoinedCall(ctx context.Context, portal Portal, content *CallMemberContent, log zerolog.Logger) error {
	call, err := s.activeCallByPortal(ctx, portal.ID)
	if err != nil {
		return fmt.Errorf("look up call: %w", err)
	}
	if call == nil {
		log.Info().Msg("RTC membership in a portal with no call, dialling out")
		// dial, not Dial: the lookup Dial would repeat is the one just made.
		_, err = s.dial(ctx, portal)
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
func (s *Subsystem) onMatrixLeftCall(ctx context.Context, portal Portal, log zerolog.Logger) {
	call, err := s.activeCallByPortal(ctx, portal.ID)
	if err != nil || call == nil {
		return
	}
	log.Info().Str("call_id", call.CallID).Msg("Matrix side left the call, hanging up")
	// The bridge has no channel of its own to hang up any more. endCall
	// removes the LiveKit participant, which drops livekit-sip's leg out of
	// the conference; that ends the call only if the conference is configured
	// to end when that leg leaves. See the SIP server contract in the README.
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
	// The participant counts as seen from here: livekit-sip has answered and
	// joined the room, so its later absence means the call ended. Waiting for
	// a poll tick to observe it meant a call shorter than the poll interval
	// was never seen at all, the watcher never ended the row, and every later
	// call to that number was refused until the membership expiry.
	s.markSeen(call.CallID)
	s.log.Info().
		Str("call_id", call.CallID).
		Str("lk_room", call.LKRoom).
		Str("lk_participant", participant.ParticipantID).
		Msg("Media bridged into LiveKit")
	return s.db.Call.Update(ctx, call)
}

// Dial places an outbound call to the number a portal represents, unless one
// is already in progress there.
func (s *Subsystem) Dial(ctx context.Context, portal Portal) (*database.Call, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("call bridging is disabled")
	}
	if existing, err := s.activeCallByPortal(ctx, portal.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	return s.dial(ctx, portal)
}

// dial places the call. The caller owns the check that the portal has no call
// in progress; repeating it here cost a second identical query on the path the
// Matrix call button takes.
func (s *Subsystem) dial(ctx context.Context, portal Portal) (*database.Call, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("call bridging is disabled")
	}
	portalID := portal.ID
	if portal.MXID == "" {
		return nil, fmt.Errorf("portal %s has no Matrix room", portalID)
	}
	if s.cfg.OutboundURI == "" {
		return nil, fmt.Errorf("calls.outbound_uri is not configured")
	}
	if s.sip == nil {
		return nil, fmt.Errorf("the SIP transport is not running")
	}

	intent, err := s.ghostFor(ctx, portalID)
	if err != nil {
		return nil, err
	}
	conference := s.conferenceFor(portalID)
	callID := newCallID()
	call := &database.Call{
		CallID:     callID,
		PortalID:   portalID,
		RoomID:     portal.MXID,
		Direction:  database.DirectionOutbound,
		Conference: conference,
		LKRoom:     LiveKitRoomName(portal.MXID.String(), SlotRoom),
		LKIdentity: s.identityFor(intent.GetMXID(), callID).participant,
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
	//
	// Without it there is nothing for the Matrix side to join, so the callee
	// would be answered into a conference with nobody in it and stay there
	// until they hung up themselves.
	if _, err := s.publishGhostMembership(ctx, intent, call); err != nil {
		_ = s.failCall(ctx, call, "could not connect")
		return nil, fmt.Errorf("publish ghost RTC membership: %w", err)
	}

	uri := strings.ReplaceAll(s.cfg.OutboundURI, "{number}", phonenum.FromID(portalID))
	// The INVITE does not return until the phone is answered, so a Matrix user
	// hanging up while it rings has to reach it through the context: that is
	// what turns the hangup into a CANCEL.
	inviteCtx, stopInvite := s.inviteContext(ctx, call.CallID)
	leg, err := s.sip.Invite(inviteCtx, uri, conference)
	stopInvite()
	if err != nil {
		_ = s.failCall(ctx, call, "no route")
		return nil, fmt.Errorf("invite %s: %w", uri, err)
	}
	s.keepLeg(call.CallID, leg)

	// Winning this transition is what makes this path the owner of the call,
	// the same compare-and-swap the inbound side does before it answers. A
	// call the Matrix side gave up on while the phone rang is already ended,
	// and bridging media into it now would leave livekit-sip and the callee
	// alone in a conference until the callee hangs up.
	mine, err := s.db.Call.Transition(ctx, call, database.StateRinging, database.StateBridged)
	if err != nil {
		_ = s.failCall(ctx, call, "could not connect")
		return nil, fmt.Errorf("record the answered call: %w", err)
	}
	if !mine {
		s.log.Info().Str("call_id", call.CallID).Str("portal_id", portalID).
			Msg("Outbound call ended while the phone was still ringing")
		if leg := s.takeLeg(call.CallID); leg != nil {
			if err := leg.Hangup(ctx); err != nil {
				s.log.Debug().Err(err).Str("call_id", call.CallID).
					Msg("Control leg was already gone")
			}
		}
		return nil, ErrEndedWhileRinging
	}

	if err := s.bridgeMedia(ctx, call); err != nil {
		_ = s.failCall(ctx, call, mediaFailureReason(err))
		return nil, err
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
