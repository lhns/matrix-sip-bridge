package connector

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
	"github.com/lhns/matrix-sip-bridge/pkg/phonenum"
	"github.com/lhns/matrix-sip-bridge/pkg/siptransport"
)

func TestParseInboundMessage(t *testing.T) {
	tests := []struct {
		name     string
		in       inboundMessage
		wantFrom string
		wantTo   string
		wantBody string
		wantErr  bool
	}{
		{
			name:     "plain e164",
			in:       inboundMessage{From: "+15551234567", To: "+15559876543", Body: "hello"},
			wantFrom: "+15551234567",
			wantTo:   "+15559876543",
			wantBody: "hello",
		},
		{
			name:     "sip uris",
			in:       inboundMessage{From: "sip:+15551234567@pbx.example.com", To: "sip:+15559876543@pbx.example.com", Body: "hi"},
			wantFrom: "+15551234567",
			wantTo:   "+15559876543",
			wantBody: "hi",
		},
		{
			// An odd To must not stop the message being bridged: the sender is
			// what the portal is keyed on.
			name:     "unparseable recipient is tolerated",
			in:       inboundMessage{From: "+15551234567", To: "voicemail", Body: "hi"},
			wantFrom: "+15551234567",
			wantTo:   "",
			wantBody: "hi",
		},
		{
			name:    "unparseable sender is rejected",
			in:      inboundMessage{From: "anonymous", To: "+15559876543", Body: "hi"},
			wantErr: true,
		},
		{
			name:    "missing sender is rejected",
			in:      inboundMessage{To: "+15559876543", Body: "hi"},
			wantErr: true,
		},
		{
			name:    "empty body is rejected",
			in:      inboundMessage{From: "+15551234567", To: "+15559876543", Body: ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInboundMessage(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.From != tt.wantFrom || got.To != tt.wantTo || got.Body != tt.wantBody {
				t.Errorf("got %+v, want From=%q To=%q Body=%q", got, tt.wantFrom, tt.wantTo, tt.wantBody)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	var c Config
	c.applyDefaults()
	if c.Messages.MaxLength != 1600 {
		t.Errorf("MaxLength default = %d", c.Messages.MaxLength)
	}
	if c.Calls.ConferencePrefix != "sip-" {
		t.Errorf("ConferencePrefix default = %q", c.Calls.ConferencePrefix)
	}
	if c.SIP.Transport != "tcp" {
		t.Errorf("SIP transport default = %q, want tcp", c.SIP.Transport)
	}
	if c.Calls.LiveKit.TrunkReconcileInterval == 0 {
		t.Error("TrunkReconcileInterval has no default")
	}
}

// Defaults must not overwrite anything the operator actually set.
func TestApplyDefaultsKeepsConfiguredValues(t *testing.T) {
	var c Config
	c.Messages.MaxLength = 140
	c.Calls.ConferencePrefix = "tel_"
	c.applyDefaults()
	if c.Messages.MaxLength != 140 || c.Calls.ConferencePrefix != "tel_" {
		t.Errorf("applyDefaults overwrote configured values: %+v", c)
	}
}

// The example config is the base every config upgrade is merged onto, so a key
// renamed in the struct and not in the YAML silently stops being configurable.
func TestExampleConfigMatchesTheStruct(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(ExampleConfig), &c); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	if c.SIP.Transport != "tcp" {
		t.Errorf("sip.transport = %q, want tcp", c.SIP.Transport)
	}
	if c.SIP.ConferenceHeader != "X-Conference" {
		t.Errorf("sip.conference_header = %q", c.SIP.ConferenceHeader)
	}
	if c.SIP.MediaPort == 0 {
		t.Error("sip.media_port did not parse")
	}
	if !c.Messages.Enabled || c.Messages.OutboundTo == "" {
		t.Errorf("messages block did not parse: %+v", c.Messages)
	}
	if !c.Calls.Enabled || c.Calls.ConferencePrefix == "" || c.Calls.OutboundURI == "" {
		t.Errorf("calls block did not parse: %+v", c.Calls)
	}
	if c.Calls.MembershipExpiry != 6*time.Hour {
		t.Errorf("calls.membership_expiry = %v, want 6h", c.Calls.MembershipExpiry)
	}
	if c.Calls.ParticipantPollInterval == 0 {
		t.Error("calls.participant_poll_interval did not parse")
	}
	if c.Calls.LiveKit.TrunkName == "" || c.Calls.LiveKit.TrunkReconcileInterval == 0 {
		t.Errorf("calls.livekit block did not parse: %+v", c.Calls.LiveKit)
	}
}

// bridgev2 removes every joined member a full list omits, with the reason
// "User is not in remote chat". The SIP side has no idea who is in the portal
// on the Matrix side, so claiming the list is complete kicks the owning user
// out of their own room on every call.
func TestChatInfoDoesNotClaimAFullMemberList(t *testing.T) {
	sc := &SIPClient{}
	info, err := sc.GetChatInfo(context.Background(), &bridgev2.Portal{
		Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "15551234567"}},
	})
	if err != nil {
		t.Fatalf("GetChatInfo: %v", err)
	}
	if info.Members.IsFull {
		t.Error("the member list must not be marked full")
	}
}

// A portal is one phone number and two parties. Without the DM room type
// bridgev2 creates the room with is_direct unset and never puts it in the
// user's m.direct, and the call presents as a group call.
func TestChatInfoIsADirectChat(t *testing.T) {
	sc := &SIPClient{}
	info, err := sc.GetChatInfo(context.Background(), &bridgev2.Portal{
		Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "15551234567"}},
	})
	if err != nil {
		t.Fatalf("GetChatInfo: %v", err)
	}
	if info.Type == nil || *info.Type != database.RoomTypeDM {
		t.Errorf("room type = %v, want %q", info.Type, database.RoomTypeDM)
	}
	// Without this bridgev2 can only find the DM partner in a member list
	// marked full, which is the thing that kicks the Matrix user.
	if info.Members.OtherUserID != networkid.UserID("15551234567") {
		t.Errorf("other user = %q, want the portal's number", info.Members.OtherUserID)
	}
	if info.Members.IsFull {
		t.Error("the member list must not be marked full")
	}
}

// The two ways an inbound text disappears, both of which are invisible from
// the SIP side: the first because the bridge deliberately answers 200 to a
// sender it cannot key a portal on, and the second because nothing is logged
// anywhere the far end can read. The counters are the only trace either
// leaves.
func TestInboundMessageDropsAreCounted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      siptransport.InboundMessage
		outcome string
		// wantErr is what the transport turns into a status: nil is answered
		// 200, an error 500.
		wantErr bool
	}{
		{
			name:    "unparseable sender",
			in:      siptransport.InboundMessage{From: "sip:anonymous@example.com", To: "sip:15551234567@example.com", Body: "hi"},
			outcome: metrics.MessageDroppedBadSender,
		},
		{
			name:    "nobody logged in",
			in:      siptransport.InboundMessage{From: "sip:+15551234567@example.com", To: "sip:15557654321@example.com", Body: "hi"},
			outcome: metrics.MessageDroppedNoLogin,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			sc := &SIPConnector{
				br:      &bridgev2.Bridge{Log: zerolog.Nop()},
				metrics: metrics.New(reg),
			}
			err := sc.handleInboundMessage(context.Background(), tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("handleInboundMessage error = %v, want error: %v", err, tc.wantErr)
			}
			if got := counter(t, reg, "sip_bridge_messages_total", metrics.DirectionInbound, tc.outcome); got != 1 {
				t.Errorf("messages_total{inbound,%s} = %v, want 1", tc.outcome, got)
			}
		})
	}
}

// counter reads one series of a labelled counter out of a registry.
func counter(t *testing.T, g prometheus.Gatherer, name string, labels ...string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var values []string
			for _, l := range m.GetLabel() {
				values = append(values, l.GetValue())
			}
			if slices.Equal(values, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no series %s%v in the registry", name, labels)
	return 0
}

// Two lines to the same number are two portal rooms, and the room list is
// where a user has to tell them apart.
func TestPortalNameCarriesTheLineButTheGhostDoesNot(t *testing.T) {
	tests := []struct {
		portalID   string
		roomName   string
		ghostName  string
		identifier []string
	}{
		{"15551234567", "+15551234567", "+15551234567", []string{"tel:+15551234567"}},
		{"home-+15551234567", "+15551234567 (home)", "+15551234567", []string{"tel:+15551234567"}},
		{"office-main-+15551234567", "+15551234567 (office-main)", "+15551234567", []string{"tel:+15551234567"}},
		// A number that only means something on its own line is not a tel:
		// URI, so the ghost publishes none.
		{"office-1001", "1001 (office)", "1001", nil},
	}
	for _, tt := range tests {
		t.Run(tt.portalID, func(t *testing.T) {
			var sc SIPClient
			portal := &bridgev2.Portal{Portal: &database.Portal{
				PortalKey: networkid.PortalKey{ID: networkid.PortalID(tt.portalID)},
			}}
			info, err := sc.GetChatInfo(context.Background(), portal)
			if err != nil {
				t.Fatalf("GetChatInfo: %v", err)
			}
			if info.Name == nil || *info.Name != tt.roomName {
				t.Errorf("room name = %v, want %q", info.Name, tt.roomName)
			}

			// The ghost is the person on the other end, who is not per-line.
			ghost := &bridgev2.Ghost{Ghost: &database.Ghost{ID: networkid.UserID(tt.portalID)}}
			user, err := sc.GetUserInfo(context.Background(), ghost)
			if err != nil {
				t.Fatalf("GetUserInfo: %v", err)
			}
			if user.Name == nil || *user.Name != tt.ghostName {
				t.Errorf("ghost name = %v, want %q", user.Name, tt.ghostName)
			}
			if !slices.Equal(user.Identifiers, tt.identifier) {
				t.Errorf("ghost identifiers = %v, want %v", user.Identifiers, tt.identifier)
			}
		})
	}
}

// One person, one conversation. The key a call uses is its conference header
// with calls.conference_prefix removed -- "sip-home-+15551234567" gives
// "home-+15551234567" -- so an inbound text on that line has to produce the
// same string, or the same human gets two rooms and two ghosts.
func TestInboundMessageKeysThePortalOnTheLine(t *testing.T) {
	tests := []struct {
		name string
		from string
		want string
	}{
		{"line in the host", "sip:+15551234567@home", "home-+15551234567"},
		{"hyphenated line", "sip:+15551234567@office-main", "office-main-+15551234567"},
		{"line with a port", "sip:+15551234567@home:5060", "home-+15551234567"},
		// The SIP server falls back to the trunk's own hostname when it cannot
		// resolve a line. A portal keyed on a trunk name is a room no call
		// would ever land in, so this keeps the line-less key instead.
		{"trunk hostname is not a line", "sip:+15551234567@sip.example.com", "15551234567"},
		{"sbc address is not a line", "sip:+15551234567@198.51.100.7", "15551234567"},
		{"no host at all", "+15551234567", "15551234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := parseInboundMessage(inboundMessage{From: tt.from, To: "+15559876543", Body: "hi"})
			if err != nil {
				t.Fatalf("parseInboundMessage: %v", err)
			}
			// From stays a number whatever the host said; the line rides
			// beside it.
			if msg.From != "+15551234567" {
				t.Errorf("From = %q, want the E.164 number", msg.From)
			}
			got, err := phonenum.IDFor(msg.Line, msg.From)
			if err != nil {
				t.Fatalf("IDFor(%q, %q): %v", msg.Line, msg.From, err)
			}
			if got != tt.want {
				t.Errorf("portal key = %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeMemberLister is the GetMembers half of the Matrix connector.
type fakeMemberLister struct {
	members map[id.UserID]*event.MemberEventContent
	err     error
	calls   int
}

func (f *fakeMemberLister) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	f.calls++
	return f.members, f.err
}

// A resync re-invites the user to every portal it touches, so a room the user
// has left must not be resynced at startup.
func TestShouldResyncPortal(t *testing.T) {
	const user = id.UserID("@alice:example.com")
	for _, tc := range []struct {
		name     string
		members  map[id.UserID]*event.MemberEventContent
		err      error
		roomType database.RoomType
		want     bool
		// wantLookups is how often the membership was read: a space is
		// skipped before that costs a request.
		wantLookups int
	}{
		{
			name:        "joined",
			members:     map[id.UserID]*event.MemberEventContent{user: {Membership: event.MembershipJoin}},
			want:        true,
			wantLookups: 1,
		},
		{
			// A space has no room type to repair and no members to sync, and
			// resyncing it only re-invites the user to it.
			name:        "a line's space is not resynced",
			members:     map[id.UserID]*event.MemberEventContent{user: {Membership: event.MembershipJoin}},
			roomType:    database.RoomTypeSpace,
			want:        false,
			wantLookups: 0,
		},
		{
			name:        "invited",
			members:     map[id.UserID]*event.MemberEventContent{user: {Membership: event.MembershipInvite}},
			want:        true,
			wantLookups: 1,
		},
		{
			name:        "left",
			members:     map[id.UserID]*event.MemberEventContent{user: {Membership: event.MembershipLeave}},
			want:        false,
			wantLookups: 1,
		},
		{
			name:        "banned",
			members:     map[id.UserID]*event.MemberEventContent{user: {Membership: event.MembershipBan}},
			want:        false,
			wantLookups: 1,
		},
		{
			name:        "no member event",
			members:     map[id.UserID]*event.MemberEventContent{"@bob:example.com": {Membership: event.MembershipJoin}},
			want:        false,
			wantLookups: 1,
		},
		{
			// Room-type repair is what resyncPortals exists for; a failed
			// lookup must not quietly stop it.
			name:        "lookup error resyncs anyway",
			err:         errors.New("no"),
			want:        true,
			wantLookups: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mx := &fakeMemberLister{members: tc.members, err: tc.err}
			log := zerolog.Nop()
			portal := &bridgev2.Portal{Portal: &database.Portal{
				PortalKey: networkid.PortalKey{ID: "15551234567"},
				MXID:      "!portal:example.com",
				RoomType:  tc.roomType,
			}}
			got := shouldResyncPortal(context.Background(), mx, portal, user, &log)
			if got != tc.want {
				t.Errorf("shouldResyncPortal = %v, want %v", got, tc.want)
			}
			if mx.calls != tc.wantLookups {
				t.Errorf("GetMembers called %d times, want %d", mx.calls, tc.wantLookups)
			}
		})
	}
}

// portalFor is a portal with nothing but its key, which is all GetChatInfo
// reads.
func portalFor(portalID string) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: networkid.PortalID(portalID)},
	}}
}

// Every portal on a line names that line's space as its parent, which is how
// bridgev2 puts the room in it -- and creates the space if it does not exist.
// A portal with no line has no space to go in and must stay parentless: there
// is no default line.
func TestChatInfoParentsAPortalOnItsLine(t *testing.T) {
	tests := []struct {
		portalID string
		want     string
	}{
		{"home-+15551234567", "home-space"},
		{"work-+15551234567", "work-space"},
		{"office-main-+15551234567", "office-main-space"},
		// A short number is still on a line.
		{"office-1001", "office-space"},
		// From before lines, and the key an inbound text with no line makes.
		{"15551234567", ""},
	}
	var sc SIPClient
	for _, tt := range tests {
		t.Run(tt.portalID, func(t *testing.T) {
			info, err := sc.GetChatInfo(context.Background(), portalFor(tt.portalID))
			if err != nil {
				t.Fatalf("GetChatInfo: %v", err)
			}
			if tt.want == "" {
				// nil, not the empty ID: an empty one would unparent the room.
				if info.ParentID != nil {
					t.Errorf("ParentID = %q, want no parent at all", *info.ParentID)
				}
				return
			}
			if info.ParentID == nil || string(*info.ParentID) != tt.want {
				t.Errorf("ParentID = %v, want %q", info.ParentID, tt.want)
			}
		})
	}
	// Two lines, two spaces.
	home, _ := sc.GetChatInfo(context.Background(), portalFor("home-+15551234567"))
	work, _ := sc.GetChatInfo(context.Background(), portalFor("work-+15551234567"))
	if home.ParentID == nil || work.ParentID == nil || *home.ParentID == *work.ParentID {
		t.Errorf("two lines share the space %v", home.ParentID)
	}
}

// GetChatInfo is shared by every portal, so the space portal flows through
// code written for phone numbers: it must come back as a space named after the
// line rather than a DM with a "+<garbage>" name and a ghost in it.
func TestChatInfoOfALineSpace(t *testing.T) {
	var sc SIPClient
	portal := portalFor("home-space")
	info, err := sc.GetChatInfo(context.Background(), portal)
	if err != nil {
		t.Fatalf("GetChatInfo: %v", err)
	}
	if info.Type == nil || *info.Type != database.RoomTypeSpace {
		t.Errorf("room type = %v, want %q", info.Type, database.RoomTypeSpace)
	}
	if info.Name == nil || *info.Name != "home" {
		t.Errorf("name = %v, want the line name", info.Name)
	}
	// A space is not a conversation: a ghost member would be a person the
	// line is not, and OtherUserID is what puts is_direct on the room.
	if info.Members != nil {
		t.Errorf("the space has a member list: %+v", info.Members)
	}
	if info.ParentID != nil {
		t.Errorf("the space has a parent %q; lines do not nest", *info.ParentID)
	}
	// A space carries no messages, so it advertises no text length.
	if feats := sc.GetCapabilities(context.Background(), portal); feats.MaxTextLength != 0 {
		t.Errorf("the space advertises max_text_length %d", feats.MaxTextLength)
	}
}

// A space has no number half, so a text in it could only be addressed to
// "+home-space". Nothing routes that, and the refusal belongs here rather
// than at the SIP server.
func TestALineSpaceCannotBeTexted(t *testing.T) {
	var sc SIPClient
	_, err := sc.HandleMatrixMessage(context.Background(), &bridgev2.MatrixMessage{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
			Portal:  portalFor("home-space"),
			Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"},
		},
	})
	if err == nil {
		t.Fatal("a message in a line's space was accepted")
	}
}

// Nothing a user can type resolves to a space: Normalize sees a number, and a
// space ID is not one. Were it to resolve, the command would offer to start a
// conversation with a room.
func TestALineSpaceIsNotResolvableAsAnIdentifier(t *testing.T) {
	var sc SIPClient
	for _, identifier := range []string{"home-space", "home", "+home-space"} {
		if resp, err := sc.ResolveIdentifier(context.Background(), identifier, false); err == nil {
			t.Errorf("ResolveIdentifier(%q) resolved to %+v", identifier, resp)
		}
	}
}

// renamedPortal is a portal a user has already renamed.
func renamedPortal(portalID string) *bridgev2.Portal {
	portal := portalFor(portalID)
	portal.Metadata = &PortalMetadata{NameSetByUser: true}
	return portal
}

// The bridge owns a portal's name until a user takes it: GetChatInfo must then
// leave the name out of the description entirely, because every other value --
// bridgev2.DefaultChatName above all -- is an instruction to change it.
func TestChatInfoLeavesAUserRenamedRoomAlone(t *testing.T) {
	for _, tc := range []struct {
		portalID  string
		wantName  string
		wantSpace bool
	}{
		{portalID: "home-+15551234567", wantName: "+15551234567 (home)"},
		{portalID: "15551234567", wantName: "+15551234567"},
		// A line's space is named after the line, and is renameable too.
		{portalID: "home-space", wantName: "home", wantSpace: true},
	} {
		t.Run(tc.portalID, func(t *testing.T) {
			var sc SIPClient
			info, err := sc.GetChatInfo(context.Background(), portalFor(tc.portalID))
			if err != nil {
				t.Fatalf("GetChatInfo: %v", err)
			}
			if info.Name == nil || *info.Name != tc.wantName {
				t.Fatalf("name = %v, want %q", info.Name, tc.wantName)
			}

			renamed, err := sc.GetChatInfo(context.Background(), renamedPortal(tc.portalID))
			if err != nil {
				t.Fatalf("GetChatInfo: %v", err)
			}
			if renamed.Name != nil {
				t.Errorf("name = %q, want nil so that the room keeps the user's", *renamed.Name)
			}
			// Suppressing the name must not quietly change the room into
			// something else.
			if (renamed.Type == nil) != (info.Type == nil) || *renamed.Type != *info.Type {
				t.Errorf("room type = %v, want %v", renamed.Type, info.Type)
			}
			if tc.wantSpace {
				if renamed.Members != nil {
					t.Errorf("the space grew a member list: %+v", renamed.Members)
				}
				if renamed.ParentID != nil {
					t.Errorf("the space grew the parent %q", *renamed.ParentID)
				}
				return
			}
			if renamed.Members == nil || renamed.Members.OtherUserID != info.Members.OtherUserID {
				t.Errorf("member list = %+v, want the same DM partner as %+v", renamed.Members, info.Members)
			}
			if renamed.Members.IsFull {
				t.Error("the member list must not be marked full")
			}
			if (renamed.ParentID == nil) != (info.ParentID == nil) ||
				(renamed.ParentID != nil && *renamed.ParentID != *info.ParentID) {
				t.Errorf("ParentID = %v, want %v", renamed.ParentID, info.ParentID)
			}
		})
	}
}

// A rename by anyone but the bridge itself has to be remembered, or the next
// thing that reasserts the portal's description takes it away again.
func TestHandleMatrixRoomNameRecordsTheUsersName(t *testing.T) {
	var sc SIPClient
	portal := portalFor("home-+15551234567")
	changed, err := sc.HandleMatrixRoomName(context.Background(), &bridgev2.MatrixRoomName{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RoomNameEventContent]{
			Portal:  portal,
			Content: &event.RoomNameEventContent{Name: "Plumber"},
		},
	})
	if err != nil || !changed {
		t.Fatalf("HandleMatrixRoomName = %v, %v, want true, nil", changed, err)
	}
	if portal.Name != "Plumber" || !portal.NameSet {
		t.Errorf("portal name = %q, set = %v", portal.Name, portal.NameSet)
	}
	if !portalMeta(portal).NameSetByUser {
		t.Error("the rename was not recorded, so the next resync will overwrite it")
	}
	info, err := sc.GetChatInfo(context.Background(), portal)
	if err != nil {
		t.Fatalf("GetChatInfo: %v", err)
	}
	if info.Name != nil {
		t.Errorf("the very next description still names the room %q", *info.Name)
	}
}

// Element's rename dialog can submit an empty name, and it has to mean "give
// the bridge's name back" rather than a room left nameless forever.
func TestEmptyRoomNameResetsToTheBridgeName(t *testing.T) {
	for _, tc := range []struct{ portalID, want string }{
		{"home-+15551234567", "+15551234567 (home)"},
		{"home-space", "home"},
	} {
		t.Run(tc.portalID, func(t *testing.T) {
			var sc SIPClient
			portal := renamedPortal(tc.portalID)
			portal.Name = "Plumber"
			portal.NameSet = true
			changed, err := sc.HandleMatrixRoomName(context.Background(), &bridgev2.MatrixRoomName{
				MatrixEventBase: bridgev2.MatrixEventBase[*event.RoomNameEventContent]{
					Portal:  portal,
					Content: &event.RoomNameEventContent{Name: ""},
				},
			})
			if err != nil || !changed {
				t.Fatalf("HandleMatrixRoomName = %v, %v, want true, nil", changed, err)
			}
			if portalMeta(portal).NameSetByUser {
				t.Error("the portal is still marked as the user's, so the name never comes back")
			}
			if portal.Name != tc.want {
				t.Errorf("portal name = %q, want the bridge's %q", portal.Name, tc.want)
			}
			// NameSet false is what makes the resync re-send a name the portal
			// already records: updateName early-returns otherwise.
			if portal.NameSet {
				t.Error("NameSet is still true, so the room keeps the empty name")
			}
			info, err := sc.GetChatInfo(context.Background(), portal)
			if err != nil {
				t.Fatalf("GetChatInfo: %v", err)
			}
			if info.Name == nil || *info.Name != tc.want {
				t.Errorf("name = %v, want the bridge's %q again", info.Name, tc.want)
			}
		})
	}
}

// The room needs two unrelated grants: the call membership, without which
// there is no call button, and m.room.name, without which there is no rename.
// Neither must cost the other.
func TestPortalPowerLevelsAllowBothCallsAndRenaming(t *testing.T) {
	levels := portalEventPowerLevels()
	if level, ok := levels[event.StateRoomName]; !ok || level != 0 {
		t.Errorf("m.room.name = %v (present: %v), want 0", level, ok)
	}
	for evtType := range calls.MembershipPowerLevels() {
		if level, ok := levels[evtType]; !ok || level != 0 {
			t.Errorf("%s = %v (present: %v), want 0", evtType.Type, level, ok)
		}
	}
	var sc SIPClient
	info, err := sc.GetChatInfo(context.Background(), portalFor("home-+15551234567"))
	if err != nil {
		t.Fatalf("GetChatInfo: %v", err)
	}
	if info.Members.PowerLevels == nil {
		t.Fatal("the portal grants no power levels at all")
	}
	if got := info.Members.PowerLevels.Events[event.StateRoomName]; got != 0 {
		t.Errorf("the portal grants m.room.name %d, want 0", got)
	}
}

// The flag rides in bridgev2's portal.metadata column, so it survives a
// restart only if it round-trips through JSON under a stable key.
func TestPortalMetadataRoundTripsThroughJSON(t *testing.T) {
	raw, err := json.Marshal(&PortalMetadata{NameSetByUser: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta PortalMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if !meta.NameSetByUser {
		t.Errorf("name_set_by_user did not survive %s", raw)
	}
	// An empty metadata object is what every portal created before this flag
	// existed has, and it must read as "the bridge still owns the name".
	var old PortalMetadata
	if err := json.Unmarshal([]byte(`{}`), &old); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if old.NameSetByUser {
		t.Error("an empty metadata object read as a user rename")
	}
}
