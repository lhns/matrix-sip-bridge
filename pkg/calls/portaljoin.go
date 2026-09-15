package calls

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"
)

// roomJoiner is the double puppet's half of bridgev2.MatrixAPI.
type roomJoiner interface {
	EnsureJoined(ctx context.Context, roomID id.RoomID, extra ...bridgev2.EnsureJoinedParams) error
}

// roomInviter is the bot's half of bridgev2.MatrixAPI.
type roomInviter interface {
	EnsureInvited(ctx context.Context, roomID id.RoomID, userID id.UserID) error
}

// errNoDoublePuppet is returned when the user has no double puppet, so the
// caller can say which of the two reasons the user was left merely invited.
var errNoDoublePuppet = errors.New("no double puppet for the Matrix user")

// ensureUserInRoom joins userID to roomID as itself, falling back to a bot
// invite. It reports whether the user ended up joined.
//
// An invite the user has not accepted is fatal for a call, not cosmetic: the
// RTC membership that makes clients ring is a state event inside the portal
// room, which a non-member cannot see.
func ensureUserInRoom(ctx context.Context, roomID id.RoomID, userID id.UserID, dp roomJoiner, bot roomInviter) (bool, error) {
	var joinErr error
	if dp != nil {
		if joinErr = dp.EnsureJoined(ctx, roomID); joinErr == nil {
			return true, nil
		}
	} else {
		joinErr = errNoDoublePuppet
	}
	if bot == nil {
		return false, joinErr
	}
	if err := bot.EnsureInvited(ctx, roomID, userID); err != nil {
		return false, errors.Join(joinErr, err)
	}
	return false, joinErr
}

// ensureUserInPortal joins the login's Matrix user to an existing portal room.
//
// bridgev2 only invites the owning user while it is creating the room, and
// never retries afterwards, so a portal whose invite was declined or ignored
// would stay unusable forever. Double puppeting turns that invite into a join.
func (b *bridgeSide) ensureUserInPortal(ctx context.Context, portal *bridgev2.Portal, source *bridgev2.UserLogin) {
	if portal.MXID == "" {
		return
	}
	log := b.log.With().
		Str("portal_id", string(portal.ID)).
		Stringer("user_id", source.UserMXID).
		Logger()
	var dp roomJoiner
	if intent := source.User.DoublePuppet(ctx); intent != nil {
		dp = intent
	}
	joined, err := ensureUserInRoom(ctx, portal.MXID, source.UserMXID, dp, b.br.Bot)
	switch {
	case joined:
		log.Debug().Msg("Matrix user is in the portal room")
	case errors.Is(err, errNoDoublePuppet):
		log.Warn().Msg("No double puppet configured; the Matrix user must accept the portal invite by hand or it cannot see the call")
	default:
		log.Err(err).Msg("Failed to join the Matrix user to the portal room; it must accept the invite by hand")
	}
}
