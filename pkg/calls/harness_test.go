package calls

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// recorder is the order in which a call touched the world. It exists for the
// one assertion that is about ordering rather than effect: the 180 has to
// precede the portal and the database, or the caller hears silence for as long
// as room creation takes.
type recorder struct {
	mu      sync.Mutex
	on      bool
	entries []string
}

func (r *recorder) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = true
}

func (r *recorder) add(what string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on {
		r.entries = append(r.entries, what)
	}
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.entries...)
}

// sentEvent is one thing the ghost put in the room.
type sentEvent struct {
	Type     event.Type
	StateKey string
	State    bool
	Content  *event.Content
}

// fakeIntent is a ghost's half of the Matrix API. It is the seam the ring
// notification, its retraction and the RTC membership all go through, so the
// events it collected are what most of these tests assert on.
type fakeIntent struct {
	rec  *recorder
	mxid id.UserID

	mu       sync.Mutex
	sent     []sentEvent
	n        int
	stateErr error
	// on runs after each event, for a test that needs something to happen at
	// the moment a particular event goes out.
	on func(sentEvent)
}

func newFakeIntent(rec *recorder) *fakeIntent {
	return &fakeIntent{rec: rec, mxid: "@sip_15551234567:example.com"}
}

func (f *fakeIntent) GetMXID() id.UserID { return f.mxid }

func (f *fakeIntent) record(evt sentEvent) (*mautrix.RespSendEvent, error) {
	f.mu.Lock()
	if evt.State && f.stateErr != nil {
		err := f.stateErr
		f.mu.Unlock()
		return nil, err
	}
	f.n++
	evtID := id.EventID(fmt.Sprintf("$event%d", f.n))
	f.sent = append(f.sent, evt)
	on := f.on
	f.mu.Unlock()
	f.rec.add("matrix:" + evt.Type.Type)
	if on != nil {
		on(evt)
	}
	return &mautrix.RespSendEvent{EventID: evtID}, nil
}

func (f *fakeIntent) SendState(_ context.Context, _ id.RoomID, eventType event.Type, stateKey string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	return f.record(sentEvent{Type: eventType, StateKey: stateKey, State: true, Content: content})
}

func (f *fakeIntent) SendMessage(_ context.Context, _ id.RoomID, eventType event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	return f.record(sentEvent{Type: eventType, Content: content})
}

func (f *fakeIntent) events() []sentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentEvent(nil), f.sent...)
}

// count reports how many events of a type the ghost sent.
func (f *fakeIntent) count(eventType event.Type) int {
	n := 0
	for _, evt := range f.events() {
		if evt.Type == eventType {
			n++
		}
	}
	return n
}

// redactions returns the events the ghost redacted, which is how a ring
// notification is retracted.
func (f *fakeIntent) redactions() []id.EventID {
	var out []id.EventID
	for _, evt := range f.events() {
		if evt.Type != event.EventRedaction {
			continue
		}
		if content, ok := evt.Content.Parsed.(*event.RedactionEventContent); ok {
			out = append(out, content.Redacts)
		}
	}
	return out
}

// fakeMatrix is the bridgev2 side of the world.
type fakeMatrix struct {
	rec    *recorder
	intent *fakeIntent

	portal    Portal
	portalErr error
	members   map[id.UserID]*event.MemberEventContent

	// asked records the portal IDs PortalRoom was called with. It is the only
	// place a dial entry point's ID arithmetic is visible, the portal itself
	// being fixed.
	mu    sync.Mutex
	asked []string
}

func newFakeMatrix(rec *recorder) *fakeMatrix {
	return &fakeMatrix{
		rec:    rec,
		intent: newFakeIntent(rec),
		portal: Portal{ID: "15551234567", MXID: "!portal:example.com"},
		members: map[id.UserID]*event.MemberEventContent{
			"@alice:example.com": {Membership: event.MembershipJoin},
		},
	}
}

func (f *fakeMatrix) GhostIntent(context.Context, string) (GhostIntent, error) {
	return f.intent, nil
}

func (f *fakeMatrix) PortalRoom(_ context.Context, portalID string) (Portal, error) {
	f.rec.add("portal")
	f.mu.Lock()
	f.asked = append(f.asked, portalID)
	f.mu.Unlock()
	if f.portalErr != nil {
		return Portal{}, f.portalErr
	}
	return f.portal, nil
}

// portalsAsked returns the portal IDs PortalRoom was called with.
func (f *fakeMatrix) portalsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (f *fakeMatrix) PortalByMXID(_ context.Context, roomID id.RoomID) (Portal, bool) {
	if roomID != f.portal.MXID {
		return Portal{}, false
	}
	return f.portal, true
}

func (f *fakeMatrix) BotMXID() id.UserID { return "@sipbot:example.com" }

func (f *fakeMatrix) IsGhost(userID id.UserID) bool {
	return strings.HasPrefix(userID.String(), "@sip_")
}

func (f *fakeMatrix) Members(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	return f.members, nil
}

// fakeOutboundLeg is the control leg of a call the bridge placed.
type fakeOutboundLeg struct {
	mu      sync.Mutex
	hangups int
	done    chan struct{}
}

func newFakeOutboundLeg() *fakeOutboundLeg {
	return &fakeOutboundLeg{done: make(chan struct{})}
}

func (l *fakeOutboundLeg) Done() <-chan struct{} { return l.done }

func (l *fakeOutboundLeg) Hangup(context.Context) error {
	l.mu.Lock()
	l.hangups++
	l.mu.Unlock()
	return nil
}

// testCaller is the Matrix user the harness dials as. Real, because "" is the
// no-user case and has its own meaning on the wire.
const testCaller = id.UserID("@alice:example.com")

// fakeTelephony is the SIP transport. Invite answers immediately, as a callee
// picking up does.
type fakeTelephony struct {
	mu      sync.Mutex
	invites []string
	// callers records the Matrix user each invite was placed for, so a test can
	// assert the identity survived the whole dial chain rather than only that a
	// call went out.
	callers []id.UserID
	leg     *fakeOutboundLeg
	err     error
}

func (f *fakeTelephony) Invite(ctx context.Context, to, _ string, caller id.UserID) (OutboundLeg, error) {
	f.mu.Lock()
	f.invites = append(f.invites, to)
	f.callers = append(f.callers, caller)
	err, leg := f.err, f.leg
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if leg == nil {
		leg = newFakeOutboundLeg()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return leg, nil
}

// queryLog counts the queries the subsystem ran, which is the only way to see
// a duplicate lookup on a path whose result is otherwise identical.
type queryLog struct {
	dbutil.DatabaseLogger

	mu       sync.Mutex
	rec      *recorder
	byPortal int
}

func (q *queryLog) QueryTiming(_ context.Context, _, query string, _ []any, _ int, _ time.Duration, _ error) {
	q.mu.Lock()
	rec := q.rec
	if strings.Contains(query, "WHERE portal_id = ") {
		q.byPortal++
	}
	q.mu.Unlock()
	rec.add("db")
}

func (q *queryLog) activeByPortal() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.byPortal
}

// harness is a subsystem wired to fakes on every side: bridgev2, LiveKit, the
// SIP transport and a real sqlite database.
type harness struct {
	*Subsystem
	rec     *recorder
	mx      *fakeMatrix
	intent  *fakeIntent
	lk      *fakeLiveKit
	sip     *fakeTelephony
	queries *queryLog
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := &recorder{}
	mx := newFakeMatrix(rec)
	lk := newFakeLiveKit(t)
	sip := &fakeTelephony{}

	queries := &queryLog{DatabaseLogger: dbutil.NoopLogger, rec: rec}
	db := testDatabaseLogged(t, queries)

	s := &Subsystem{
		cfg: Config{
			Enabled:                 true,
			ConferencePrefix:        "sip-",
			OutboundURI:             "sip:{number}@pbx.example.com",
			RingTimeout:             5 * time.Second,
			MembershipExpiry:        6 * time.Hour,
			ParticipantPollInterval: time.Hour,
			IdentityScheme:          IdentityUserDevice,
		},
		mx:       mx,
		sip:      sip,
		lk:       lk.client(),
		db:       db,
		log:      zerolog.Nop(),
		answered: map[string]chan struct{}{},
		declined: map[string]chan struct{}{},
		ended:    map[string]chan struct{}{},
		notifies: map[id.EventID]string{},
		legs:     map[string]OutboundLeg{},
		seen:     map[string]bool{},
	}
	trunk := "ST_test"
	s.trunkID.Store(&trunk)

	return &harness{Subsystem: s, rec: rec, mx: mx, intent: mx.intent, lk: lk, sip: sip, queries: queries}
}

// activeCall returns the call still in progress for the harness's portal, or
// nil. "Ended" is the assertion most of these tests make.
func (h *harness) activeCall(t *testing.T) *database.Call {
	t.Helper()
	call, err := h.db.Call.GetActiveByPortal(context.Background(), h.mx.portal.ID)
	if err != nil {
		t.Fatalf("GetActiveByPortal: %v", err)
	}
	return call
}

// insertRinging writes the row an inbound call has while it rings, for the
// tests that start partway through one.
func (h *harness) insertRinging(t *testing.T) *database.Call {
	t.Helper()
	callID := newCallID()
	call := &database.Call{
		CallID:     callID,
		PortalID:   h.mx.portal.ID,
		RoomID:     h.mx.portal.MXID,
		Direction:  database.DirectionInbound,
		Conference: h.conferenceFor(h.mx.portal.ID),
		LKRoom:     LiveKitRoomName(h.mx.portal.MXID.String(), SlotRoom),
		LKIdentity: h.identityFor(h.intent.GetMXID(), callID).participant,
		State:      database.StateRinging,
	}
	if err := h.db.Call.Insert(context.Background(), call); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	return call
}

// insertRingingOutbound writes the row a call the Matrix side placed has while
// the phone rings, which is the only window a decline acts in.
func (h *harness) insertRingingOutbound(t *testing.T) *database.Call {
	t.Helper()
	call := h.insertRinging(t)
	call.Direction = database.DirectionOutbound
	if _, err := h.db.Exec(context.Background(),
		"UPDATE sip_call SET direction = 'outbound' WHERE call_id = $1", call.CallID); err != nil {
		t.Fatalf("mark the call outbound: %v", err)
	}
	return call
}

// backdate moves a row's timestamps into the past, which is how a test reaches
// a state only the passage of time produces.
func (h *harness) backdate(t *testing.T, call *database.Call, by time.Duration) {
	t.Helper()
	then := time.Now().Add(-by).UnixMilli()
	_, err := h.db.Exec(context.Background(),
		"UPDATE sip_call SET created_at = $2, updated_at = $2 WHERE call_id = $1", call.CallID, then)
	if err != nil {
		t.Fatalf("backdate call: %v", err)
	}
	call.CreatedAt = time.UnixMilli(then)
	call.UpdatedAt = time.UnixMilli(then)
}
