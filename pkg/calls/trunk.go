package calls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// wantedTrunk is the trunk object the bridge expects LiveKit to hold.
func (s *Subsystem) wantedTrunk() *SIPOutboundTrunk {
	t := &SIPOutboundTrunk{
		Name:         s.cfg.LiveKit.TrunkName,
		Address:      s.cfg.LiveKit.TrunkAddress,
		AuthUsername: s.cfg.LiveKit.TrunkAuthUsername,
		AuthPassword: s.cfg.LiveKit.TrunkAuthPassword,
	}
	if s.cfg.LiveKit.TrunkNumber != "" {
		t.Numbers = []string{s.cfg.LiveKit.TrunkNumber}
	}
	return t
}

// trunkFingerprint identifies a wanted trunk spec without exposing it. It
// covers the password, so it must never be logged or put in an error.
func trunkFingerprint(t *SIPOutboundTrunk) string {
	h := sha256.New()
	for _, f := range append([]string{t.Name, t.Address, t.AuthUsername, t.AuthPassword}, t.Numbers...) {
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// trunkDiff names the fields in which LiveKit's copy differs from the wanted
// one. Names only: the values include a password.
func trunkDiff(have, want *SIPOutboundTrunk) []string {
	var diff []string
	if have.Address != want.Address {
		diff = append(diff, "address")
	}
	if !slices.Equal(have.Numbers, want.Numbers) {
		diff = append(diff, "numbers")
	}
	if have.AuthUsername != want.AuthUsername {
		diff = append(diff, "auth_username")
	}
	if have.AuthPassword != want.AuthPassword {
		diff = append(diff, "auth_password")
	}
	return diff
}

// reconcileTrunk makes sure the outbound trunk exists, matches the config and
// caches its ID.
//
// livekit-sip stores trunk objects in Redis with no persistence guarantee, so a
// Redis restart drops them and every later CreateSIPParticipant fails. There is
// no event for that, so the trunk is checked at startup and then on a timer.
//
// Matching on the name alone is not enough: a trunk created before the SIP
// credentials were configured keeps failing every call with "sip server
// required auth, but no username or password was provided" until the stored
// object itself is corrected.
func (s *Subsystem) reconcileTrunk(ctx context.Context) error {
	if s.cfg.LiveKit.TrunkAddress == "" {
		return fmt.Errorf("livekit trunk_address is not configured")
	}
	trunks, err := s.lk.ListSIPOutboundTrunk(ctx)
	if err != nil {
		return fmt.Errorf("list outbound trunks: %w", err)
	}
	want := s.wantedTrunk()
	fp := trunkFingerprint(want)
	for _, t := range trunks {
		if t.Name != want.Name {
			continue
		}
		s.trunkID.Store(&t.SipTrunkID)
		diff := trunkDiff(t, want)
		if len(diff) == 0 {
			s.trunkPushed.Store(&fp)
			return nil
		}
		// A deployment that withholds auth_password from List reports a diff
		// no write can ever close. Once this exact spec has been pushed to
		// this trunk, a password-only diff against a blank is that, not drift.
		if len(diff) == 1 && diff[0] == "auth_password" && t.AuthPassword == "" {
			if pushed := s.trunkPushed.Load(); pushed != nil && *pushed == fp {
				return nil
			}
		}
		// UpdateSIPOutboundTrunk's replace action swaps the whole object and
		// keeps the trunk ID, so a call already dialling through this trunk
		// keeps a valid ID and picks the new settings up on its next INVITE.
		updated, err := s.lk.UpdateSIPOutboundTrunk(ctx, t.SipTrunkID, want)
		if err != nil {
			return fmt.Errorf("update outbound trunk %q: %w", want.Name, err)
		}
		s.trunkID.Store(&updated.SipTrunkID)
		s.trunkPushed.Store(&fp)
		s.log.Info().
			Str("trunk_id", updated.SipTrunkID).
			Str("trunk_name", updated.Name).
			Str("fields", strings.Join(diff, ",")).
			Msg("Updated LiveKit outbound trunk to match the config")
		return nil
	}
	created, err := s.lk.CreateSIPOutboundTrunk(ctx, want)
	if err != nil {
		return fmt.Errorf("create outbound trunk %q: %w", want.Name, err)
	}
	s.trunkID.Store(&created.SipTrunkID)
	s.trunkPushed.Store(&fp)
	s.log.Info().
		Str("trunk_id", created.SipTrunkID).
		Str("trunk_name", created.Name).
		Msg("Recreated LiveKit outbound trunk")
	return nil
}

// DefaultTrunkReconcileInterval is how often the trunk is re-checked when the
// config does not say. It is a compromise: the trunk only disappears when
// livekit-sip's Redis is restarted, and the first call after that fails
// whatever this is, so a shorter interval buys a faster recovery and nothing
// else. The connector applies it to the config; runTrunkReconciler repeats it
// for a subsystem built without one.
const DefaultTrunkReconcileInterval = 5 * time.Minute

// runTrunkReconciler reconciles once immediately and then on a timer.
func (s *Subsystem) runTrunkReconciler(ctx context.Context) {
	interval := s.cfg.LiveKit.TrunkReconcileInterval
	if interval <= 0 {
		interval = DefaultTrunkReconcileInterval
	}
	s.reconcileAndReport(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileAndReport(ctx)
		}
	}
}

// reconcileAndReport is one reconcile plus its outcome.
//
// Both halves are reported, not just the failures: the bridge state it feeds
// has to be able to say the trunk came back, and nothing else ever will.
func (s *Subsystem) reconcileAndReport(ctx context.Context) {
	err := s.reconcileTrunk(ctx)
	if ctx.Err() != nil {
		// Shutdown cancelled it; that is not a fault to report.
		return
	}
	if err != nil {
		s.log.Warn().Err(err).Msg("Failed to reconcile LiveKit outbound trunk")
	}
	if fn := s.onTrunkState.Load(); fn != nil {
		(*fn)(err)
	}
}

// ErrNoTrunk is the one media failure with a cause worth telling the room
// about: without a trunk no call can be routed at all, and it is what a
// dropped Redis leaves behind.
var ErrNoTrunk = errors.New("no LiveKit outbound trunk available yet")

// currentTrunkID returns the cached trunk ID, or an error if the trunk has
// never been successfully reconciled.
func (s *Subsystem) currentTrunkID() (string, error) {
	id := s.trunkID.Load()
	if id == nil || *id == "" {
		return "", ErrNoTrunk
	}
	return *id, nil
}
