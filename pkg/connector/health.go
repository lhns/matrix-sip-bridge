package connector

import (
	"sync"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

// Bridge state error codes. They are the machine-readable half of the notice
// that reaches the management room, so they are stable strings.
const (
	errSIPNotRegistered = status.BridgeStateErrorCode("sip-not-registered")
	errTrunkUnavailable = status.BridgeStateErrorCode("livekit-trunk-unavailable")
)

// callHealth holds the two facts that decide whether the bridge can carry a
// call at all, and turns a change in either into one bridge state.
//
// Both are reported as TRANSIENT_DISCONNECT rather than an error state, which
// with bridge_status_notices: errors is what makes a blip silent and a
// sustained outage reach the management room after three minutes. An error
// state would also make bridgev2 tear the login down and rebuild it, which
// fixes neither a dropped registration nor a missing trunk.
type callHealth struct {
	notices calls.NoticeConfig

	mu       sync.Mutex
	sipDown  bool
	trunkErr string
	last     status.BridgeState
	reported bool
}

func newCallHealth(notices calls.NoticeConfig) *callHealth {
	return &callHealth{notices: notices}
}

// SIP records whether the SIP endpoint is usable and returns the state to send,
// or false if nothing a Matrix user can see has changed.
func (h *callHealth) SIP(ready bool) (status.BridgeState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sipDown = !ready
	return h.changed()
}

// Trunk records the outcome of a trunk reconcile the same way.
func (h *callHealth) Trunk(err error) (status.BridgeState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trunkErr = ""
	if err != nil {
		h.trunkErr = err.Error()
	}
	return h.changed()
}

// changed computes the state the facts imply and reports whether it differs
// from the last one sent. Without the comparison a poll loop would re-send the
// same state forever.
func (h *callHealth) changed() (status.BridgeState, bool) {
	state := h.state()
	// Compared field by field: BridgeState carries a map and a timestamp, so
	// it is neither comparable nor equal to itself across two sends.
	if h.reported && state.StateEvent == h.last.StateEvent &&
		state.Error == h.last.Error && state.Message == h.last.Message {
		return status.BridgeState{}, false
	}
	h.last = state
	h.reported = true
	return state, true
}

// state ranks the faults: without a SIP endpoint no call happens in either
// direction, where a missing trunk still lets an inbound INVITE arrive.
//
// A category switched off in the config is not merely unreported, it does not
// enter the state at all -- the point of the switch is log-only, and a state
// that flapped without ever being sent would still suppress the recovery.
func (h *callHealth) state() status.BridgeState {
	switch {
	case h.sipDown && h.notices.SIPDownEnabled():
		return status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      errSIPNotRegistered,
			Message:    "The SIP endpoint is not listening or not registered; no call can be bridged",
		}
	case h.trunkErr != "" && h.notices.TrunkMissingEnabled():
		return status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      errTrunkUnavailable,
			// The trunk error names fields and the trunk, never its password;
			// see trunkDiff.
			Message: "The LiveKit outbound trunk is unusable: " + h.trunkErr,
		}
	default:
		return status.BridgeState{StateEvent: status.StateConnected}
	}
}
