package connector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
)

// keyLineMembers is the bridgev2 KV key holding the line_members of the last
// start, to tell who was removed.
const keyLineMembers = database.Key("sip_line_members")

// memberEntries flattens line_members into sorted "<line> <mxid>" entries.
func memberEntries(members map[string][]id.UserID) []string {
	var out []string
	for line, users := range members {
		for _, u := range users {
			out = append(out, line+" "+string(u))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ensureLineMemberLogins gives every permitted member of a line a login,
// before bridgev2 connects the logins; their rooms follow in Connect.
//
// A member who is gone from the config keeps their login and rooms. Removing
// them would delete rooms, so it is only logged.
func (sc *SIPConnector) ensureLineMemberLogins(ctx context.Context) {
	br := sc.br
	log := br.Log.With().Str("action", "line members").Logger()
	current := memberEntries(sc.Config.LineMembers)
	if previous := br.DB.KV.Get(ctx, keyLineMembers); previous != "" {
		for _, entry := range strings.Split(previous, "\n") {
			if !slices.Contains(current, entry) {
				line, user, _ := strings.Cut(entry, " ")
				log.Info().Str("line", line).Str("user_id", user).
					Msg("No longer a member of the line; their rooms on it are kept")
			}
		}
	}
	for _, entry := range current {
		line, user, _ := strings.Cut(entry, " ")
		mxid := id.UserID(user)
		mlog := log.With().Str("line", line).Stringer("user_id", mxid).Logger()
		if _, err := phonenum.SpaceIDFor(line); err != nil {
			mlog.Warn().Err(err).Msg("Skipping a line member: the line name is not usable")
			continue
		}
		if !isUserID(mxid) {
			mlog.Warn().Msg("Skipping a line member that is not a Matrix user ID")
			continue
		}
		if _, _, err := sc.logins.ensure(ctx, mxid, true); errors.Is(err, calls.ErrRecipientRefused) {
			mlog.Warn().Msg("Skipping a line member that bridge.permissions does not let log in")
		} else if err != nil {
			mlog.Warn().Err(err).Msg("Could not create a line member's login")
		}
	}
	br.DB.KV.Set(ctx, keyLineMembers, strings.Join(current, "\n"))
}

// ensureLineRooms creates the space and dial room of every line this login's
// user is a member of, and lists the space in their personal space. Existing
// rooms are not touched beyond that: re-inviting the user to a room they left
// on every restart is what resyncPortals avoids too.
func (sc *SIPClient) ensureLineRooms(ctx context.Context) {
	// The old shared login, kept while the migration could not finish, is the
	// owner's too; its rooms would be a second set.
	if sc.UserLogin.ID == legacyLoginID {
		return
	}
	log := sc.UserLogin.Log.With().Str("action", "ensure line rooms").Logger()
	ctx = log.WithContext(ctx)
	for _, line := range sc.conn.Config.linesOf(sc.UserLogin.UserMXID) {
		if err := sc.ensureLineRoom(ctx, line); err != nil {
			log.Warn().Err(err).Str("line", line).Msg("Could not set up the line's rooms")
		}
	}
}

func (sc *SIPClient) ensureLineRoom(ctx context.Context, line string) error {
	if sc.conn.Config.LineSpaces {
		spaceID, err := phonenum.SpaceIDFor(line)
		if err != nil {
			return err
		}
		space, err := sc.ensureRoom(ctx, spaceID)
		if err != nil {
			return fmt.Errorf("the space: %w", err)
		}
		if err := ensureInPersonalSpace(ctx, sc.UserLogin, space, false); err != nil {
			return fmt.Errorf("list the space in the personal space: %w", err)
		}
	}
	dialID, err := phonenum.DialIDFor(line)
	if err != nil {
		return err
	}
	if _, err := sc.ensureRoom(ctx, dialID); err != nil {
		return fmt.Errorf("the dial room: %w", err)
	}
	return nil
}
