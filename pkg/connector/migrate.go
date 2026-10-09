package connector

import (
	"context"
	"fmt"
	"slices"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// moveUserPortalsQuery hands the old login's user_portal rows to the new one,
// which keeps preferred, in_space and last_read: deleting the old login would
// cascade them away. It writes bridgev2's own table, which has no API for it.
// A row the new login already has is left to that cascade.
const moveUserPortalsQuery = `
	UPDATE user_portal SET login_id=$3
	WHERE bridge_id=$1 AND login_id=$2 AND NOT EXISTS (
		SELECT 1 FROM user_portal o
		WHERE o.bridge_id=user_portal.bridge_id AND o.user_mxid=user_portal.user_mxid
		  AND o.login_id=$3 AND o.portal_id=user_portal.portal_id
		  AND o.portal_receiver=user_portal.portal_receiver
	)`

// migrateSharedLogin moves the rooms of the old shared "sip" login to its
// owner's own login, keeping the Matrix rooms, and then removes the old login.
// It is idempotent: with no old login and no receiver-less portal it does
// nothing.
//
// It runs before the SIP endpoint starts, so no inbound text can create a
// second portal for a number while its room is still keyed the old way.
//
// bridge.split_portals is deliberately not used: it guesses a receiver per
// portal and deletes the rooms it cannot assign.
func (sc *SIPConnector) migrateSharedLogin(ctx context.Context) error {
	br := sc.br
	log := br.Log.With().Str("action", "migrate shared login").Logger()
	ctx = log.WithContext(ctx)

	legacy, err := br.GetExistingUserLoginByID(ctx, legacyLoginID)
	if err != nil {
		return fmt.Errorf("get the shared login: %w", err)
	}
	if legacy != nil {
		br.DB.KV.Set(ctx, keyDefaultRecipient, string(legacy.UserMXID))
	}
	owner := id.UserID(br.DB.KV.Get(ctx, keyDefaultRecipient))

	portals, err := portalsWithoutReceiver(ctx, br)
	if err != nil {
		return err
	}
	if legacy == nil && len(portals) == 0 {
		return nil
	}
	if owner == "" {
		// Never guess whose rooms these are.
		log.Warn().Int("portals", len(portals)).
			Msg("Portals without a receiver and no default recipient; leaving them alone")
		return nil
	}

	target, _, err := sc.logins.ensure(ctx, owner, false)
	if err != nil {
		return fmt.Errorf("login for %s: %w", owner, err)
	}

	// Spaces first. portal_parent_fkey is ON UPDATE CASCADE, so re-keying a
	// parent rewrites its children's parent_receiver in the database before any
	// child is loaded into bridgev2's cache; a child cached with the old
	// parent key would write it back and violate the foreign key.
	slices.SortStableFunc(portals, func(a, b *database.Portal) int {
		return boolToInt(isSpacePortal(b)) - boolToInt(isSpacePortal(a))
	})
	skipped := 0
	for _, src := range portals {
		if !sc.reIDPortal(ctx, src, target.ID) {
			skipped++
		}
	}

	if legacy == nil {
		if skipped > 0 {
			log.Warn().Int("skipped", skipped).Msg("Some portals could not be moved")
		}
		return nil
	}
	if _, err := br.DB.Exec(ctx, moveUserPortalsQuery, br.ID, legacy.ID, target.ID); err != nil {
		return fmt.Errorf("move user portals: %w", err)
	}
	if skipped > 0 {
		log.Warn().Int("skipped", skipped).
			Msg("Keeping the shared login because some portals could not be moved")
		return nil
	}
	if err := transferSpace(ctx, legacy, target); err != nil {
		return err
	}
	// DontCleanupRooms: the rooms now belong to the new login.
	legacy.Delete(ctx, status.BridgeState{}, bridgev2.DeleteOpts{DontCleanupRooms: true, BlockingCleanup: true})
	log.Info().Int("portals", len(portals)).Stringer("owner", owner).
		Msg("Moved the shared login's portals to its owner's own login and removed the shared login")
	return nil
}

// reIDPortal re-keys one portal onto the new login's receiver and reports
// whether it was moved. A portal is skipped, with a warning, rather than ever
// merged into an existing room: ReIDPortal's both-have-rooms path tombstones
// and deletes the source room.
func (sc *SIPConnector) reIDPortal(ctx context.Context, src *database.Portal, receiver networkid.UserLoginID) bool {
	br := sc.br
	log := br.Log.With().Object("portal_key", src.PortalKey).Logger()
	target := networkid.PortalKey{ID: src.ID, Receiver: receiver}
	existing, err := br.DB.Portal.GetByKey(ctx, target)
	if err != nil {
		log.Warn().Err(err).Msg("Could not check for an existing portal; not moving this one")
		return false
	}
	if existing != nil && existing.MXID != "" {
		log.Warn().Stringer("existing_room", existing.MXID).Stringer("room", src.MXID).
			Msg("The owner already has a room for this portal; not moving it")
		return false
	}
	result, _, err := br.ReIDPortal(ctx, src.PortalKey, target)
	if err != nil {
		log.Warn().Err(err).Msg("Could not move the portal")
		return false
	}
	log.Info().Stringer("room", src.MXID).Stringer("result", result).Msg("Moved a portal")
	// The parent foreign key cascades to the children's rows, but not to
	// children bridgev2 already has cached.
	if isSpacePortal(src) {
		if err := reparentChildren(ctx, br, target); err != nil {
			log.Warn().Err(err).Msg("Could not re-parent the space's rooms")
		}
	}
	return true
}

// transferSpace gives the old login's personal filtering space to the new
// login, or, if that has one already, just detaches it: UserLogin.Delete
// deletes the login's space room unconditionally, and it is the owner's.
func transferSpace(ctx context.Context, from, to *bridgev2.UserLogin) error {
	if from.SpaceRoom == "" {
		return nil
	}
	if to.SpaceRoom == "" {
		to.SpaceRoom = from.SpaceRoom
		if err := to.Save(ctx); err != nil {
			return fmt.Errorf("save the space on the new login: %w", err)
		}
	}
	from.SpaceRoom = ""
	if err := from.Save(ctx); err != nil {
		return fmt.Errorf("clear the space on the shared login: %w", err)
	}
	return nil
}

// portalsWithoutReceiver lists the portals keyed with no receiver.
func portalsWithoutReceiver(ctx context.Context, br *bridgev2.Bridge) ([]*database.Portal, error) {
	all, err := br.DB.Portal.GetAllWithoutReceiver(ctx)
	if err != nil {
		return nil, fmt.Errorf("list portals without a receiver: %w", err)
	}
	// The query also returns children whose parent_receiver is empty but whose
	// own receiver is set; those belong to someone already.
	return slices.DeleteFunc(all, func(p *database.Portal) bool { return p.Receiver != "" }), nil
}

func isSpacePortal(p *database.Portal) bool {
	return p.RoomType == database.RoomTypeSpace || phonenum.IsSpaceID(string(p.ID))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
