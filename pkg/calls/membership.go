package calls

import (
	"context"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// deviceIDLen is how much of the call ID the ghost's device ID carries. The
// device ID only has to be stable and unique among the calls live in one room
// at once, so it is shortened to stay readable in a state key.
const deviceIDLen = 8

// deviceIDFor is the device the ghost claims to be calling from.
//
// It is an invented string, not a Matrix device: matrix-js-sdk treats
// member.device_id and member.id as opaque, hashes them together with the user
// ID to derive the LiveKit identity, and never looks either of them up. No
// device keys, no /devices registration, no cross-signing.
//
// A shorter call ID is used whole rather than sliced: this runs inside a
// Matrix event handler, on an ID that came back from the database, and a panic
// there would take down the handler for every later event too.
func deviceIDFor(callID string) string {
	if len(callID) > deviceIDLen {
		callID = callID[:deviceIDLen]
	}
	return "SIP" + strings.ToUpper(callID)
}

// callIdentity is the set of names one call's RTC membership and its LiveKit
// participant share.
//
// The member ID published in Matrix and the identity handed to
// CreateSIPParticipant are derived together on purpose: a disagreement between
// the two is the same silent-audio bug as picking the wrong scheme.
type callIdentity struct {
	userID       id.UserID
	deviceID     string
	membershipID string
	participant  string
}

// identityFor derives the whole set from the ghost's MXID and the call ID,
// with no I/O of its own, so the participant identity can be written by the
// INSERT that creates the call row rather than by an UPDATE afterwards.
func (s *Subsystem) identityFor(userID id.UserID, callID string) callIdentity {
	deviceID := deviceIDFor(callID)
	membershipID := MemberIDFor(s.cfg.IdentityScheme, userID.String(), deviceID, callID)
	return callIdentity{
		userID:       userID,
		deviceID:     deviceID,
		membershipID: membershipID,
		participant:  ParticipantIdentityFor(s.cfg.IdentityScheme, userID.String(), deviceID, membershipID),
	}
}

// publishGhostMembership announces the caller as a participant of the room's
// RTC session, which is what makes the call joinable. It returns the event ID
// so the ring notification can reference the membership it belongs to.
//
// Joinable is not ringing: a client shows an incoming call only once the
// MSC4075 notification arrives as well. See publishRingNotification.
//
// The event is sent as the ghost, not by the bot on its behalf:
// checkRtcMembershipData rejects a membership whose member.user_id is not the
// sender, so appservice masquerading is load-bearing here.
//
// There is no refresh timer; see Config.MembershipExpiry.
func (s *Subsystem) publishGhostMembership(ctx context.Context, intent GhostIntent, call *database.Call) (id.EventID, error) {
	ident := s.identityFor(intent.GetMXID(), call.CallID)
	s.warnIfEncrypted(ctx, intent, call.RoomID)
	stateKey := s.stateKeyFor(ctx, intent, call.RoomID, ident.userID, ident.deviceID)
	content := ghostMembership(ident.userID, call.RoomID, ident.deviceID, ident.membershipID,
		s.cfg.LiveKit.JWTServiceURL, s.cfg.MembershipExpiry)
	resp, err := intent.SendState(ctx, call.RoomID, CallMemberEventType, stateKey, content, time.Time{})
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// publishRingNotification sends the event that actually makes a client ring.
//
// It is sent as the ghost for the same reason the membership is: a client that
// sees a notification whose sender is not the RTC member it relates to has no
// caller to display, and the decline it sends back would not refer to anyone.
//
// Its lifetime bounds the ring; retractRingNotification ends it sooner when
// the call does.
func (s *Subsystem) publishRingNotification(ctx context.Context, intent GhostIntent, call *database.Call, membership id.EventID) (id.EventID, error) {
	content := ringNotification(time.Now(), s.cfg.RingTimeout, membership, s.humanMembers(ctx, call.RoomID))
	resp, err := intent.SendMessage(ctx, call.RoomID, RtcNotificationEventType, content, nil)
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// humanMembers lists the room members that are neither ghosts nor the bridge
// bot, which is who the ring notification mentions.
func (s *Subsystem) humanMembers(ctx context.Context, roomID id.RoomID) []id.UserID {
	members, err := s.mx.Members(ctx, roomID)
	if err != nil {
		s.log.Warn().Err(err).Stringer("room_id", roomID).
			Msg("Could not list room members; the ring notification will mention nobody and may not push")
		return nil
	}
	botMXID := s.mx.BotMXID()
	users := make([]id.UserID, 0, len(members))
	for userID, member := range members {
		if member == nil || member.Membership != event.MembershipJoin {
			continue
		}
		if userID == botMXID {
			continue
		}
		if s.mx.IsGhost(userID) {
			continue
		}
		users = append(users, userID)
	}
	slices.Sort(users)
	return users
}

// retractRingNotification redacts the events that made clients ring.
//
// MSC4075 has no cancellation event, so redaction is the retraction: without
// it a caller who gives up after eight seconds, or a call that fails on a
// missing trunk, leaves every device in the room ringing for the rest of the
// notification's lifetime.
//
// The redaction is sent by the ghost that sent the notification, which is what
// makes it allowed without giving the ghost a power level.
func (s *Subsystem) retractRingNotification(ctx context.Context, call *database.Call) {
	notifies := s.takeNotifications(call.CallID)
	if len(notifies) == 0 {
		return
	}
	intent, err := s.mx.GhostIntent(ctx, call.PortalID)
	if err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Could not retract the ring notification; clients will ring until it lapses")
		return
	}
	for _, notify := range notifies {
		content := &event.Content{Parsed: &event.RedactionEventContent{Redacts: notify}}
		if _, err := intent.SendMessage(ctx, call.RoomID, event.EventRedaction, content, nil); err != nil {
			s.log.Warn().Err(err).Str("call_id", call.CallID).Stringer("notification", notify).
				Msg("Could not retract the ring notification; clients will ring until it lapses")
		}
	}
}

// retractGhostMembership publishes the empty content that ends a membership.
// Matrix has no state deletion, so leaving is an empty state event.
func (s *Subsystem) retractGhostMembership(ctx context.Context, call *database.Call) error {
	intent, err := s.mx.GhostIntent(ctx, call.PortalID)
	if err != nil {
		return err
	}
	userID := intent.GetMXID()
	stateKey := s.stateKeyFor(ctx, intent, call.RoomID, userID, deviceIDFor(call.CallID))
	_, err = intent.SendState(ctx, call.RoomID, CallMemberEventType, stateKey, leaveMembership(), time.Time{})
	return err
}

// sweepStaleMemberships tears down the calls that were in progress when the
// bridge last stopped.
//
// A membership is only ever retracted by this process, so a crash mid-call
// leaves a phantom participant in the room's call UI. Rather than carry a
// heartbeat or a delayed leave event for a case that lasts as long as a pod
// takes to restart, the memberships are reconciled once at startup against
// the call table, which after a restart lists exactly the calls that cannot
// still be running.
//
// The LiveKit participant goes with the membership. Retracting only the Matrix
// side left livekit-sip in the room and the caller in the conference with
// nobody to talk to, which also blocks the next call to that number.
func (s *Subsystem) sweepStaleMemberships(ctx context.Context, stale []*database.Call) {
	for _, call := range stale {
		log := s.log.With().Str("call_id", call.CallID).Logger()
		s.removeParticipant(ctx, call, log)
		if err := s.retractGhostMembership(ctx, call); err != nil {
			log.Warn().Err(err).
				Msg("Failed to retract a membership left over from the last run")
			continue
		}
		log.Info().Stringer("room_id", call.RoomID).
			Msg("Retracted an RTC membership left over from the last run")
	}
}

// stateKeyFor builds the membership state key for a room.
//
// The format is "<user>_<device>_<application><slot>", prefixed with an
// underscore unless the room version lets a user own state keys that begin
// with their own MXID. Getting it wrong means the event is refused by the
// server or ignored by clients, so an unreadable room version falls back to
// the prefixed form, which every room version accepts.
func (s *Subsystem) stateKeyFor(ctx context.Context, intent GhostIntent, roomID id.RoomID, userID id.UserID, deviceID string) string {
	return rtcStateKey(userID, deviceID, SlotRoom, s.ownedStateKeys(ctx, intent, roomID))
}

func rtcStateKey(userID id.UserID, deviceID, slot string, owned bool) string {
	key := userID.String() + "_" + deviceID + "_" + slot
	if owned {
		return key
	}
	return "_" + key
}

func (s *Subsystem) ownedStateKeys(ctx context.Context, intent GhostIntent, roomID id.RoomID) bool {
	// The version of a room never changes, and an upgraded room is a
	// different room, so the answer is cached for the subsystem's life.
	if cached, ok := s.roomVersions.Load(roomID); ok {
		return cached.(bool)
	}
	owned := false
	// An intent that cannot read room state is a test double; see
	// roomStateReader. The prefixed key is what that conservative answer is.
	if reader, ok := intent.(roomStateReader); ok {
		evt, err := reader.GetStateEvent(ctx, roomID, event.StateCreate, "")
		if err != nil {
			s.log.Debug().Err(err).Stringer("room_id", roomID).
				Msg("Could not read the room version; using the prefixed RTC state key")
		} else if evt != nil {
			if create, ok := evt.Content.Parsed.(*event.CreateEventContent); ok {
				owned = supportsOwnedStateKeys(create.RoomVersion)
			}
		}
	}
	s.roomVersions.Store(roomID, owned)
	return owned
}

// supportsOwnedStateKeys reports whether a room version allows a state key
// beginning with the sender's own MXID (MSC3757). Only the unstable room
// versions carrying the MSC advertise it; everything else needs the underscore
// prefix.
func supportsOwnedStateKeys(version id.RoomVersion) bool {
	v := string(version)
	return strings.HasPrefix(v, "org.matrix.msc3757.") || strings.HasPrefix(v, "org.matrix.msc3779.")
}

// ghostMembership builds the state event content the bridge publishes on
// behalf of a phone-number ghost, so that Matrix clients render the caller as
// a participant of the RTC session rather than an anonymous stream.
//
// member.device_id and member.id are invented strings. What matters is that
// the same triple of user ID, device ID and member ID is hashed into the
// LiveKit participant identity: Element Call filters audio tracks to the
// identities its membership list derives, and an unmatched participant is
// inaudible as well as invisible. See ADR-0010.
func ghostMembership(userID id.UserID, roomID id.RoomID, deviceID, membershipID, jwtServiceURL string, expiry time.Duration) *event.Content {
	now := time.Now()
	return &event.Content{Raw: map[string]any{
		"member": map[string]any{
			"user_id":     userID.String(),
			"device_id":   deviceID,
			"id":          membershipID,
			"application": "m.call",
			"scope":       "m.room",
		},
		// The flat fields are the older SessionMembershipData shape, kept
		// because clients have not all moved to the nested one.
		"application":  "m.call",
		"call_id":      "",
		"scope":        "m.room",
		"device_id":    deviceID,
		"membershipID": membershipID,
		"created_ts":   now.UnixMilli(),
		// A duration relative to created_ts, not a deadline. Writing an
		// absolute timestamp here parses as a duration of forty thousand
		// years, which is not the harmless mistake it looks like: see
		// membershipExpiry.
		"expires": expiry.Milliseconds(),
		// Read only in a room the client considers a DM: Element X has no
		// non-DM voice intent and drops this before Element Call sees it.
		// Without it the call opens with the camera on and on the speaker.
		"m.call.intent": "audio",
		// focus_active and the two livekit_* fields are mandatory in the
		// receiving parsers. A membership missing any of them is dropped
		// whole, the client then believes the room has no active call, and it
		// neither rings nor renders the caller. Nothing reports the failure.
		"focus_active": map[string]any{
			"type":            "livekit",
			"focus_selection": "oldest_membership",
		},
		"foci_preferred": []any{
			map[string]any{
				"type":                "livekit",
				"livekit_alias":       roomID.String(),
				"livekit_service_url": jwtServiceURL,
			},
		},
	}}
}

// leaveMembership is the empty content that ends a membership.
func leaveMembership() *event.Content {
	return &event.Content{Raw: map[string]any{}}
}

// warnIfEncrypted reports a portal room that will connect a call and then
// carry no audible sound.
//
// Element Call picks E2eeType.PER_PARTICIPANT whenever the room has an
// m.room.encryption event, turns on manageMediaKeys, and SFrame-encrypts media
// with keys handed out over to-device messages. The bridge publishes plain
// audio into LiveKit, which such a client receives and cannot decode: the call
// looks connected on both sides and is silent, with no error anywhere. Portal
// rooms must therefore stay unencrypted.
func (s *Subsystem) warnIfEncrypted(ctx context.Context, intent GhostIntent, roomID id.RoomID) {
	// Both answers are cached, not just the positive one: caching only the
	// encrypted case left the common one re-reading m.room.encryption on
	// every call. A room's encryption cannot be turned off again, so a
	// cached "not encrypted" is only ever stale in the direction that costs
	// the warning, not the call.
	if _, done := s.encryptedRooms.Load(roomID); done {
		return
	}
	reader, ok := intent.(roomStateReader)
	if !ok {
		// A test double; see roomStateReader. Nothing is cached, so a real
		// intent for the same room still gets its answer.
		return
	}
	evt, err := reader.GetStateEvent(ctx, roomID, event.StateEncryption, "")
	if err != nil {
		// A failed read says nothing about the room, so it is not cached.
		return
	}
	if evt == nil {
		s.encryptedRooms.Store(roomID, false)
		return
	}
	s.encryptedRooms.Store(roomID, true)
	s.log.Error().Stringer("room_id", roomID).
		Msg("Portal room is encrypted; Element Call will expect SFrame media and this call will be silent")
}
