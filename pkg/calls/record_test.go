package calls

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// bodies returns the plain messages the ghost put in the room, which is what a
// user scrolling the portal sees.
func (f *fakeIntent) bodies() []string {
	var out []string
	for _, evt := range f.events() {
		if evt.Type != event.EventMessage {
			continue
		}
		if content, ok := evt.Content.Parsed.(*event.MessageEventContent); ok {
			out = append(out, content.Body)
		}
	}
	return out
}

// The record is the only thing a finished call leaves behind, so what it says
// has to distinguish the outcomes a caller acts on differently.
func TestCallRecordsSayWhatHappened(t *testing.T) {
	answered := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	now := answered.Add(80 * time.Second)
	for _, tc := range []struct {
		name string
		call database.Call
		end  callEnd
		want string
	}{
		{"missed", database.Call{Direction: database.DirectionInbound, State: database.StateRinging}, callEnd{}, "Missed call"},
		{"declined", database.Call{Direction: database.DirectionInbound, State: database.StateRinging}, callEnd{Declined: true}, "Call declined"},
		{"incoming", database.Call{Direction: database.DirectionInbound, State: database.StateBridged, UpdatedAt: answered}, callEnd{}, "Incoming call — 1m 20s"},
		{"outgoing", database.Call{Direction: database.DirectionOutbound, State: database.StateBridged, UpdatedAt: answered}, callEnd{}, "Outgoing call — 1m 20s"},
		{"unanswered outgoing", database.Call{Direction: database.DirectionOutbound, State: database.StateRinging}, callEnd{}, "Outgoing call — no answer"},
		{"failed", database.Call{Direction: database.DirectionInbound, State: database.StateRinging}, callEnd{Failure: "no route"}, "Call failed — no route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := callRecordBody(&tc.call, tc.end, now); got != tc.want {
				t.Errorf("the room was told %q, want %q", got, tc.want)
			}
		})
	}
}

// The outcome label is a closed set, and the counter and the room record are
// two readings of the same switch. A state or a callEnd shape that fell
// through it would produce an empty label -- a bucket nobody named, that no
// dashboard queries and that no alert can miss -- so every combination the
// code can reach is enumerated here rather than sampled.
func TestEveryCallHasAnOutcome(t *testing.T) {
	closed := map[string]bool{
		metrics.OutcomeAnswered: true,
		metrics.OutcomeMissed:   true,
		metrics.OutcomeDeclined: true,
		metrics.OutcomeFailed:   true,
		metrics.OutcomeStale:    true,
	}
	ends := map[string]callEnd{
		"plain":    {},
		"declined": {Declined: true},
		"failure":  {Failure: "no route"},
		"quiet":    {Quiet: true},
	}
	for _, direction := range []database.CallDirection{database.DirectionInbound, database.DirectionOutbound} {
		for _, state := range []database.CallState{database.StateRinging, database.StateBridged, database.StateEnded} {
			for name, end := range ends {
				call := database.Call{Direction: direction, State: state}
				got := callOutcome(&call, end)
				if !closed[got] {
					t.Errorf("%s/%s/%s has outcome %q, which is not in the closed set",
						direction, state, name, got)
				}
				// The record the room gets comes off the same switch, so a
				// combination that renders nothing is the same bug.
				if body := callRecordBody(&call, end, time.Now()); body == "" {
					t.Errorf("%s/%s/%s leaves the room no record", direction, state, name)
				}
			}
		}
	}
}

// A row discarded by bookkeeping long after its call ended says nothing about
// that call. Folding it into "missed" would make a teardown bug look like
// callers giving up, which is the one thing the counter would be consulted to
// rule out.
func TestAStaleRowIsItsOwnOutcome(t *testing.T) {
	call := database.Call{Direction: database.DirectionInbound, State: database.StateRinging}
	if got := callOutcome(&call, callEnd{Quiet: true}); got != metrics.OutcomeStale {
		t.Errorf("a stale row counted as %q, want %q", got, metrics.OutcomeStale)
	}
}

// The counter and the room record are two readings of one call, taken on the
// same path. This is the assertion that they are taken from the same switch:
// if they ever disagree, nothing in a dashboard says which of them is wrong.
func TestAMissedCallIsCountedAsTheRoomRecordsIt(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newHarness(t)
	h.metrics = metrics.New(reg)
	call := h.insertRinging(t)
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)

	leg := newFakeInboundLeg()
	leg.finish() // the caller gave up
	h.waitForMatrix(t.Context(), call, leg, zerolog.Nop(), waiters)

	if got := h.intent.bodies(); len(got) != 1 || got[0] != "Missed call" {
		t.Fatalf("the room was told %v, want one \"Missed call\"", got)
	}
	if got := callCount(t, reg, metrics.DirectionInbound, metrics.OutcomeMissed); got != 1 {
		t.Errorf("calls_total{inbound,missed} = %v, want 1", got)
	}
}

// callCount reads one series of sip_bridge_calls_total.
func callCount(t *testing.T, g prometheus.Gatherer, direction, outcome string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "sip_bridge_calls_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["direction"] == direction && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no calls_total{%s,%s} series", direction, outcome)
	return 0
}

// A call short enough to round to nothing still lasted; hours have to survive
// the minute field.
func TestCallDurationsAreReadableAtEveryScale(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{200 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h 2m 3s"},
	} {
		if got := formatCallDuration(tc.in); got != tc.want {
			t.Errorf("a %s call was reported as %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Nothing about a call reached the timeline before: a missed call left no
// trace, no unread and no history. It has to be a plain message, because
// .m.rule.suppress_notices drops the push for an m.notice.
func TestAMissedCallReachesTheTimeline(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)

	leg := newFakeInboundLeg()
	leg.finish() // the caller gave up
	h.waitForMatrix(t.Context(), call, leg, zerolog.Nop(), waiters)

	if got := h.intent.bodies(); len(got) != 1 || got[0] != "Missed call" {
		t.Fatalf("the room was told %v, want one \"Missed call\"", got)
	}
	for _, evt := range h.intent.events() {
		if evt.Type != event.EventMessage {
			continue
		}
		content, ok := evt.Content.Parsed.(*event.MessageEventContent)
		if !ok || content.MsgType != event.MsgText {
			t.Errorf("the record was sent as %v, want m.text or it gets no unread badge", content)
		}
	}
}

// An answered call is the one record that carries information beyond the fact
// it happened, and End overwrites the timestamp it is measured from.
func TestAnAnsweredCallRecordsItsDuration(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	if mine, err := h.db.Call.Transition(t.Context(), call, database.StateRinging, database.StateBridged); err != nil || !mine {
		t.Fatalf("Transition = %v, %v", mine, err)
	}
	// Back-date the answer so the record has a duration to report. How the
	// duration itself renders is TestCallRecordsSayWhatHappened's job, on a
	// fixed clock: asserting an exact second here would only measure how long
	// the test took.
	call.UpdatedAt = call.UpdatedAt.Add(-30 * time.Second)

	if err := h.endCall(t.Context(), call); err != nil {
		t.Fatalf("endCall: %v", err)
	}
	got := h.intent.bodies()
	if len(got) != 1 || !strings.HasPrefix(got[0], "Incoming call — 3") {
		t.Fatalf("the room was told %v, want one answered incoming call of about 30s", got)
	}
}

// A rejection is not an absence, and the room is the only place the user can
// see afterwards which of the two it was.
func TestADeclinedCallIsRecordedAsDeclined(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)
	h.rememberNotification("$ring", call.CallID)
	if _, ok := h.signalDecline("$ring"); !ok {
		t.Fatal("the decline was not matched to the call it names")
	}

	leg := newFakeInboundLeg()
	h.waitForMatrix(t.Context(), call, leg, zerolog.Nop(), waiters)

	if got := h.intent.bodies(); len(got) != 1 || got[0] != "Call declined" {
		t.Fatalf("the room was told %v, want one \"Call declined\"", got)
	}
}

// A call that fails after ringing left the user with a phone that rang and
// then silence: the failure was only ever in the bridge log.
func TestAFailedCallIsReportedInTheRoom(t *testing.T) {
	h := newHarness(t)
	h.trunkID.Store(nil)

	leg := newFakeInboundLeg()
	h.HandleInboundCall(t.Context(), leg)

	if got := h.intent.bodies(); len(got) != 1 || got[0] != "Call failed — no route" {
		t.Fatalf("the room was told %v, want one \"Call failed — no route\"", got)
	}
}

// Every category is on by default so that a real week of behaviour decides
// which stay loud, and each has to be switchable to log-only without a code
// change.
func TestNoticeCategoriesCanBeSwitchedOff(t *testing.T) {
	off := false

	t.Run("ended", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Notices.CallEnded = &off
		call := h.insertRinging(t)
		if err := h.endCall(t.Context(), call); err != nil {
			t.Fatalf("endCall: %v", err)
		}
		if got := h.intent.bodies(); len(got) != 0 {
			t.Errorf("the room was told %v with call_ended off", got)
		}
	})

	t.Run("failed", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Notices.CallFailed = &off
		call := h.insertRinging(t)
		if err := h.failCall(t.Context(), call, "no route"); err != nil {
			t.Fatalf("failCall: %v", err)
		}
		if got := h.intent.bodies(); len(got) != 0 {
			t.Errorf("the room was told %v with call_failed off", got)
		}
	})

	t.Run("on by default", func(t *testing.T) {
		var cfg NoticeConfig
		if !cfg.callEnded() || !cfg.callFailed() {
			t.Error("an absent notices block switched a category off")
		}
	})
}

// A row discarded hours after the call it describes must not announce itself:
// the number was out of service, not called.
func TestAStaleRowIsDiscardedSilently(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	call.CreatedAt = time.Now().Add(-24 * time.Hour)

	got, err := h.discardIfStale(t.Context(), call, nil)
	if err != nil || got != nil {
		t.Fatalf("discardIfStale = %v, %v; want the row discarded", got, err)
	}
	if bodies := h.intent.bodies(); len(bodies) != 0 {
		t.Errorf("the room was told %v about a call that ended a day ago", bodies)
	}
}

// Only the missing trunk is a fault the reader can act on; everything else is
// this one call not being takeable.
func TestOnlyTheMissingTrunkIsNamedAsNoRoute(t *testing.T) {
	if got := mediaFailureReason(errors.New("create SIP participant: 500")); got != "could not connect" {
		t.Errorf("a LiveKit failure was reported as %q", got)
	}
	if got := mediaFailureReason(errors.New("x: " + ErrNoTrunk.Error())); got == "no route" {
		t.Error("a message that merely looks like the trunk error was reported as no route")
	}
	if got := mediaFailureReason(errors.Join(errors.New("bridge media"), ErrNoTrunk)); got != "no route" {
		t.Errorf("a wrapped missing trunk was reported as %q, want no route", got)
	}
}
