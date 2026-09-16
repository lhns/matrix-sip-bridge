package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// families is what /metrics has to carry before anything has happened, and how
// many series each family is pre-initialised with.
var families = map[string]int{
	"sip_bridge_calls_total":                  10, // 2 directions x 5 outcomes
	"sip_bridge_call_setup_seconds":           2,  // 2 directions
	"sip_bridge_sip_registered":               1,
	"sip_bridge_trunk_reconcile_total":        2,
	"sip_bridge_messages_total":               9, // 5 inbound outcomes + 4 outbound
	"sip_bridge_start_timestamp_seconds":      1,
	"sip_bridge_sip_listen_timestamp_seconds": 1,
}

// A family that only appears once it has fired cannot answer "has this stopped
// happening?", which is the question these metrics exist for: the interesting
// ones -- a carrier refusing every text, a sender that never parses -- are
// rare, and their absence is indistinguishable from health until the series
// exists.
func TestEveryFamilyIsPresentAtZeroBeforeAnythingHappens(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := New(reg)

	got, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]int{}
	for _, mf := range got {
		seen[mf.GetName()] = len(mf.GetMetric())
	}
	for name, want := range families {
		if seen[name] != want {
			t.Errorf("%s has %d series, want %d", name, seen[name], want)
		}
	}
	for _, direction := range []string{DirectionInbound, DirectionOutbound} {
		for _, outcome := range callOutcomes {
			if v := testutil.ToFloat64(r.calls.WithLabelValues(direction, outcome)); v != 0 {
				t.Errorf("calls{%s,%s} starts at %v, want 0", direction, outcome, v)
			}
		}
	}
	// The listener has not bound, which is exactly what a bridge that is
	// running but deaf looks like.
	if v := testutil.ToFloat64(r.sipListen); v != 0 {
		t.Errorf("sip_listen_timestamp = %v before the listener bound, want 0", v)
	}
	if v := testutil.ToFloat64(r.start); v <= 0 {
		t.Errorf("start_timestamp = %v, want the process start time", v)
	}
}

// Every collector is optional: pkg/calls and pkg/siptransport are built
// without one by most of their tests, and a Recorder that panicked on nil
// would make instrumentation something every caller has to opt into.
func TestANilRecorderRecordsNothingAndDoesNotPanic(t *testing.T) {
	var r *Recorder
	r.Call(DirectionInbound, OutcomeAnswered)
	r.CallSetup(DirectionOutbound, time.Second)
	r.SIPRegistered(true)
	r.TrunkReconcile(errors.New("no trunk"))
	r.Message(DirectionOutbound, MessageRejected)
	r.SIPListening()
}

func TestEventsMoveTheirOwnSeries(t *testing.T) {
	r := New(prometheus.NewRegistry())

	r.Call(DirectionInbound, OutcomeDeclined)
	r.Call(DirectionInbound, OutcomeDeclined)
	if v := testutil.ToFloat64(r.calls.WithLabelValues(DirectionInbound, OutcomeDeclined)); v != 2 {
		t.Errorf("declined inbound calls = %v, want 2", v)
	}
	if v := testutil.ToFloat64(r.calls.WithLabelValues(DirectionOutbound, OutcomeDeclined)); v != 0 {
		t.Errorf("declined outbound calls = %v, want 0", v)
	}

	r.TrunkReconcile(nil)
	r.TrunkReconcile(errors.New("list outbound trunks"))
	if v := testutil.ToFloat64(r.trunk.WithLabelValues(TrunkOK)); v != 1 {
		t.Errorf("ok reconciles = %v, want 1", v)
	}
	if v := testutil.ToFloat64(r.trunk.WithLabelValues(TrunkError)); v != 1 {
		t.Errorf("failed reconciles = %v, want 1", v)
	}

	r.Message(DirectionOutbound, MessageRejected)
	if v := testutil.ToFloat64(r.messages.WithLabelValues(DirectionOutbound, MessageRejected)); v != 1 {
		t.Errorf("rejected outbound messages = %v, want 1", v)
	}

	r.CallSetup(DirectionInbound, 3*time.Second)
	if n := testutil.CollectAndCount(r.callSetup); n != 2 {
		t.Errorf("call setup series = %d, want 2", n)
	}
}

// The gauge must not latch, unlike the readiness endpoint. Readiness latches
// so that a registration dropped hours after startup does not take the pod out
// of its Service; this gauge is where that tolerated fault stays visible.
func TestSIPRegisteredFollowsTheEndpointBothWays(t *testing.T) {
	r := New(prometheus.NewRegistry())
	r.SIPRegistered(true)
	if v := testutil.ToFloat64(r.registered); v != 1 {
		t.Fatalf("sip_registered = %v after the endpoint came up, want 1", v)
	}
	r.SIPRegistered(false)
	if v := testutil.ToFloat64(r.registered); v != 0 {
		t.Errorf("sip_registered = %v after the registration dropped, want 0", v)
	}
}

// The cardinality rule at the top of this package is only as good as the label
// values the code can produce. Everything exposed here has to come from one of
// the constant sets, so that no call site can invent a value -- a number, a
// room ID, a SIP status -- that the twelve-month store would then keep.
func TestLabelValuesComeFromTheClosedSets(t *testing.T) {
	allowed := map[string]bool{}
	for _, v := range append(append([]string{DirectionInbound, DirectionOutbound, TrunkOK, TrunkError}, callOutcomes...),
		append(inboundMessageOutcomes, outboundMessageOutcomes...)...) {
		allowed[v] = true
	}
	reg := prometheus.NewRegistry()
	New(reg)
	got, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range got {
		for _, m := range mf.GetMetric() {
			for _, label := range m.GetLabel() {
				if !allowed[label.GetValue()] {
					t.Errorf("%s has label %s=%q, which is not in any closed set",
						mf.GetName(), label.GetName(), label.GetValue())
				}
			}
		}
	}
}
