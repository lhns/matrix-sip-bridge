package siptransport

import (
	"strings"
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	// TCP is not a taste: chan_sip caps a SIP packet at 20480 bytes and a
	// multi-part SMS that overflows a UDP datagram is dropped silently.
	if c.Transport != "tcp" {
		t.Errorf("Transport default = %q, want tcp", c.Transport)
	}
	if c.ConferenceHeader != "X-Conference" {
		t.Errorf("ConferenceHeader default = %q", c.ConferenceHeader)
	}
	if c.MediaPort == 0 {
		t.Error("MediaPort has no default; m=audio 0 rejects the stream")
	}
	if c.PublicAddress != c.Listen {
		t.Errorf("PublicAddress = %q, want it to fall back to Listen", c.PublicAddress)
	}
	if c.Register.Expiry != 5*time.Minute {
		t.Errorf("Register.Expiry default = %v", c.Register.Expiry)
	}
}

func TestApplyDefaultsKeepsConfiguredValues(t *testing.T) {
	c := Config{
		Listen:           "192.0.2.1:5062",
		Transport:        "udp",
		PublicAddress:    "sip.example.com:5062",
		ConferenceHeader: "X-Room",
		MediaPort:        12345,
	}
	c.ApplyDefaults()
	if c.Transport != "udp" || c.PublicAddress != "sip.example.com:5062" ||
		c.ConferenceHeader != "X-Room" || c.MediaPort != 12345 {
		t.Errorf("ApplyDefaults overwrote configured values: %+v", c)
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c := Config{Listen: "0.0.0.0:5060", Domain: "example.com"}
		c.ApplyDefaults()
		return c
	}
	tests := []struct {
		name    string
		tune    func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"no domain", func(c *Config) { c.Domain = "" }, "domain"},
		{"listen without port", func(c *Config) { c.Listen = "0.0.0.0" }, "listen"},
		{"unknown transport", func(c *Config) { c.Transport = "sctp" }, "transport"},
		{"registering with no server", func(c *Config) { c.Register.Enabled = true }, "register.server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.tune(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
