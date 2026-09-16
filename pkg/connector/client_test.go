package connector

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
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
