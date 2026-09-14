package calls

import (
	"context"
	"fmt"
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

// reconcileTrunk makes sure the outbound trunk exists and caches its ID.
//
// livekit-sip stores trunk objects in Redis with no persistence guarantee, so a
// Redis restart drops them and every later CreateSIPParticipant fails. There is
// no event for that, so the trunk is checked at startup and then on a timer.
func (s *Subsystem) reconcileTrunk(ctx context.Context) error {
	if s.cfg.LiveKit.TrunkAddress == "" {
		return fmt.Errorf("livekit trunk_address is not configured")
	}
	trunks, err := s.lk.ListSIPOutboundTrunk(ctx)
	if err != nil {
		return fmt.Errorf("list outbound trunks: %w", err)
	}
	want := s.wantedTrunk()
	for _, t := range trunks {
		if t.Name == want.Name {
			s.trunkID.Store(&t.SipTrunkID)
			return nil
		}
	}
	created, err := s.lk.CreateSIPOutboundTrunk(ctx, want)
	if err != nil {
		return fmt.Errorf("create outbound trunk %q: %w", want.Name, err)
	}
	s.trunkID.Store(&created.SipTrunkID)
	s.log.Info().
		Str("trunk_id", created.SipTrunkID).
		Str("trunk_name", created.Name).
		Msg("Recreated LiveKit outbound trunk")
	return nil
}

// runTrunkReconciler reconciles once immediately and then on a timer.
func (s *Subsystem) runTrunkReconciler(ctx context.Context) {
	interval := s.cfg.LiveKit.TrunkReconcileInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	reconcile := func() {
		if err := s.reconcileTrunk(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn().Err(err).Msg("Failed to reconcile LiveKit outbound trunk")
		}
	}
	reconcile()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		}
	}
}

// currentTrunkID returns the cached trunk ID, or an error if the trunk has
// never been successfully reconciled.
func (s *Subsystem) currentTrunkID() (string, error) {
	id := s.trunkID.Load()
	if id == nil || *id == "" {
		return "", fmt.Errorf("no LiveKit outbound trunk available yet")
	}
	return *id, nil
}
