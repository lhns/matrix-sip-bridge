package connector

import (
	_ "embed"
	"time"

	"go.mau.fi/util/configupgrade"

	"github.com/lhns/matrix-sip-bridge/pkg/calls"
	"github.com/lhns/matrix-sip-bridge/pkg/siptransport"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config is the network half of the bridge config file. Most of it describes
// one particular SIP server, LiveKit and trunk; applyDefaults fills in only
// the values that are protocol constants rather than site facts.
type Config struct {
	SIP      siptransport.Config `yaml:"sip"`
	Messages MessageConfig       `yaml:"messages"`
	Calls    calls.Config        `yaml:"calls"`
}

// MessageConfig is the text-messaging half.
type MessageConfig struct {
	// Enabled turns the messaging half on. Calls and messages are independent;
	// either can run alone.
	Enabled bool `yaml:"enabled"`
	// OutboundTo is the SIP URI template for outbound messages. "{number}" is
	// replaced with the E.164 destination including the plus.
	OutboundTo string `yaml:"outbound_to"`
	// OutboundFrom is the From URI presented on outbound messages.
	OutboundFrom string `yaml:"outbound_from"`
	// MaxLength is advertised to Matrix clients as the room's text limit.
	MaxLength int `yaml:"max_length"`
}

// LoginID is the single static UserLogin the bridge runs under.
//
// There are no per-user accounts on the SIP side: there is one endpoint and
// one trunk, so every Matrix user shares it. Relay mode plus this fixed login
// is what makes that expressible in bridgev2, which otherwise assumes one
// remote account per Matrix user.
const LoginID = "sip"

// LoginFlowID is the ID of the only login flow.
const LoginFlowID = "sip"

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "sip", "listen")
	helper.Copy(configupgrade.Str, "sip", "transport")
	helper.Copy(configupgrade.Str, "sip", "public_address")
	helper.Copy(configupgrade.Str, "sip", "username")
	helper.Copy(configupgrade.Str, "sip", "domain")
	helper.Copy(configupgrade.Str, "sip", "media_address")
	helper.Copy(configupgrade.Int, "sip", "media_port")
	helper.Copy(configupgrade.Str, "sip", "conference_header")
	helper.Copy(configupgrade.Bool, "sip", "register", "enabled")
	helper.Copy(configupgrade.Str, "sip", "register", "server")
	helper.Copy(configupgrade.Str, "sip", "register", "password")
	helper.Copy(configupgrade.Str, "sip", "register", "expiry")

	helper.Copy(configupgrade.Bool, "messages", "enabled")
	helper.Copy(configupgrade.Str, "messages", "outbound_to")
	helper.Copy(configupgrade.Str, "messages", "outbound_from")
	helper.Copy(configupgrade.Int, "messages", "max_length")

	helper.Copy(configupgrade.Bool, "calls", "enabled")
	helper.Copy(configupgrade.Str, "calls", "conference_prefix")
	helper.Copy(configupgrade.Str, "calls", "outbound_uri")
	helper.Copy(configupgrade.Str, "calls", "membership_expiry")
	helper.Copy(configupgrade.Str, "calls", "ring_timeout")
	helper.Copy(configupgrade.Str, "calls", "identity_scheme")
	helper.Copy(configupgrade.Str, "calls", "participant_poll_interval")
	helper.Copy(configupgrade.Bool, "calls", "notices", "call_ended")
	helper.Copy(configupgrade.Bool, "calls", "notices", "call_failed")
	helper.Copy(configupgrade.Bool, "calls", "notices", "sip_down")
	helper.Copy(configupgrade.Bool, "calls", "notices", "trunk_missing")

	helper.Copy(configupgrade.Str, "calls", "livekit", "url")
	helper.Copy(configupgrade.Str, "calls", "livekit", "api_key")
	helper.Copy(configupgrade.Str, "calls", "livekit", "api_secret")
	helper.Copy(configupgrade.Str, "calls", "livekit", "timeout")
	helper.Copy(configupgrade.Str, "calls", "livekit", "jwt_service_url")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_name")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_address")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_number")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_auth_username")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_auth_password")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_reconcile_interval")
}

// GetConfig returns the example config, the struct to unmarshal into and the
// upgrader that migrates an existing file.
func (c *SIPConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &c.Config, &configupgrade.StructUpgrader{
		SimpleUpgrader: upgradeConfig,
		Blocks: [][]string{
			{"sip"},
			{"messages"},
			{"calls"},
		},
		Base: ExampleConfig,
	}
}

// applyDefaults fills in the values that have a sensible default because they
// are protocol constants rather than site facts.
func (c *Config) applyDefaults() {
	c.SIP.ApplyDefaults()
	if c.Messages.MaxLength <= 0 {
		// SIP MESSAGE is capped by the 20480-byte packet limit, but a message
		// that becomes an SMS downstream is segmented well before that.
		c.Messages.MaxLength = 1600
	}
	if c.Calls.ConferencePrefix == "" {
		c.Calls.ConferencePrefix = "sip-"
	}
	if c.Calls.LiveKit.TrunkName == "" {
		c.Calls.LiveKit.TrunkName = "matrix-sip-bridge"
	}
	if c.Calls.LiveKit.TrunkReconcileInterval <= 0 {
		c.Calls.LiveKit.TrunkReconcileInterval = 5 * time.Minute
	}
}
