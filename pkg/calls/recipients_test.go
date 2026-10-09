package calls

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// inboundCall is one inbound leg held by HandleInboundCall in the background.
type inboundCall struct {
	leg  *fakeInboundLeg
	room id.RoomID
	call *database.Call
	done chan struct{}
}

// waitFor polls a condition the code under test reaches on its own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// notificationOf returns the ring notification a call sent.
func (h *harness) notificationOf(callID string) id.EventID {
	h.declinedMu.Lock()
	defer h.declinedMu.Unlock()
	for notify, owner := range h.notifies {
		if owner == callID {
			return notify
		}
	}
	return ""
}

// pendingJoins is how many joins a call's leg has not yet read.
func (h *harness) pendingJoins(callID string) int {
	h.answeredMu.Lock()
	defer h.answeredMu.Unlock()
	return len(h.answered[callID])
}

// start runs a leg through HandleInboundCall and returns once the room it rings
// has been told, i.e. once a join can be heard.
func (h *harness) start(t *testing.T, leg *fakeInboundLeg, room id.RoomID) *inboundCall {
	t.Helper()
	leg.rec = h.rec
	ctx, cancel := context.WithCancel(context.Background())
	ic := &inboundCall{leg: leg, room: room, done: make(chan struct{})}
	go func() {
		defer close(ic.done)
		h.HandleInboundCall(ctx, leg)
	}()
	// A leg is released before its context dies, as the transport does it.
	t.Cleanup(func() {
		leg.finish()
		cancel()
		<-ic.done
	})
	waitFor(t, "the room to ring", func() bool {
		call := h.activeCallIn(t, room)
		if call == nil || h.notificationOf(call.CallID) == "" {
			return false
		}
		ic.call = call
		return true
	})
	return ic
}

// ring starts a leg for a recipient header, whose room is owner's.
func (h *harness) ring(t *testing.T, recipient string, owner id.UserID, name string) *inboundCall {
	t.Helper()
	leg := newFakeInboundLeg()
	leg.recipient = recipient
	return h.start(t, leg, h.mx.roomOf(owner, name))
}

// ringBoth rings the two users, as the SIP server does with one leg each.
func (h *harness) ringBoth(t *testing.T) (alice, bob *inboundCall) {
	t.Helper()
	alice = h.ring(t, testCaller.String(), testCaller, "Alice")
	bob = h.ring(t, testBob.String(), testBob, "Bob")
	return alice, bob
}

func callMemberEvent(room id.RoomID, sender id.UserID, active bool) *event.Event {
	raw := []byte(`{}`)
	if active {
		raw = []byte(`{"application":"m.call","device_id":"DEVICE"}`)
	}
	key := sender.String()
	return &event.Event{
		Sender:   sender,
		RoomID:   room,
		Type:     CallMemberEventType,
		StateKey: &key,
		Content:  event.Content{VeryRaw: raw},
	}
}

func (h *harness) join(room id.RoomID, user id.UserID) {
	h.handleCallMember(context.Background(), callMemberEvent(room, user, true))
}

func (h *harness) leave(room id.RoomID, user id.UserID) {
	h.handleCallMember(context.Background(), callMemberEvent(room, user, false))
}

func (ic *inboundCall) answered() int {
	n, _ := ic.leg.state()
	return n
}

func (ic *inboundCall) waitAnswered(t *testing.T) {
	t.Helper()
	waitFor(t, "the leg to be answered", func() bool { return ic.answered() > 0 })
}

// cancel is the SIP server cancelling the leg, and waits for the bridge to be
// done with it.
func (ic *inboundCall) cancel() {
	ic.leg.finish()
	<-ic.done
}

func participantsCreated(h *harness) int {
	n := 0
	for _, method := range h.lk.calls() {
		if method == "CreateSIPParticipant" {
			n++
		}
	}
	return n
}

func rowCount(t *testing.T, h *harness) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(context.Background(), "SELECT count(*) FROM sip_call").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// Asterisk sends one leg per recipient with the same conference. Each has to
// ring its own recipient's room and nobody else's.
func TestTwoRecipientsRingSeparately(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.ringBoth(t)

	if alice.room == bob.room {
		t.Fatalf("both recipients share room %s", alice.room)
	}
	if alice.call.CallID == bob.call.CallID {
		t.Fatal("both legs share one call row")
	}
	if alice.call.Conference != bob.call.Conference {
		t.Errorf("conferences %q and %q differ, want one shared", alice.call.Conference, bob.call.Conference)
	}
	if alice.call.Receiver != testCaller || bob.call.Receiver != testBob {
		t.Errorf("receivers = %q and %q, want %q and %q", alice.call.Receiver, bob.call.Receiver, testCaller, testBob)
	}
	for _, room := range []id.RoomID{alice.room, bob.room} {
		if got := h.intent.countIn(room, RtcNotificationEventType); got != 1 {
			t.Errorf("room %s got %d ring notifications, want one", room, got)
		}
	}
	if got := participantsCreated(h); got != 0 {
		t.Errorf("%d SIP participants exist before anyone answered", got)
	}
}

// Several legs would put several livekit-sip participants into one conference,
// so the first join claims it and the others only wait to be cancelled.
func TestFirstAnswerWins(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := newHarness(t)
	h.metrics = metrics.New(reg)
	alice, bob := h.ringBoth(t)

	h.join(alice.room, testCaller)
	alice.waitAnswered(t)
	if got := participantsCreated(h); got != 1 {
		t.Fatalf("%d SIP participants were created, want exactly one", got)
	}
	if got := h.activeCallIn(t, alice.room); got == nil || got.State != database.StateBridged || got.MatrixUser != testCaller {
		t.Fatalf("the winner's row = %+v, want bridged for %s", got, testCaller)
	}

	// The SIP server now cancels the other leg.
	bob.cancel()

	if got := h.intent.bodiesIn(bob.room); !slices.Equal(got, []string{"Answered by Alice"}) {
		t.Errorf("the losing room was told %v, want one \"Answered by Alice\"", got)
	}
	if got := h.intent.bodiesIn(alice.room); len(got) != 0 {
		t.Errorf("the winning room was told %v, want nothing while the call is up", got)
	}
	if got := callCount(t, reg, metrics.DirectionInbound, metrics.OutcomeAnsweredElsewhere); got != 1 {
		t.Errorf("calls_total{inbound,answered_elsewhere} = %v, want 1", got)
	}
}

// A second user joining after the claim must not answer or add media.
func TestALateJoinDoesNotAnswerASecondTime(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.ringBoth(t)

	h.join(alice.room, testCaller)
	alice.waitAnswered(t)
	h.join(bob.room, testBob)
	waitFor(t, "the late join to be read", func() bool { return h.pendingJoins(bob.call.CallID) == 0 })
	bob.cancel()

	answered, rejects := bob.leg.state()
	if answered != 0 || len(rejects) != 0 {
		t.Errorf("the losing leg was answered %d times and rejected with %v, want neither", answered, rejects)
	}
	if alice.answered() != 1 {
		t.Errorf("the winner was answered %d times, want once", alice.answered())
	}
	if got := participantsCreated(h); got != 1 {
		t.Errorf("%d SIP participants were created, want exactly one", got)
	}
	if got := h.intent.bodiesIn(bob.room); !slices.Equal(got, []string{"Answered by Alice"}) {
		t.Errorf("the losing room was told %v, want one \"Answered by Alice\"", got)
	}
}

// A decline answers only the leg of the room that declined.
func TestADeclineEndsOnlyItsOwnLeg(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.ringBoth(t)

	h.handleRtcDecline(context.Background(), declineEvent(h.notificationOf(alice.call.CallID)))
	<-alice.done

	if _, rejects := alice.leg.state(); !slices.Equal(rejects, []int{486}) {
		t.Errorf("the declining leg was rejected with %v, want one 486", rejects)
	}
	if got := h.intent.bodiesIn(alice.room); !slices.Equal(got, []string{"Call declined"}) {
		t.Errorf("the declining room was told %v, want \"Call declined\"", got)
	}
	if got := h.activeCallIn(t, bob.room); got == nil || got.State != database.StateRinging {
		t.Fatalf("the other leg = %+v, want it still ringing", got)
	}
	if _, rejects := bob.leg.state(); len(rejects) != 0 {
		t.Errorf("the other leg was rejected with %v", rejects)
	}

	h.join(bob.room, testBob)
	bob.waitAnswered(t)
}

func TestNobodyAnsweringIsAMissedCallInEachRoom(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.ringBoth(t)

	alice.cancel()
	bob.cancel()

	for _, ic := range []*inboundCall{alice, bob} {
		if got := h.intent.bodiesIn(ic.room); !slices.Equal(got, []string{"Missed call"}) {
			t.Errorf("room %s was told %v, want one \"Missed call\"", ic.room, got)
		}
	}
}

// A leg that names nobody keeps working as it did when there was one login.
func TestAMissingRecipientRingsTheDefaultOwner(t *testing.T) {
	h := newHarness(t)
	ic := h.ring(t, "", testCaller, "Alice")

	if ic.room != h.mx.portal.MXID || ic.call.Receiver != testCaller {
		t.Errorf("rang %s for %q, want the default owner's room %s", ic.room, ic.call.Receiver, h.mx.portal.MXID)
	}
}

// The refusal is the SIP answer, and it must leave nothing behind: no room
// for a user the bridge does not serve and no row to block the number.
func TestARefusedRecipientIsRejectedBeforeAnyPortal(t *testing.T) {
	tests := []struct {
		name   string
		refuse bool
		want   int
	}{
		{"refused", true, 403},
		{"unknown", false, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			const header = "@mallory:example.com"
			if tt.refuse {
				h.mx.refused[header] = true
			} else {
				h.mx.unknown[header] = true
			}
			leg := newFakeInboundLeg()
			leg.recipient = header

			h.HandleInboundCall(t.Context(), leg)

			if _, rejects := leg.state(); !slices.Equal(rejects, []int{tt.want}) {
				t.Errorf("the leg was rejected with %v, want one %d", rejects, tt.want)
			}
			if got := h.mx.portalsAsked(); len(got) != 0 {
				t.Errorf("a portal was looked up anyway: %v", got)
			}
			if got := rowCount(t, h); got != 0 {
				t.Errorf("%d call rows were written", got)
			}
		})
	}
}

// Media before the answer would put one livekit-sip participant per leg into
// the shared conference. The ghost's membership has to be published again once
// the participant exists, and the caller is only connected after that.
func TestMediaComesAfterTheJoinAndBeforeThe200(t *testing.T) {
	h := newHarness(t)
	var lkAtMembership [][]string
	h.intent.on = func(evt sentEvent) {
		if evt.Type == CallMemberEventType && len(evt.Content.Raw) > 0 {
			lkAtMembership = append(lkAtMembership, h.lk.calls())
		}
	}
	var lkAtAnswer []string
	leg := newFakeInboundLeg()
	leg.onAnswer = func() { lkAtAnswer = h.lk.calls() }
	ic := h.start(t, leg, h.mx.portal.MXID)

	if got := participantsCreated(h); got != 0 {
		t.Fatalf("%d SIP participants exist while the call only rings", got)
	}
	h.join(ic.room, testCaller)
	ic.waitAnswered(t)
	<-ic.done

	if len(lkAtMembership) != 2 {
		t.Fatalf("the call published %d memberships, want two", len(lkAtMembership))
	}
	if slices.Contains(lkAtMembership[0], "CreateSIPParticipant") {
		t.Errorf("the first membership went out after LiveKit saw %v, want it before the participant", lkAtMembership[0])
	}
	if !slices.Contains(lkAtMembership[1], "CreateSIPParticipant") {
		t.Errorf("the second membership went out after LiveKit saw %v, want the participant already created", lkAtMembership[1])
	}
	if !slices.Contains(lkAtAnswer, "CreateSIPParticipant") {
		t.Errorf("the 200 went out after LiveKit saw %v, want the participant already created", lkAtAnswer)
	}
	if got := activeMemberships(h.intent.events()); len(got) != 2 || got[0].StateKey != got[1].StateKey {
		t.Errorf("the memberships = %v, want one state key published twice", got)
	}
}

// A caller must not be connected to a conference nobody can hear, and the
// ringing must stop.
func TestMediaFailingAfterTheJoinRejectsTheLegAndEndsTheCall(t *testing.T) {
	h := newHarness(t)
	h.lk.stageError("CreateSIPParticipant", 500, `{"code":"internal","msg":"no trunk"}`)
	ic := h.ring(t, "", testCaller, "Alice")
	notify := h.notificationOf(ic.call.CallID)

	h.join(ic.room, testCaller)
	<-ic.done

	answered, rejects := ic.leg.state()
	if answered != 0 || !slices.Equal(rejects, []int{503}) {
		t.Errorf("the leg was answered %d times and rejected with %v, want one 503 and no 200", answered, rejects)
	}
	if got := h.intent.bodiesIn(ic.room); !slices.Equal(got, []string{"Call failed — could not connect"}) {
		t.Errorf("the room was told %v, want the failure", got)
	}
	if got := h.intent.redactions(); !slices.Contains(got, notify) {
		t.Errorf("the call redacted %v, want the ring notification; clients would ring on", got)
	}
	if call := h.activeCallIn(t, ic.room); call != nil {
		t.Errorf("call %s is still %q; every later call to this number is refused", call.CallID, call.State)
	}
}

// Someone who joined the room to look and left again must not hang up the
// call the answerer is on.
func TestOnlyTheAnswerersLeaveEndsTheCall(t *testing.T) {
	h := newHarness(t)
	ic := h.ring(t, "", testCaller, "Alice")
	h.join(ic.room, testCaller)
	ic.waitAnswered(t)

	h.leave(ic.room, testBob)
	if h.activeCallIn(t, ic.room) == nil {
		t.Fatal("another member leaving ended the call")
	}
	h.leave(ic.room, testCaller)
	if call := h.activeCallIn(t, ic.room); call != nil {
		t.Errorf("call %s is still %q after the answerer left", call.CallID, call.State)
	}
}

// !dial and DialNumber look at the caller's own rooms: another user's rooms
// are their conversations, and a call in them is no reason to refuse this one.
func TestDialNumberResolvesOnlyTheCallersPortals(t *testing.T) {
	h := newHarness(t)
	h.mx.portalIDs = []string{"home-+15551234567"}
	h.mx.otherPortalIDs = map[id.UserID][]string{testBob: {"work-+15551234567"}}

	bobCall, err := h.DialNumber(t.Context(), "+15551234567", "", testBob)
	if err != nil {
		t.Fatalf("DialNumber as bob: %v", err)
	}
	aliceCall, err := h.DialNumber(t.Context(), "+15551234567", "", testCaller)
	if err != nil {
		t.Fatalf("DialNumber as alice: %v", err)
	}

	if got, want := h.mx.portalsAsked(), []string{"work-+15551234567", "home-+15551234567"}; !slices.Equal(got, want) {
		t.Errorf("the portals asked for were %v, want %v", got, want)
	}
	if got, want := h.mx.ownersAsked(), []id.UserID{testBob, testCaller}; !slices.Equal(got, want) {
		t.Errorf("the owners asked for were %v, want %v", got, want)
	}
	if bobCall.RoomID == aliceCall.RoomID {
		t.Errorf("both users were put in room %s", bobCall.RoomID)
	}
	if bobCall.Receiver != testBob || bobCall.MatrixUser != testBob {
		t.Errorf("bob's call = receiver %q, user %q, want %q for both", bobCall.Receiver, bobCall.MatrixUser, testBob)
	}
	if aliceCall.Receiver != testCaller || aliceCall.MatrixUser != testCaller {
		t.Errorf("alice's call = receiver %q, user %q, want %q for both", aliceCall.Receiver, aliceCall.MatrixUser, testCaller)
	}
}

// A call that answered a previous call from the same number is not the winner
// of this one.
func TestAnAnswerOutsideTheRingWindowIsNotTheWinner(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.ringBoth(t)
	h.join(alice.room, testCaller)
	alice.waitAnswered(t)
	h.backdate(t, alice.call, time.Hour)

	bob.cancel()

	if got := h.intent.bodiesIn(bob.room); !slices.Equal(got, []string{"Missed call"}) {
		t.Errorf("the room was told %v, want \"Missed call\"", got)
	}
}
