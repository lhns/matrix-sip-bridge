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
	RoomID   id.RoomID
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

func (f *fakeIntent) SendState(_ context.Context, roomID id.RoomID, eventType event.Type, stateKey string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	return f.record(sentEvent{RoomID: roomID, Type: eventType, StateKey: stateKey, State: true, Content: content})
}

func (f *fakeIntent) SendMessage(_ context.Context, roomID id.RoomID, eventType event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	return f.record(sentEvent{RoomID: roomID, Type: eventType, Content: content})
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

// countIn is count for one room.
func (f *fakeIntent) countIn(roomID id.RoomID, eventType event.Type) int {
	n := 0
	for _, evt := range f.events() {
		if evt.Type == eventType && evt.RoomID == roomID {
			n++
		}
	}
	return n
}

// bodiesIn is bodies for one room.
func (f *fakeIntent) bodiesIn(roomID id.RoomID) []string {
	var out []string
	for _, evt := range f.events() {
		if evt.Type != event.EventMessage || evt.RoomID != roomID {
			continue
		}
		if content, ok := evt.Content.Parsed.(*event.MessageEventContent); ok {
			out = append(out, content.Body)
		}
	}
	return out
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

// testBob is a second Matrix user, with a room of their own per number.
// testCaller is the default recipient.
const testBob = id.UserID("@bob:example.com")

// fakeMatrix is the bridgev2 side of the world.
type fakeMatrix struct {
	rec    *recorder
	intent *fakeIntent

	// portal is the default recipient's portal. Any other owner gets a room
	// of their own, made on first use.
	portal    Portal
	portalErr error

	// refused and unknown name recipient header values Recipient fails on.
	refused map[string]bool
	unknown map[string]bool

	// portalIDs is what already exists on the bridge for the default
	// recipient, which is what a !dial with no line resolves against;
	// otherPortalIDs is the same for any other user.
	portalIDs      []string
	otherPortalIDs map[id.UserID][]string
	portalsErr     error

	mu sync.Mutex
	// asked and askedOwners record what PortalRoom was called with. They are
	// the only place a dial entry point's ID arithmetic is visible, the
	// portal itself being fixed.
	asked       []string
	askedOwners []id.UserID
	// rooms maps each owner to their room, and members is who is in it.
	rooms   map[id.UserID]id.RoomID
	members map[id.RoomID]map[id.UserID]*event.MemberEventContent
}

func newFakeMatrix(rec *recorder) *fakeMatrix {
	f := &fakeMatrix{
		rec:     rec,
		intent:  newFakeIntent(rec),
		portal:  Portal{ID: "15551234567", MXID: "!portal:example.com", Owner: testCaller},
		refused: map[string]bool{},
		unknown: map[string]bool{},
		rooms:   map[id.UserID]id.RoomID{},
		members: map[id.RoomID]map[id.UserID]*event.MemberEventContent{},
	}
	f.rooms[testCaller] = f.portal.MXID
	f.members[f.portal.MXID] = map[id.UserID]*event.MemberEventContent{
		testCaller: {Membership: event.MembershipJoin, Displayname: "Alice"},
	}
	return f
}

// roomOf returns an owner's room, making it, with the owner joined under the
// given display name, on first use.
func (f *fakeMatrix) roomOf(owner id.UserID, displayname string) id.RoomID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner == "" {
		owner = testCaller
	}
	if room, ok := f.rooms[owner]; ok {
		return room
	}
	room := id.RoomID("!" + strings.TrimPrefix(strings.SplitN(owner.String(), ":", 2)[0], "@") + "-room:example.com")
	f.rooms[owner] = room
	f.members[room] = map[id.UserID]*event.MemberEventContent{
		owner: {Membership: event.MembershipJoin, Displayname: displayname},
	}
	return room
}

func (f *fakeMatrix) GhostIntent(context.Context, string) (GhostIntent, error) {
	return f.intent, nil
}

func (f *fakeMatrix) Recipient(_ context.Context, header string) (id.UserID, error) {
	switch {
	case f.refused[header]:
		return "", ErrRecipientRefused
	case f.unknown[header]:
		return "", ErrUnknownRecipient
	case header == "":
		return testCaller, nil
	}
	return id.UserID(header), nil
}

func (f *fakeMatrix) PortalRoom(_ context.Context, portalID string, owner id.UserID) (Portal, error) {
	f.rec.add("portal")
	f.mu.Lock()
	f.asked = append(f.asked, portalID)
	f.askedOwners = append(f.askedOwners, owner)
	f.mu.Unlock()
	if f.portalErr != nil {
		return Portal{}, f.portalErr
	}
	if owner == "" || owner == testCaller {
		return f.portal, nil
	}
	return Portal{ID: portalID, MXID: f.roomOf(owner, owner.String()), Owner: owner}, nil
}

// portalsAsked returns the portal IDs PortalRoom was called with.
func (f *fakeMatrix) portalsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// ownersAsked returns the owners PortalRoom was called with.
func (f *fakeMatrix) ownersAsked() []id.UserID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]id.UserID(nil), f.askedOwners...)
}

func (f *fakeMatrix) PortalIDs(_ context.Context, owner id.UserID) ([]string, error) {
	if f.portalsErr != nil {
		return nil, f.portalsErr
	}
	if owner == "" || owner == testCaller {
		return f.portalIDs, nil
	}
	return f.otherPortalIDs[owner], nil
}

func (f *fakeMatrix) PortalByMXID(_ context.Context, roomID id.RoomID) (Portal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for owner, room := range f.rooms {
		if room == roomID {
			return Portal{ID: f.portal.ID, MXID: room, Owner: owner}, true
		}
	}
	return Portal{}, false
}

func (f *fakeMatrix) BotMXID() id.UserID { return "@sipbot:example.com" }

func (f *fakeMatrix) IsGhost(userID id.UserID) bool {
	return strings.HasPrefix(userID.String(), "@sip_")
}

func (f *fakeMatrix) Members(_ context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.members[roomID], nil
}

// fakeOutboundLeg is the control leg of a call the bridge placed.
type fakeOutboundLeg struct {
	done chan struct{}
}

func newFakeOutboundLeg() *fakeOutboundLeg {
	return &fakeOutboundLeg{done: make(chan struct{})}
}

func (l *fakeOutboundLeg) Done() <-chan struct{} { return l.done }

func (l *fakeOutboundLeg) Hangup(context.Context) error { return nil }

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

	mu     sync.Mutex
	rec    *recorder
	byRoom int
}

func (q *queryLog) QueryTiming(_ context.Context, _, query string, _ []any, _ int, _ time.Duration, _ error) {
	q.mu.Lock()
	rec := q.rec
	if strings.Contains(query, "WHERE room_id = ") {
		q.byRoom++
	}
	q.mu.Unlock()
	rec.add("db")
}

func (q *queryLog) activeByRoom() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.byRoom
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
		answered: map[string]chan id.UserID{},
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

// activeCall returns the call still in progress in the harness's portal room,
// or nil. "Ended" is the assertion most of these tests make.
func (h *harness) activeCall(t *testing.T) *database.Call {
	t.Helper()
	return h.activeCallIn(t, h.mx.portal.MXID)
}

// activeCallIn is activeCall for any room.
func (h *harness) activeCallIn(t *testing.T, room id.RoomID) *database.Call {
	t.Helper()
	call, err := h.db.Call.GetActiveByRoom(context.Background(), room)
	if err != nil {
		t.Fatalf("GetActiveByRoom: %v", err)
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
		Receiver:   h.mx.portal.Owner,
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
