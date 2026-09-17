package calls

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Portal is a portal room as the call subsystem needs it: the number it
// bridges and the Matrix room that number's calls happen in.
type Portal struct {
	ID   string
	MXID id.RoomID
}

// GhostIntent is the slice of bridgev2.MatrixAPI used to act as the ghost
// representing a phone number. Memberships and notifications are sent by the
// ghost itself rather than by the bot on its behalf; see
// publishGhostMembership for why the masquerade is load-bearing.
type GhostIntent interface {
	GetMXID() id.UserID
	SendState(ctx context.Context, roomID id.RoomID, eventType event.Type, stateKey string, content *event.Content, ts time.Time) (*mautrix.RespSendEvent, error)
	SendMessage(ctx context.Context, roomID id.RoomID, eventType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error)
}

// roomStateReader is the arbitrary-room-state half of
// bridgev2.MatrixAPIWithArbitraryRoomState. Asserting to bridgev2's own
// interface would demand the whole of MatrixAPI from a test double, and the
// only method either caller here uses is this one.
//
// bridgev2's own ghost intent implements it, so an intent that does not is a
// test double. Both callers -- ownedStateKeys and warnIfEncrypted -- then take
// the conservative answer rather than failing: the prefixed state key every
// room version accepts, and no encryption warning.
type roomStateReader interface {
	GetStateEvent(ctx context.Context, roomID id.RoomID, eventType event.Type, stateKey string) (*event.Event, error)
}

// matrixSide is the whole of bridgev2 the call subsystem depends on.
//
// bridgev2's bridge, portals, ghosts and logins are concrete types, so without
// this seam none of the orchestration below can be instantiated in a test —
// which is the band every bug found by a live call has been in. The production
// implementation is bridgeSide, a thin adapter; anything that looks like
// policy belongs on Subsystem, not here.
type matrixSide interface {
	// GhostIntent returns the Matrix API of the ghost representing a number.
	GhostIntent(ctx context.Context, portalID string) (GhostIntent, error)
	// PortalRoom returns the portal for a number, creating the Matrix room if
	// it does not exist yet and repairing an older one that predates the
	// call-capable power levels.
	PortalRoom(ctx context.Context, portalID string) (Portal, error)
	// PortalIDs lists the IDs of the portals that already have a Matrix room.
	// bridgev2 has no number -> portal index, so a number is resolved to the
	// lines it is already a conversation on by reading them all; see
	// portalIDForNumber.
	PortalIDs(ctx context.Context) ([]string, error)
	// PortalByMXID maps a Matrix room back to the portal it is, reporting
	// false for a room that is not one.
	PortalByMXID(ctx context.Context, roomID id.RoomID) (Portal, bool)
	// BotMXID is the bridge bot, whose own events must not be reacted to.
	BotMXID() id.UserID
	// IsGhost reports a user the bridge itself puppets, for the same reason.
	IsGhost(userID id.UserID) bool
	// Members lists a room's members, which is who the ring notification
	// mentions.
	Members(ctx context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error)
}

// bridgeSide implements matrixSide over a real bridgev2 bridge.
type bridgeSide struct {
	br  *bridgev2.Bridge
	log zerolog.Logger

	// loginID names the bridge's one static UserLogin. Every portal bridgev2
	// creates needs it as the source; there is no per-user login to take it
	// from.
	loginID networkid.UserLoginID

	// repaired names the portal rooms already known to carry the power levels
	// a call needs, so reapplyChatInfo reads them once rather than on every
	// call. A room never loses them again.
	repaired sync.Map // id.RoomID -> struct{}
}

func newBridgeSide(br *bridgev2.Bridge, loginID networkid.UserLoginID, log zerolog.Logger) *bridgeSide {
	return &bridgeSide{br: br, loginID: loginID, log: log}
}

func (b *bridgeSide) GhostIntent(ctx context.Context, portalID string) (GhostIntent, error) {
	ghost, err := b.br.GetGhostByID(ctx, networkid.UserID(portalID))
	if err != nil {
		return nil, err
	}
	return ghost.Intent, nil
}

func (b *bridgeSide) PortalRoom(ctx context.Context, portalID string) (Portal, error) {
	key := networkid.PortalKey{ID: networkid.PortalID(portalID)}
	portal, err := b.br.GetPortalByKey(ctx, key)
	if err != nil {
		return Portal{}, fmt.Errorf("get portal %s: %w", portalID, err)
	}
	source, err := b.sourceLogin()
	if err != nil {
		return Portal{}, err
	}
	if portal.MXID == "" {
		// The room info is left to bridgev2, which asks the login's
		// GetChatInfo for it. That is the same call the inbound SMS path makes
		// through QueueRemoteEvent, so both paths land on one portal per
		// number rather than two descriptions of it that can drift.
		if err := portal.CreateMatrixRoom(ctx, source, nil); err != nil {
			return Portal{}, fmt.Errorf("create room for %s: %w", portalID, err)
		}
	} else {
		b.reapplyChatInfo(ctx, portal, source)
	}
	// Every call, not just the first: GetChatInfo lists only the ghost, so
	// bridgev2 leaves the Matrix user to UserLogin.MarkInPortal, which runs
	// before the room exists and caches itself as done. Nothing else ever
	// rechecks the user's membership.
	b.ensureUserInPortal(ctx, portal, source)
	return Portal{ID: string(portal.ID), MXID: portal.MXID}, nil
}

// PortalIDs reads every portal because bridgev2 indexes portals by key and by
// MXID and by nothing else. Rows with no room are left out: a key with no room
// is not a conversation to consolidate onto, and dialling it would create the
// room anyway.
func (b *bridgeSide) PortalIDs(ctx context.Context) ([]string, error) {
	portals, err := b.br.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return nil, fmt.Errorf("list portals: %w", err)
	}
	ids := make([]string, 0, len(portals))
	for _, portal := range portals {
		ids = append(ids, string(portal.ID))
	}
	return ids, nil
}

func (b *bridgeSide) PortalByMXID(ctx context.Context, roomID id.RoomID) (Portal, bool) {
	portal, err := b.br.GetPortalByMXID(ctx, roomID)
	if err != nil || portal == nil {
		return Portal{}, false
	}
	return Portal{ID: string(portal.ID), MXID: portal.MXID}, true
}

func (b *bridgeSide) BotMXID() id.UserID {
	return b.br.Bot.GetMXID()
}

func (b *bridgeSide) IsGhost(userID id.UserID) bool {
	_, isGhost := b.br.Matrix.ParseGhostMXID(userID)
	return isGhost
}

func (b *bridgeSide) Members(ctx context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	return b.br.Matrix.GetMembers(ctx, roomID)
}

// reapplyChatInfo repairs a portal that was created before the call-capable
// power levels existed.
//
// bridgev2 applies the power level overrides while creating a room and never
// revisits them, so without this an older portal would stay uncallable
// forever. It runs on every call, so a room found to need nothing is
// remembered: the check itself is a state read, and re-sending room state is
// both a write against the database for no reason and a change clients render.
func (b *bridgeSide) reapplyChatInfo(ctx context.Context, portal *bridgev2.Portal, source *bridgev2.UserLogin) {
	if _, done := b.repaired.Load(portal.MXID); done {
		return
	}
	levels, err := b.br.Matrix.GetPowerLevels(ctx, portal.MXID)
	if err != nil {
		b.log.Warn().Err(err).Str("portal_id", string(portal.ID)).
			Msg("Could not read the portal's power levels")
		return
	}
	if callPowerLevelsApplied(levels) {
		b.repaired.Store(portal.MXID, struct{}{})
		return
	}
	info, err := source.Client.GetChatInfo(ctx, portal)
	if err != nil {
		b.log.Warn().Err(err).Str("portal_id", string(portal.ID)).
			Msg("Could not refresh the portal description")
		return
	}
	b.log.Info().Str("portal_id", string(portal.ID)).
		Msg("Granting the portal the power levels a call needs")
	portal.UpdateInfo(ctx, info, source, nil, time.Time{})
}

// callPowerLevelsApplied reports whether a room already lets its members send
// the RTC membership, i.e. whether there is anything to repair.
func callPowerLevelsApplied(levels *event.PowerLevelsEventContent) bool {
	if levels == nil {
		return false
	}
	for evtType, want := range MembershipPowerLevels() {
		if levels.GetEventLevel(evtType) != want {
			return false
		}
	}
	return true
}

// sourceLogin returns the login every portal is created on behalf of.
//
// bridgev2 dereferences the source unconditionally while creating a room, so a
// call arriving before anyone has logged in has to fail here rather than panic
// inside the portal machinery.
func (b *bridgeSide) sourceLogin() (*bridgev2.UserLogin, error) {
	login := b.br.GetCachedUserLoginByID(b.loginID)
	if login == nil {
		return nil, fmt.Errorf("no %q login yet; nobody has logged in", b.loginID)
	}
	return login, nil
}
