package calls

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// deviceIDFor is the device the ghost claims to be calling from.
//
// It is an invented string, not a Matrix device: matrix-js-sdk treats
// member.device_id and member.id as opaque, hashes them together with the user
// ID to derive the LiveKit identity, and never looks either of them up. No
// device keys, no /devices registration, no cross-signing.
func deviceIDFor(callID string) string {
	return "SIP" + strings.ToUpper(callID[:8])
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
func (s *Subsystem) publishGhostMembership(ctx context.Context, call *database.Call) (id.EventID, error) {
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return "", err
	}
	userID := ghost.Intent.GetMXID()
	deviceID := deviceIDFor(call.CallID)
	membershipID := MemberIDFor(s.cfg.IdentityScheme, userID.String(), deviceID, call.CallID)

	// The identity handed to CreateSIPParticipant and the member ID published
	// here are derived together on purpose: a disagreement between the two is
	// the same silent-audio bug as picking the wrong scheme.
	call.LKIdentity = ParticipantIdentityFor(s.cfg.IdentityScheme, userID.String(), deviceID, membershipID)
	if err := s.db.Call.Update(ctx, call); err != nil {
		return "", err
	}
	s.warnIfEncrypted(ctx, ghost, call.RoomID)
	stateKey := s.stateKeyFor(ctx, ghost, call.RoomID, userID, deviceID)
	content := ghostMembership(userID, call.RoomID, deviceID, membershipID,
		s.cfg.LiveKit.JWTServiceURL, s.cfg.MembershipExpiry)
	resp, err := ghost.Intent.SendState(ctx, call.RoomID, CallMemberEventType, stateKey, content, time.Time{})
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
// Nothing retracts this event. Its lifetime is the ring timeout, so the ring
// stops on its own at exactly the moment the bridge gives up on the call.
func (s *Subsystem) publishRingNotification(ctx context.Context, call *database.Call, membership id.EventID) (id.EventID, error) {
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return "", err
	}
	content := ringNotification(time.Now(), s.cfg.RingTimeout, membership, s.humanMembers(ctx, call.RoomID))
	resp, err := ghost.Intent.SendMessage(ctx, call.RoomID, RtcNotificationEventType, content, nil)
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// humanMembers lists the room members that are neither ghosts nor the bridge
// bot, which is who the ring notification mentions.
func (s *Subsystem) humanMembers(ctx context.Context, roomID id.RoomID) []id.UserID {
	members, err := s.br.Matrix.GetMembers(ctx, roomID)
	if err != nil {
		s.log.Warn().Err(err).Stringer("room_id", roomID).
			Msg("Could not list room members; the ring notification will mention nobody and may not push")
		return nil
	}
	botMXID := s.br.Bot.GetMXID()
	users := make([]id.UserID, 0, len(members))
	for userID, member := range members {
		if member == nil || member.Membership != event.MembershipJoin {
			continue
		}
		if userID == botMXID {
			continue
		}
		if _, isGhost := s.br.Matrix.ParseGhostMXID(userID); isGhost {
			continue
		}
		users = append(users, userID)
	}
	slices.Sort(users)
	return users
}

// retractGhostMembership publishes the empty content that ends a membership.
// Matrix has no state deletion, so leaving is an empty state event.
func (s *Subsystem) retractGhostMembership(ctx context.Context, call *database.Call) error {
	ghost, err := s.ghostFor(ctx, call.PortalID)
	if err != nil {
		return err
	}
	userID := ghost.Intent.GetMXID()
	stateKey := s.stateKeyFor(ctx, ghost, call.RoomID, userID, deviceIDFor(call.CallID))
	_, err = ghost.Intent.SendState(ctx, call.RoomID, CallMemberEventType, stateKey, leaveMembership(), time.Time{})
	return err
}

// sweepStaleMemberships retracts the memberships of calls that were in
// progress when the bridge last stopped.
//
// A membership is only ever retracted by this process, so a crash mid-call
// leaves a phantom participant in the room's call UI. Rather than carry a
// heartbeat or a delayed leave event for a case that lasts as long as a pod
// takes to restart, the memberships are reconciled once at startup against
// the call table, which after a restart lists exactly the calls that cannot
// still be running.
func (s *Subsystem) sweepStaleMemberships(ctx context.Context, stale []*database.Call) {
	for _, call := range stale {
		if err := s.retractGhostMembership(ctx, call); err != nil {
			s.log.Warn().Err(err).Str("call_id", call.CallID).
				Msg("Failed to retract a membership left over from the last run")
			continue
		}
		s.log.Info().Str("call_id", call.CallID).Stringer("room_id", call.RoomID).
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
func (s *Subsystem) stateKeyFor(ctx context.Context, ghost *bridgev2.Ghost, roomID id.RoomID, userID id.UserID, deviceID string) string {
	return rtcStateKey(userID, deviceID, SlotRoom, s.ownedStateKeys(ctx, ghost, roomID))
}

func rtcStateKey(userID id.UserID, deviceID, slot string, owned bool) string {
	key := userID.String() + "_" + deviceID + "_" + slot
	if owned {
		return key
	}
	return "_" + key
}

// roomVersions caches m.room.create lookups; the version of a room never
// changes, and an upgraded room is a different room.
var roomVersions sync.Map // id.RoomID -> bool

func (s *Subsystem) ownedStateKeys(ctx context.Context, ghost *bridgev2.Ghost, roomID id.RoomID) bool {
	if cached, ok := roomVersions.Load(roomID); ok {
		return cached.(bool)
	}
	owned := false
	if reader, ok := ghost.Intent.(bridgev2.MatrixAPIWithArbitraryRoomState); ok {
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
	roomVersions.Store(roomID, owned)
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

// encryptedRooms remembers which rooms have already been complained about.
var encryptedRooms sync.Map // id.RoomID -> bool

// warnIfEncrypted reports a portal room that will connect a call and then
// carry no audible sound.
//
// Element Call picks E2eeType.PER_PARTICIPANT whenever the room has an
// m.room.encryption event, turns on manageMediaKeys, and SFrame-encrypts media
// with keys handed out over to-device messages. The bridge publishes plain
// audio into LiveKit, which such a client receives and cannot decode: the call
// looks connected on both sides and is silent, with no error anywhere. Portal
// rooms must therefore stay unencrypted.
func (s *Subsystem) warnIfEncrypted(ctx context.Context, ghost *bridgev2.Ghost, roomID id.RoomID) {
	if _, done := encryptedRooms.Load(roomID); done {
		return
	}
	reader, ok := ghost.Intent.(bridgev2.MatrixAPIWithArbitraryRoomState)
	if !ok {
		return
	}
	evt, err := reader.GetStateEvent(ctx, roomID, event.StateEncryption, "")
	if err != nil || evt == nil {
		return
	}
	encryptedRooms.Store(roomID, true)
	s.log.Error().Stringer("room_id", roomID).
		Msg("Portal room is encrypted; Element Call will expect SFrame media and this call will be silent")
}
