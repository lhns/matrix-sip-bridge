package connector

import (
	"errors"
	"testing"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

// BridgeState was sent exactly once, in Connect, and never updated: a dropped
// registration was invisible until a call failed.
func TestBridgeStateFollowsTheSIPEndpoint(t *testing.T) {
	h := newCallHealth(calls.NoticeConfig{})

	state, changed := h.SIP(true)
	if !changed || state.StateEvent != status.StateConnected {
		t.Fatalf("a usable endpoint reported %v, %v; want CONNECTED", state.StateEvent, changed)
	}
	if _, changed := h.SIP(true); changed {
		t.Error("an unchanged endpoint re-sent its state; a poll loop would send it forever")
	}
	state, changed = h.SIP(false)
	if !changed || state.StateEvent != status.StateTransientDisconnect || state.Error != errSIPNotRegistered {
		t.Fatalf("a dropped registration reported %v/%v, %v", state.StateEvent, state.Error, changed)
	}
	state, changed = h.SIP(true)
	if !changed || state.StateEvent != status.StateConnected {
		t.Errorf("a recovered registration reported %v, %v; want CONNECTED", state.StateEvent, changed)
	}
}

// A trunk lost with Redis makes every call fail with no other symptom.
func TestBridgeStateReportsAnUnusableTrunk(t *testing.T) {
	h := newCallHealth(calls.NoticeConfig{})
	if _, changed := h.SIP(true); !changed {
		t.Fatal("the first state was not reported")
	}

	state, changed := h.Trunk(errors.New("list outbound trunks: connection refused"))
	if !changed || state.StateEvent != status.StateTransientDisconnect || state.Error != errTrunkUnavailable {
		t.Fatalf("a missing trunk reported %v/%v, %v", state.StateEvent, state.Error, changed)
	}
	if state.Message == "" || state.Message == "The LiveKit outbound trunk is unusable: " {
		t.Errorf("the state said %q, which does not say why", state.Message)
	}
	state, changed = h.Trunk(nil)
	if !changed || state.StateEvent != status.StateConnected {
		t.Errorf("a reconciled trunk reported %v, %v; want CONNECTED", state.StateEvent, changed)
	}
}

// Both faults are transient states rather than error ones, so that with
// bridge_status_notices: errors a blip stays quiet and only a sustained outage
// reaches the management room. An error state would also make bridgev2 tear
// the login down and rebuild it, which fixes neither fault.
func TestNeitherFaultIsReportedAsAnErrorState(t *testing.T) {
	h := newCallHealth(calls.NoticeConfig{})
	sip, _ := h.SIP(false)
	if _, changed := h.SIP(true); !changed {
		t.Fatal("the recovery was not reported")
	}
	trunk, _ := h.Trunk(errors.New("no trunk"))
	for _, state := range []status.BridgeState{sip, trunk} {
		if state.StateEvent != status.StateTransientDisconnect {
			t.Errorf("%v: bridgev2 treats anything else as an error to rebuild the login over", state.StateEvent)
		}
	}
}

// Without a SIP endpoint no call happens in either direction, where a missing
// trunk still lets an inbound INVITE arrive: reporting the lesser fault would
// send the reader after the wrong thing.
func TestTheSIPEndpointOutranksTheTrunk(t *testing.T) {
	h := newCallHealth(calls.NoticeConfig{})
	if _, changed := h.Trunk(errors.New("no trunk")); !changed {
		t.Fatal("the trunk failure was not reported")
	}
	state, changed := h.SIP(false)
	if !changed || state.Error != errSIPNotRegistered {
		t.Fatalf("with both broken the state said %v, %v; want the endpoint", state.Error, changed)
	}
	state, _ = h.SIP(true)
	if state.Error != errTrunkUnavailable {
		t.Errorf("with the endpoint back the state said %v; the trunk is still broken", state.Error)
	}
}

// Every category is on by default; one that proves noisy has to become
// log-only without a code change.
func TestNoticeCategoriesGateTheBridgeState(t *testing.T) {
	off := false

	t.Run("sip_down", func(t *testing.T) {
		h := newCallHealth(calls.NoticeConfig{SIPDown: &off})
		if _, changed := h.SIP(true); !changed {
			t.Fatal("the first state was not reported")
		}
		if state, changed := h.SIP(false); changed {
			t.Errorf("a dropped registration still reported %v with sip_down off", state.StateEvent)
		}
	})

	t.Run("trunk_missing", func(t *testing.T) {
		h := newCallHealth(calls.NoticeConfig{TrunkMissing: &off})
		if _, changed := h.SIP(true); !changed {
			t.Fatal("the first state was not reported")
		}
		if state, changed := h.Trunk(errors.New("no trunk")); changed {
			t.Errorf("a missing trunk still reported %v with trunk_missing off", state.StateEvent)
		}
	})

	t.Run("on by default", func(t *testing.T) {
		var cfg calls.NoticeConfig
		if !cfg.SIPDownEnabled() || !cfg.TrunkMissingEnabled() {
			t.Error("an absent notices block switched a category off")
		}
	})
}
