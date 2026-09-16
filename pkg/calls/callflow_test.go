package calls

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// Until a provisional response arrives the caller hears nothing, and building
// the portal for a number that has never called before takes seconds — one
// measured branch took 5.6s. The 180 therefore has to precede the portal and
// every query, not follow them.
func TestRingingPrecedesThePortalAndTheDatabase(t *testing.T) {
	h := newHarness(t)
	leg := newFakeInboundLeg()
	leg.rec = h.rec
	h.rec.arm()

	// The portal lookup fails, so the call goes no further than the work this
	// test is about the order of.
	h.mx.portalErr = errors.New("synapse is slow")
	h.HandleInboundCall(t.Context(), leg)

	order := h.rec.all()
	if len(order) == 0 || order[0] != "180" {
		t.Fatalf("the call did %v; want the 180 first", order)
	}
	if !slices.Contains(order, "portal") {
		t.Fatalf("the call did %v; want it to have reached the portal", order)
	}
}

// A call that fails after the ring has already gone out leaves every device in
// the room ringing until the notification lapses, and a row that blocks every
// later call from the same number. Both have to be undone.
func TestLiveKitFailingAfterTheRingRetractsItAndEndsTheCall(t *testing.T) {
	h := newHarness(t)
	h.lk.stageError("CreateSIPParticipant", http.StatusInternalServerError, `{"code":"internal","msg":"no trunk"}`)

	leg := newFakeInboundLeg()
	h.HandleInboundCall(t.Context(), leg)

	if _, rejects := leg.state(); len(rejects) != 1 || rejects[0] != 503 {
		t.Errorf("the leg was rejected with %v, want one 503", rejects)
	}
	// The notification was sent, so exactly it must be redacted.
	if h.intent.count(RtcNotificationEventType) != 1 {
		t.Fatalf("the call sent %d ring notifications, want one", h.intent.count(RtcNotificationEventType))
	}
	if got := h.intent.redactions(); len(got) != 1 {
		t.Errorf("the call redacted %v, want the ring notification; clients would ring on", got)
	}
	if call := h.activeCall(t); call != nil {
		t.Errorf("call %s is still %q; every later call to this number is refused", call.CallID, call.State)
	}
}

// A call shorter than the poll interval was never observed in the LiveKit room,
// so the watcher read its absence as "not joined yet" and never ended the row.
// That took the number out of service for six hours.
func TestACallShorterThanThePollIntervalStillEnds(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	if err := h.bridgeMedia(t.Context(), call); err != nil {
		t.Fatalf("bridgeMedia: %v", err)
	}
	if mine, err := h.db.Call.Transition(t.Context(), call, database.StateRinging, database.StateBridged); err != nil || !mine {
		t.Fatalf("Transition = %v, %v", mine, err)
	}

	// The very first tick, with the caller already gone.
	h.lk.stage("ListParticipants", `{"participants":[]}`)
	h.checkParticipants(t.Context())

	if got := h.activeCall(t); got != nil {
		t.Fatalf("call %s is still %q after the participant left", got.CallID, got.State)
	}
	if !slices.Contains(h.lk.calls(), "RemoveParticipant") {
		t.Errorf("LiveKit saw %v, want the participant removed", h.lk.calls())
	}
}

// A crash mid-call leaves livekit-sip in the room and the caller in the
// conference with nobody on the Matrix side. Retracting only the membership
// left that behind, and the conference blocks the next call to the number.
func TestStartupSweepRemovesTheLiveKitParticipantToo(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)

	h.sweepStaleMemberships(t.Context(), []*database.Call{call})

	if !slices.Contains(h.lk.calls(), "RemoveParticipant") {
		t.Errorf("LiveKit saw %v, want the participant of the last run's call removed", h.lk.calls())
	}
	// The Matrix half of the sweep must not have been lost either.
	var left bool
	for _, evt := range h.intent.events() {
		if evt.Type == CallMemberEventType && len(evt.Content.Raw) == 0 {
			left = true
		}
	}
	if !left {
		t.Error("the RTC membership from the last run was not retracted")
	}
}

// A failed publishGhostMembership used to be warned about and carried on from.
// The call then had no membership for anyone to join and no way to be ended,
// so the number stayed out of service until the membership expiry.
func TestDialFailsWhenTheGhostMembershipCannotBePublished(t *testing.T) {
	h := newHarness(t)
	h.intent.stateErr = errors.New("synapse said no")

	if _, err := h.Dial(t.Context(), h.mx.portal); err == nil {
		t.Fatal("Dial succeeded without a ghost membership")
	}
	if call := h.activeCall(t); call != nil {
		t.Fatalf("call %s is still %q, so it can never be ended", call.CallID, call.State)
	}
	// The proof that it is not merely recorded as ended: the next call to the
	// same number gets through.
	h.intent.stateErr = nil
	if _, err := h.Dial(t.Context(), h.mx.portal); err != nil {
		t.Fatalf("the next call to the same number failed: %v", err)
	}
	if len(h.sip.invites) != 1 {
		t.Errorf("the SIP transport saw %v, want one INVITE", h.sip.invites)
	}
}

// onMatrixJoinedCall has already established that the portal has no call in
// progress; Dial repeated the identical query. On a cluster where one query
// intermittently takes seconds, the round trip count is the latency.
func TestMatrixCallButtonLooksUpTheCallOnce(t *testing.T) {
	h := newHarness(t)

	if err := h.onMatrixJoinedCall(t.Context(), h.mx.portal, zerolog.Nop()); err != nil {
		t.Fatalf("onMatrixJoinedCall: %v", err)
	}
	if len(h.sip.invites) != 1 {
		t.Fatalf("the SIP transport saw %v, want one INVITE", h.sip.invites)
	}
	if got := h.queries.activeByPortal(); got != 1 {
		t.Errorf("the dial ran %d lookups by portal, want 1", got)
	}
}

// The same race as TestAnswerArrivingBeforeTheWaitIsNotLost, but through the
// whole of HandleInboundCall: the join lands while the ring notification is
// still going out. It is why the waiters are registered before the call is
// announced to Matrix at all.
func TestAnsweringTheInstantTheNotificationArrives(t *testing.T) {
	h := newHarness(t)
	// The join lands while HandleInboundCall is still inside setup.
	h.intent.on = func(evt sentEvent) {
		if evt.Type != RtcNotificationEventType {
			return
		}
		call, err := h.db.Call.GetActiveByPortal(context.Background(), h.mx.portal.ID)
		if err != nil || call == nil {
			t.Errorf("no ringing call when the notification went out: %v", err)
			return
		}
		h.signalAnswer(call.CallID)
	}

	leg := newFakeInboundLeg()
	h.HandleInboundCall(t.Context(), leg)

	answered, rejects := leg.state()
	if answered != 1 {
		t.Errorf("the leg was answered %d times, want once", answered)
	}
	if len(rejects) != 0 {
		t.Errorf("the leg was rejected with %v, want no rejection", rejects)
	}
}

// Returning from the ring without ending the call left the row ringing and the
// ghost's membership pinned in the room, which blocks every later call to the
// number until the row goes stale.
func TestBridgeShutdownDuringTheRingTearsTheCallDown(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	h.rememberNotification("$ring", call.CallID)

	ctx, cancel := context.WithCancel(t.Context())
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)
	leg := newFakeInboundLeg()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	h.waitForMatrix(ctx, call, leg, zerolog.Nop(), waiters)

	if _, rejects := leg.state(); len(rejects) != 1 || rejects[0] != 503 {
		t.Errorf("the leg was rejected with %v, want one 503", rejects)
	}
	if got := h.activeCall(t); got != nil {
		t.Fatalf("call %s is still %q after the bridge went away", got.CallID, got.State)
	}
	if got := h.intent.redactions(); !slices.Contains(got, "$ring") {
		t.Errorf("the call redacted %v, want the ring notification", got)
	}
}

// The watcher only ever inspected bridged rows, so a row left ringing — an
// outbound call whose INVITE never returned, or a setup cut short after the
// membership went out — was cleared only when someone next called that number.
// Until then the ghost sat in the room's call UI as a participant of a call
// that had no phone on the other end of it.
func TestTheWatcherClearsARowLeftRinging(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	h.rememberNotification("$ring", call.CallID)
	// Older than the ring timeout plus the setup grace, so nothing can still
	// be ringing for it.
	h.backdate(t, call, h.cfg.RingTimeout+2*time.Minute)

	h.checkParticipants(t.Context())

	if got := h.activeCall(t); got != nil {
		t.Fatalf("call %s is still %q; the watcher left it for the next caller to clear", got.CallID, got.State)
	}
	if got := h.intent.redactions(); !slices.Contains(got, "$ring") {
		t.Errorf("the call redacted %v, want the ring notification", got)
	}
}

// The conference header namespaces a conversation by line, so a portal ID is
// "<line>-<number>". Everything that dials, addresses or names the far end
// wants the number half: putting the whole ID in the outbound URI produced
// sip:+office-main-+15551234567@pbx, which the SIP server's route regex
// refuses, so no call out of a portal created after lines arrived ever
// connected.
func TestOutboundCallFromALinePortalDialsTheBareNumber(t *testing.T) {
	tests := []struct {
		name     string
		portalID string
		want     string
	}{
		{"line and e164", "office-main-+15551234567", "sip:+15551234567@pbx.example.com"},
		{"short number on a line", "office-1001", "sip:1001@pbx.example.com"},
		{"legacy id from before lines", "15551234567", "sip:+15551234567@pbx.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mx.portal = Portal{ID: tt.portalID, MXID: "!portal:example.com"}

			call, err := h.Dial(t.Context(), h.mx.portal)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			if len(h.sip.invites) != 1 || h.sip.invites[0] != tt.want {
				t.Fatalf("INVITE went to %v, want [%q]", h.sip.invites, tt.want)
			}
			// The conference, unlike the URI, keeps the whole portal ID: it is
			// what an inbound call to the same line has to match.
			if call.Conference != "sip-"+tt.portalID {
				t.Errorf("conference = %q, want the portal ID kept whole", call.Conference)
			}
			var participant map[string]any
			if err := json.Unmarshal(h.lk.bodies["CreateSIPParticipant"], &participant); err != nil {
				t.Fatalf("participant body: %v", err)
			}
			wantName := strings.TrimSuffix(strings.TrimPrefix(tt.want, "sip:"), "@pbx.example.com")
			if participant["participant_name"] != wantName {
				t.Errorf("LiveKit participant name = %v, want %q", participant["participant_name"], wantName)
			}
		})
	}
}
