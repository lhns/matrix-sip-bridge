package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
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

	if _, err := h.Dial(t.Context(), h.mx.portal, testCaller); err == nil {
		t.Fatal("Dial succeeded without a ghost membership")
	}
	if call := h.activeCall(t); call != nil {
		t.Fatalf("call %s is still %q, so it can never be ended", call.CallID, call.State)
	}
	// The proof that it is not merely recorded as ended: the next call to the
	// same number gets through.
	h.intent.stateErr = nil
	if _, err := h.Dial(t.Context(), h.mx.portal, testCaller); err != nil {
		t.Fatalf("the next call to the same number failed: %v", err)
	}
	if len(h.sip.invites) != 1 {
		t.Errorf("the SIP transport saw %v, want one INVITE", h.sip.invites)
	}
}

// The SIP server decides who may dial out, and it can only do that if the bridge
// says who is asking. Every dial entry point had the Matrix user in hand and
// dropped it on the floor, so every outbound call looked the same to the server.
func TestTheDiallingMatrixUserReachesTheInvite(t *testing.T) {
	h := newHarness(t)

	if _, err := h.Dial(t.Context(), h.mx.portal, testCaller); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if got := h.sip.callers; len(got) != 1 || got[0] != testCaller {
		t.Errorf("the INVITE was placed for %v, want [%s]", got, testCaller)
	}
}

// The call button is the other entry point, and the joining user is the caller:
// nothing else in the room asked for this call.
func TestTheJoiningMatrixUserReachesTheInvite(t *testing.T) {
	h := newHarness(t)

	if err := h.onMatrixJoinedCall(t.Context(), h.mx.portal, testCaller, zerolog.Nop()); err != nil {
		t.Fatalf("onMatrixJoinedCall: %v", err)
	}
	if got := h.sip.callers; len(got) != 1 || got[0] != testCaller {
		t.Errorf("the INVITE was placed for %v, want [%s]", got, testCaller)
	}
}

// onMatrixJoinedCall has already established that the portal has no call in
// progress; Dial repeated the identical query. On a cluster where one query
// intermittently takes seconds, the round trip count is the latency.
func TestMatrixCallButtonLooksUpTheCallOnce(t *testing.T) {
	h := newHarness(t)

	if err := h.onMatrixJoinedCall(t.Context(), h.mx.portal, testCaller, zerolog.Nop()); err != nil {
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

// A caller who gives up while Matrix is still ringing cancels the context the
// call runs on, and cancels it BEFORE the leg's Done closes. That made the
// ctx.Done case win every time and told the room the bridge had restarted --
// for an ordinary unanswered call, on a bridge with days of uptime. The leg is
// asked instead of the channel raced.
func TestACallerHangingUpDuringTheRingIsAMissedCallNotAFailure(t *testing.T) {
	h := newHarness(t)
	call := h.insertRinging(t)
	h.rememberNotification("$ring", call.CallID)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)
	leg := newFakeInboundLeg()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// The real teardown order: the leg commits to releasing, cancels the
		// call's context, and only then closes Done. Reproducing it here is
		// the whole point -- with Done already closed the select could pass
		// by luck.
		leg.beginFinish()
		cancel()
	}()
	h.waitForMatrix(ctx, call, leg, zerolog.Nop(), waiters)

	if got := h.intent.bodies(); len(got) != 1 || got[0] != "Missed call" {
		t.Errorf("the room was told %v, want one \"Missed call\"", got)
	}
	if _, rejects := leg.state(); len(rejects) != 0 {
		t.Errorf("the leg was rejected with %v, want no rejection -- the caller is already gone", rejects)
	}
	if got := h.activeCall(t); got != nil {
		t.Fatalf("call %s is still %q after the caller hung up", got.CallID, got.State)
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

			call, err := h.Dial(t.Context(), h.mx.portal, testCaller)
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

// A user who types !dial is not in the RTC session, so the membership alone
// tells them nothing: they were left to find the portal room and join by hand.
// Only the call button's caller is already in the session.
func TestOnlyTheCallButtonSkipsTheRingNotification(t *testing.T) {
	tests := []struct {
		name string
		dial func(t *testing.T, h *harness)
		want int
	}{
		{"the dial command rings", func(t *testing.T, h *harness) {
			if _, err := h.Dial(t.Context(), h.mx.portal, testCaller); err != nil {
				t.Fatalf("Dial: %v", err)
			}
		}, 1},
		{"the call button does not", func(t *testing.T, h *harness) {
			if err := h.onMatrixJoinedCall(t.Context(), h.mx.portal, testCaller, zerolog.Nop()); err != nil {
				t.Fatalf("onMatrixJoinedCall: %v", err)
			}
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.dial(t, h)
			if got := h.intent.count(RtcNotificationEventType); got != tt.want {
				t.Errorf("the call sent %d ring notifications, want %d", got, tt.want)
			}
		})
	}
}

// The notification a commanded call sends has to be retracted like any other,
// or the user's phone keeps showing an incoming call after the call is over.
func TestEndingACommandedCallRetractsItsRingNotification(t *testing.T) {
	h := newHarness(t)
	call, err := h.Dial(t.Context(), h.mx.portal, testCaller)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var notify id.EventID
	for i, evt := range h.intent.events() {
		if evt.Type == RtcNotificationEventType {
			notify = id.EventID(fmt.Sprintf("$event%d", i+1))
		}
	}
	if notify == "" {
		t.Fatal("the commanded call sent no ring notification")
	}
	if err := h.endCall(t.Context(), call); err != nil {
		t.Fatalf("endCall: %v", err)
	}
	if got := h.intent.redactions(); !slices.Contains(got, notify) {
		t.Errorf("redacted %v, want the ring notification %s among them", got, notify)
	}
}

// A number typed with a line has to key the same portal an inbound call on
// that line keys, or the command opens a third room for the same person.
func TestDialNumberKeysThePortalOnTheLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"no line keeps the legacy key", "", "15551234567"},
		{"a line scopes the key", "home", "home-+15551234567"},
		{"a hyphenated line", "office-main", "office-main-+15551234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.DialNumber(t.Context(), "+15551234567", tt.line, testCaller); err != nil {
				t.Fatalf("DialNumber: %v", err)
			}
			if got := h.mx.portalsAsked(); len(got) != 1 || got[0] != tt.want {
				t.Errorf("the portal asked for was %v, want [%q]", got, tt.want)
			}
		})
	}
}

// Falling back to the line-less portal would put the call in a different room
// from the one the user asked for, with nothing saying so.
func TestDialNumberRefusesAnUnusableLine(t *testing.T) {
	h := newHarness(t)
	_, err := h.DialNumber(t.Context(), "+15551234567", "not a line", testCaller)
	if !errors.Is(err, phonenum.ErrBadLine) {
		t.Fatalf("DialNumber with an unusable line failed with %v, want ErrBadLine", err)
	}
	if got := h.mx.portalsAsked(); len(got) != 0 {
		t.Errorf("a portal was looked up anyway: %v", got)
	}
	if len(h.sip.invites) != 0 {
		t.Errorf("an INVITE went out anyway: %v", h.sip.invites)
	}
}

// The bridge had to publish the membership before CreateSIPParticipant -- it
// is what the Matrix side joins -- so a client that read it then resolved a
// LiveKit identity that was not in the room yet, and nothing later told it to
// look again. Audio went to the phone and none came back.
func TestAnOutboundCallRepublishesTheMembershipOnceTheParticipantExists(t *testing.T) {
	h := newHarness(t)
	// What LiveKit had been asked for at the moment each membership went out.
	var lkAt [][]string
	h.intent.on = func(evt sentEvent) {
		if evt.Type == CallMemberEventType && len(evt.Content.Raw) > 0 {
			lkAt = append(lkAt, h.lk.calls())
		}
	}

	if _, err := h.Dial(t.Context(), h.mx.portal, testCaller); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	memberships := activeMemberships(h.intent.events())
	if len(memberships) != 2 {
		t.Fatalf("the call published %d memberships, want two", len(memberships))
	}
	// One state key, so the second replaces the first rather than adding a
	// participant that the retraction would then leave behind.
	if memberships[0].StateKey != memberships[1].StateKey {
		t.Errorf("state keys %q and %q differ; that is two memberships, not one replaced",
			memberships[0].StateKey, memberships[1].StateKey)
	}
	if len(lkAt) != 2 || slices.Contains(lkAt[0], "CreateSIPParticipant") {
		t.Errorf("the first membership went out after LiveKit saw %v, want it before the participant", lkAt)
	}
	if len(lkAt) != 2 || !slices.Contains(lkAt[1], "CreateSIPParticipant") {
		t.Errorf("the second membership went out after LiveKit saw %v, want the participant already created", lkAt)
	}
}

// Synapse drops a state write whose content equals the current state, so a
// re-publish that differed in nothing would never reach a client and the fix
// above would be inert. created_ts is what makes the two events differ, and it
// has to be the only thing that does: the LiveKit identity is derived from the
// rest, and changing any of it would point the client at a participant that
// does not exist.
func TestOnlyCreatedTSDiffersBetweenTwoPublishesOfOneMembership(t *testing.T) {
	first := ghostMembership(time.UnixMilli(1_700_000_000_000), "@sip_15551234567:example.com",
		"!portal:example.com", "SIPABCD", "m1", "https://jwt.example.com", 6*time.Hour)
	second := ghostMembership(time.UnixMilli(1_700_000_004_000), "@sip_15551234567:example.com",
		"!portal:example.com", "SIPABCD", "m1", "https://jwt.example.com", 6*time.Hour)

	if first.Raw["created_ts"] == second.Raw["created_ts"] {
		t.Fatal("created_ts did not move, so the second publish is deduplicated away")
	}
	delete(first.Raw, "created_ts")
	delete(second.Raw, "created_ts")
	a, _ := json.Marshal(first.Raw)
	b, _ := json.Marshal(second.Raw)
	if string(a) != string(b) {
		t.Errorf("the two memberships differ in more than created_ts:\n%s\n%s", a, b)
	}
}

// Inbound already has its participant live before any client reads the
// membership, so it needs no second publish -- and an extra one there would be
// a state change in a room mid-call for no reason.
func TestAnInboundCallPublishesOneMembership(t *testing.T) {
	h := newHarness(t)
	leg := newFakeInboundLeg()
	call, err := h.beginInboundCall(t.Context(), newCallID(), h.mx.portal.ID, h.conferenceFor(h.mx.portal.ID))
	if err != nil {
		t.Fatalf("beginInboundCall: %v", err)
	}
	// The waiters have to exist before the answer is signalled; signalling a
	// call that has none is a no-op and the ring would run to its timeout.
	waiters := h.waitersFor(call.CallID)
	defer h.forgetWaiters(call.CallID)
	h.signalAnswer(call.CallID)
	h.waitForMatrix(t.Context(), call, leg, zerolog.Nop(), waiters)

	if got := activeMemberships(h.intent.events()); len(got) != 1 {
		t.Errorf("the inbound call published %d memberships, want one", len(got))
	}
}

// Whatever a call published, ending it must leave exactly one leave event and
// no membership standing: the production rooms are already full of stale ones.
func TestEndingAnOutboundCallLeavesNoMembershipBehind(t *testing.T) {
	h := newHarness(t)
	call, err := h.Dial(t.Context(), h.mx.portal, testCaller)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := h.endCall(t.Context(), call); err != nil {
		t.Fatalf("endCall: %v", err)
	}
	var leaves []sentEvent
	for _, evt := range h.intent.events() {
		if evt.Type == CallMemberEventType && len(evt.Content.Raw) == 0 {
			leaves = append(leaves, evt)
		}
	}
	if len(leaves) != 1 {
		t.Fatalf("ending the call sent %d leave events, want one", len(leaves))
	}
	if got := activeMemberships(h.intent.events()); leaves[0].StateKey != got[0].StateKey {
		t.Errorf("the leave cleared %q, want the membership's own %q", leaves[0].StateKey, got[0].StateKey)
	}
}

// activeMemberships returns the call.member events that are memberships rather
// than the empty content that ends one.
func activeMemberships(events []sentEvent) []sentEvent {
	var out []sentEvent
	for _, evt := range events {
		if evt.Type == CallMemberEventType && len(evt.Content.Raw) > 0 {
			out = append(out, evt)
		}
	}
	return out
}

// !dial sends the user a ring notification for a call they placed. Its Decline
// button reached nothing: only the inbound path has a goroutine waiting on the
// decline channel, so the phone went on ringing.
func TestDecliningACommandedCallEndsIt(t *testing.T) {
	h := newHarness(t)
	call := h.insertRingingOutbound(t)
	h.rememberNotification("$ring", call.CallID)

	h.handleRtcDecline(t.Context(), declineEvent("$ring"))

	if got := h.activeCall(t); got != nil {
		t.Fatalf("call %s is still %q after it was declined", got.CallID, got.State)
	}
	if got := h.intent.redactions(); !slices.Contains(got, "$ring") {
		t.Errorf("the call redacted %v, want the ring notification it declined", got)
	}
}

// A decline naming a notification from another bridge, or from a previous run,
// must not tear down whatever is ringing now.
func TestAnUnknownDeclineEndsNothing(t *testing.T) {
	h := newHarness(t)
	call := h.insertRingingOutbound(t)
	h.rememberNotification("$ring", call.CallID)

	h.handleRtcDecline(t.Context(), declineEvent("$someone-elses-ring"))

	if got := h.activeCall(t); got == nil {
		t.Fatal("the call was ended by a decline that named a different notification")
	}
}

// An answered call is not declined by a late decline from a second device:
// recording it as declined would be a lie about a call that happened.
func TestADeclineAfterTheCallIsUpIsIgnored(t *testing.T) {
	h := newHarness(t)
	call, err := h.Dial(t.Context(), h.mx.portal, testCaller)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	notify := id.EventID("$ring")
	h.rememberNotification(notify, call.CallID)

	h.handleRtcDecline(t.Context(), declineEvent(notify))

	if got := h.activeCall(t); got == nil {
		t.Fatal("a decline ended a call that was already up")
	}
}

// declineEvent is the MSC4310 event a client sends to reject the call a ring
// notification announced.
func declineEvent(notify id.EventID) *event.Event {
	raw := []byte(`{"m.relates_to":{"rel_type":"m.reference","event_id":"` + notify.String() + `"}}`)
	return &event.Event{
		Sender:  "@alice:example.com",
		RoomID:  "!portal:example.com",
		Type:    RtcDeclineEventType,
		Content: event.Content{VeryRaw: raw},
	}
}
