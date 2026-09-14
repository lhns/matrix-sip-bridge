package connector

import (
	"strings"
	"testing"

	"github.com/lhns/matrix-sip-bridge/pkg/asteriskami"
)

func packetFrom(fields map[string]string) *asteriskami.Packet {
	p := asteriskami.NewPacket()
	p.Set("Event", "UserEvent")
	for k, v := range fields {
		p.Set(k, v)
	}
	return p
}

func TestParseInboundMessage(t *testing.T) {
	tests := []struct {
		name     string
		fields   map[string]string
		wantFrom string
		wantTo   string
		wantBody string
		wantErr  bool
	}{
		{
			name:     "plain e164",
			fields:   map[string]string{"From": "+15551234567", "To": "+15559876543", "Body": "hello"},
			wantFrom: "+15551234567",
			wantTo:   "+15559876543",
			wantBody: "hello",
		},
		{
			name:     "sip uris",
			fields:   map[string]string{"From": "sip:+15551234567@pbx.example.com", "To": "sip:+15559876543@pbx.example.com", "Body": "hi"},
			wantFrom: "+15551234567",
			wantTo:   "+15559876543",
			wantBody: "hi",
		},
		{
			// An odd To must not stop the message being bridged: the sender is
			// what the portal is keyed on.
			name:     "unparseable recipient is tolerated",
			fields:   map[string]string{"From": "+15551234567", "To": "voicemail", "Body": "hi"},
			wantFrom: "+15551234567",
			wantTo:   "",
			wantBody: "hi",
		},
		{
			name:    "unparseable sender is rejected",
			fields:  map[string]string{"From": "anonymous", "To": "+15559876543", "Body": "hi"},
			wantErr: true,
		},
		{
			name:    "missing sender is rejected",
			fields:  map[string]string{"To": "+15559876543", "Body": "hi"},
			wantErr: true,
		},
		{
			name:    "empty body is rejected",
			fields:  map[string]string{"From": "+15551234567", "To": "+15559876543", "Body": ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInboundMessage(packetFrom(tt.fields))
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
		{"pjsip endpoint", "pjsip:{number}@trunk", "+15551234567", "pjsip:+15551234567@trunk"},
		{"full sip uri", "pjsip:sip:{number}@pbx.example.com", "+442071234567", "pjsip:sip:+442071234567@pbx.example.com"},
		{"template with no placeholder is left alone", "pjsip:voicemail@trunk", "+15551234567", "pjsip:voicemail@trunk"},
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
	if c.Messages.UserEvent != "SipMessage" {
		t.Errorf("UserEvent default = %q", c.Messages.UserEvent)
	}
	if c.Messages.MaxLength != 1600 {
		t.Errorf("MaxLength default = %d", c.Messages.MaxLength)
	}
	if c.Calls.Asterisk.ConferencePrefix != "sip-" {
		t.Errorf("ConferencePrefix default = %q", c.Calls.Asterisk.ConferencePrefix)
	}
	if c.Calls.LiveKit.TrunkReconcileInterval == 0 {
		t.Error("TrunkReconcileInterval has no default")
	}
}

// Defaults must not overwrite anything the operator actually set.
func TestApplyDefaultsKeepsConfiguredValues(t *testing.T) {
	var c Config
	c.Messages.UserEvent = "IncomingText"
	c.Messages.MaxLength = 140
	c.Calls.Asterisk.ConferencePrefix = "tel_"
	c.applyDefaults()
	if c.Messages.UserEvent != "IncomingText" || c.Messages.MaxLength != 140 || c.Calls.Asterisk.ConferencePrefix != "tel_" {
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
