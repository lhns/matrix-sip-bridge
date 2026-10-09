package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// lineSpaceID is the space a portal belongs in, false for one that has none:
// a space itself, or a number with no line.
func lineSpaceID(portalID string) (networkid.PortalID, bool) {
	if phonenum.IsSpaceID(portalID) {
		return "", false
	}
	line := phonenum.LineFromDialID(portalID)
	if line == "" {
		line = phonenum.LineFromID(portalID)
	}
	space, err := phonenum.SpaceIDFor(line)
	if err != nil {
		return "", false
	}
	return networkid.PortalID(space), true
}

// lineParent puts a portal into its line's space under the portal's own
// receiver.
//
// It replaces ChatInfo.ParentID, which bridgev2 keys without a receiver unless
// bridge.split_portals is on (ADR-0016 keeps it off): every receiver's room
// would name one shared, receiver-less space, created on the first resync.
// What follows is bridgev2's updateParent with the receiver kept.
func (sc *SIPClient) lineParent(space networkid.PortalID) bridgev2.ExtraUpdater[*bridgev2.Portal] {
	return func(ctx context.Context, portal *bridgev2.Portal) bool {
		want := networkid.PortalKey{ID: space, Receiver: portal.Receiver}
		keyed := portal.ParentKey == want
		if keyed && portal.MXID == "" {
			return false
		}
		br := portal.Bridge
		log := zerolog.Ctx(ctx)
		parent, err := br.GetPortalByKey(ctx, want)
		if err != nil {
			log.Err(err).Object("parent_key", want).Msg("Failed to get the line's space")
			return false
		}
		// Keyed is not linked: creating the space or listing the room may have
		// failed, and only a resync that finds it unlinked retries.
		if keyed && portal.InSpace && parent.MXID != "" {
			return false
		}
		if !keyed && portal.MXID != "" && portal.InSpace && portal.Parent != nil && portal.Parent.MXID != "" {
			if err := linkToParent(ctx, br, portal.Parent.MXID, portal.MXID, true); err != nil {
				log.Err(err).Stringer("old_space_mxid", portal.Parent.MXID).Msg("Failed to remove the room from its old space")
			}
		}
		portal.ParentKey = want
		portal.Parent = parent
		portal.InSpace = false
		// A room still being created gets the parent from bridgev2, which
		// reads portal.Parent once the room exists.
		if portal.MXID == "" {
			return true
		}
		if parent.MXID == "" {
			if err := parent.CreateMatrixRoom(ctx, sc.UserLogin, nil); err != nil {
				log.Err(err).Msg("Failed to create the line's space")
				return true
			}
		}
		if err := linkToParent(ctx, br, parent.MXID, portal.MXID, false); err != nil {
			log.Err(err).Stringer("space_mxid", parent.MXID).Msg("Failed to add the room to its line's space")
		} else {
			portal.InSpace = true
		}
		return true
	}
}

// setSpaceChild lists child in space, or removes it.
func setSpaceChild(ctx context.Context, br *bridgev2.Bridge, space, child id.RoomID, remove bool) error {
	content := &event.SpaceChildEventContent{}
	if !remove {
		content.Via = []string{br.Matrix.ServerName()}
	}
	_, err := br.Bot.SendState(ctx, space, event.StateSpaceChild, child.String(), &event.Content{Parsed: content}, time.Now())
	return err
}

// linkToParent is setSpaceChild plus the child's canonical m.space.parent, as
// bridgev2 links a portal into its parent.
func linkToParent(ctx context.Context, br *bridgev2.Bridge, parent, child id.RoomID, remove bool) error {
	if err := setSpaceChild(ctx, br, parent, child, remove); err != nil {
		return err
	}
	content := &event.SpaceParentEventContent{Canonical: !remove}
	if !remove {
		content.Via = []string{br.Matrix.ServerName()}
	}
	_, err := br.Bot.SendState(ctx, child, event.StateSpaceParent, parent.String(), &event.Content{Parsed: content}, time.Now())
	return err
}

// ensureInPersonalSpace lists a parentless portal in the login's personal
// filtering space. force sends the entry even where user_portal says it is
// there already, which a repaired space cannot trust.
func ensureInPersonalSpace(ctx context.Context, login *bridgev2.UserLogin, portal *bridgev2.Portal, force bool) error {
	up, err := login.Bridge.DB.UserPortal.GetOrCreate(ctx, login.UserLogin, portal.PortalKey)
	if err != nil {
		return fmt.Errorf("get user portal: %w", err)
	}
	if !force && up.InSpace != nil && *up.InSpace {
		return nil
	}
	return login.AddPortalToSpace(ctx, portal, up.CopyWithoutValues())
}

// reparentChildrenQuery points a receiver's rooms at that receiver's own copy
// of a space they still name receiver-less.
const reparentChildrenQuery = `
	UPDATE portal SET parent_receiver=$3
	WHERE bridge_id=$1 AND parent_id=$2 AND parent_receiver='' AND receiver=$3`

// reparentChildren points a space's receiver's rooms at it, in the database
// and in bridgev2's cache: a child cached with the old parent key would write
// it back on its next save.
func reparentChildren(ctx context.Context, br *bridgev2.Bridge, key networkid.PortalKey) error {
	if _, err := br.DB.Exec(ctx, reparentChildrenQuery, br.ID, key.ID, key.Receiver); err != nil {
		return fmt.Errorf("re-parent children of %s: %w", key, err)
	}
	space, err := br.GetPortalByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("get space %s: %w", key, err)
	}
	children, err := br.GetChildPortals(ctx, key)
	if err != nil {
		return fmt.Errorf("get children of %s: %w", key, err)
	}
	for _, child := range children {
		child.ParentKey = key
		child.Parent = space
	}
	return nil
}

// repairLineSpaces re-keys a receiver-less line space whose rooms all belong
// to one receiver onto that receiver.
//
// Such a space is what ChatInfo.ParentID created before lineParent replaced
// it: the receiver's rooms were moved into it, and the receiver's own space
// was left behind, empty. That one is dropped (its Matrix room is not touched)
// so the one holding the rooms can take its key. A space whose rooms belong to
// several receivers, or whose receiver's own space still has rooms, is left
// alone with a warning: there is no way to tell which one is meant.
//
// Idempotent: once re-keyed, no receiver-less space has a receiver's rooms.
func (sc *SIPConnector) repairLineSpaces(ctx context.Context) error {
	br := sc.br
	log := br.Log.With().Str("action", "repair line spaces").Logger()
	ctx = log.WithContext(ctx)
	portals, err := portalsWithoutReceiver(ctx, br)
	if err != nil {
		return err
	}
	for _, src := range portals {
		if !isSpacePortal(src) {
			continue
		}
		children, err := br.DB.Portal.GetChildren(ctx, src.PortalKey)
		if err != nil {
			return fmt.Errorf("children of %s: %w", src.ID, err)
		}
		receiver, ok := soleReceiver(children)
		if !ok {
			if receiver != "" {
				log.Warn().Str("portal_id", string(src.ID)).Msg("A receiver-less space holds rooms of several receivers; leaving it alone")
			}
			continue
		}
		slog := log.With().Str("portal_id", string(src.ID)).Str("receiver", string(receiver)).Logger()
		login, err := br.GetExistingUserLoginByID(ctx, receiver)
		if err != nil {
			return fmt.Errorf("get login %s: %w", receiver, err)
		} else if login == nil {
			slog.Warn().Msg("A receiver-less space holds the rooms of a login that does not exist; leaving it alone")
			continue
		}
		target := networkid.PortalKey{ID: src.ID, Receiver: receiver}
		var dead id.RoomID
		if stale, err := br.DB.Portal.GetByKey(ctx, target); err != nil {
			return fmt.Errorf("get %s: %w", target, err)
		} else if stale != nil {
			if staleChildren, err := br.DB.Portal.GetChildren(ctx, target); err != nil {
				return fmt.Errorf("children of %s: %w", target, err)
			} else if len(staleChildren) > 0 {
				slog.Warn().Stringer("room", src.MXID).Stringer("other_room", stale.MXID).
					Msg("Both the receiver-less space and the receiver's own hold rooms; leaving them alone")
				continue
			}
			portal, err := br.GetPortalByKey(ctx, target)
			if err != nil {
				return fmt.Errorf("get %s: %w", target, err)
			}
			if err := portal.Delete(ctx); err != nil {
				return fmt.Errorf("drop the empty space %s: %w", target, err)
			}
			dead = stale.MXID
			slog.Info().Stringer("room", dead).Msg("Dropped the receiver's empty space row; its Matrix room is left as it is")
		}
		if !sc.reIDPortal(ctx, src, receiver) {
			continue
		}
		space, err := br.GetPortalByKey(ctx, target)
		if err != nil {
			return fmt.Errorf("get %s: %w", target, err)
		}
		if err := ensureInPersonalSpace(ctx, login, space, true); err != nil {
			slog.Warn().Err(err).Msg("Could not list the space in the personal space")
		}
		if dead != "" && login.SpaceRoom != "" {
			if err := setSpaceChild(ctx, br, login.SpaceRoom, dead, true); err != nil {
				slog.Warn().Err(err).Stringer("room", dead).Msg("Could not remove the dropped space from the personal space")
			}
		}
		slog.Info().Stringer("room", src.MXID).Int("rooms", len(children)).Msg("Moved a receiver-less line space to its receiver")
	}
	return nil
}

// soleReceiver is the one receiver every portal has. ok is false for none or
// several; receiver is then the first non-empty one, if any.
func soleReceiver(portals []*database.Portal) (receiver networkid.UserLoginID, ok bool) {
	for _, p := range portals {
		switch {
		case p.Receiver == "":
			return receiver, false
		case receiver == "":
			receiver = p.Receiver
		case p.Receiver != receiver:
			return receiver, false
		}
	}
	return receiver, receiver != ""
}
