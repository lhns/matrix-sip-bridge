package connector

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestOutboundURITemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		number   string
		want     string
	}{
		{"sip uri", "sip:{number}@pbx.example.com", "+15551234567", "sip:+15551234567@pbx.example.com"},
		{"other domain", "sip:{number}@sms.example.com", "+442071234567", "sip:+442071234567@sms.example.com"},
		{"template with no placeholder is left alone", "sip:voicemail@pbx.example.com", "+15551234567", "sip:voicemail@pbx.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.ReplaceAll(tt.template, "{number}", tt.number)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
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

// The example config is shipped as the base for config upgrades, so it must
// parse and must not carry any real deployment's details.
func TestExampleConfigIsGeneric(t *testing.T) {
	if !strings.Contains(ExampleConfig, "example.com") {
		t.Error("example config should use example.com hostnames")
	}
	for _, forbidden := range []string{"lhns.de", "10.1.", "eventphone", "rfn.de"} {
		if strings.Contains(ExampleConfig, forbidden) {
			t.Errorf("example config leaks a site-specific value: %q", forbidden)
		}
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
