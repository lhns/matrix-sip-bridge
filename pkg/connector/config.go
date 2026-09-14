package connector

import (
	_ "embed"
	"time"

	"go.mau.fi/util/configupgrade"

	"github.com/lhns/matrix-sip-bridge/pkg/asteriskami"
	"github.com/lhns/matrix-sip-bridge/pkg/calls"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config is the network half of the bridge config file. Nothing in it has a
// useful default: every value describes one particular Asterisk, LiveKit and
// SIP trunk.
type Config struct {
	Asterisk asteriskami.Config `yaml:"asterisk"`
	Messages MessageConfig      `yaml:"messages"`
	Calls    calls.Config       `yaml:"calls"`
}

// MessageConfig is the SIP MESSAGE contract with the dialplan.
type MessageConfig struct {
	// Enabled turns the messaging half on. Calls and messages are independent;
	// either can run alone.
	Enabled bool `yaml:"enabled"`
	// UserEvent is the name of the dialplan UserEvent that carries an inbound
	// SIP MESSAGE. The dialplan must also set From, To and Body headers on it.
	UserEvent string `yaml:"user_event"`
	// OutboundTo is the Asterisk message URI template for outbound messages.
	// "{number}" is replaced with the E.164 destination including the plus.
	OutboundTo string `yaml:"outbound_to"`
	// OutboundFrom is the From URI presented on outbound messages.
	OutboundFrom string `yaml:"outbound_from"`
	// MaxLength is advertised to Matrix clients as the room's text limit.
	MaxLength int `yaml:"max_length"`
}

// LoginID is the single static UserLogin the bridge runs under.
//
// The SIP side has no per-user accounts: there is one Asterisk and one trunk,
// so every Matrix user shares it. Relay mode plus this fixed login is what
// makes that expressible in bridgev2, which otherwise assumes one remote
// account per Matrix user.
const LoginID = "asterisk"

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "asterisk", "address")
	helper.Copy(configupgrade.Str, "asterisk", "username")
	helper.Copy(configupgrade.Str, "asterisk", "secret")
	helper.Copy(configupgrade.Bool, "asterisk", "tls")
	helper.Copy(configupgrade.Str, "asterisk", "timeout")
	helper.Copy(configupgrade.Str, "asterisk", "reconnect_interval")

	helper.Copy(configupgrade.Bool, "messages", "enabled")
	helper.Copy(configupgrade.Str, "messages", "user_event")
	helper.Copy(configupgrade.Str, "messages", "outbound_to")
	helper.Copy(configupgrade.Str, "messages", "outbound_from")
	helper.Copy(configupgrade.Int, "messages", "max_length")

	helper.Copy(configupgrade.Bool, "calls", "enabled")
	helper.Copy(configupgrade.Str, "calls", "membership_expiry")
	helper.Copy(configupgrade.Str, "calls", "ring_timeout")

	helper.Copy(configupgrade.Str, "calls", "livekit", "url")
	helper.Copy(configupgrade.Str, "calls", "livekit", "api_key")
	helper.Copy(configupgrade.Str, "calls", "livekit", "api_secret")
	helper.Copy(configupgrade.Str, "calls", "livekit", "timeout")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_name")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_address")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_number")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_auth_username")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_auth_password")
	helper.Copy(configupgrade.Str, "calls", "livekit", "trunk_reconcile_interval")

	helper.Copy(configupgrade.Str, "calls", "asterisk", "conference_prefix")
	helper.Copy(configupgrade.Str, "calls", "asterisk", "outbound_channel")
	helper.Copy(configupgrade.Str, "calls", "asterisk", "context")
	helper.Copy(configupgrade.Str, "calls", "asterisk", "extension")
	helper.Copy(configupgrade.Str, "calls", "asterisk", "caller_id")
	helper.Copy(configupgrade.Int, "calls", "asterisk", "originate_timeout")
}

// GetConfig returns the example config, the struct to unmarshal into and the
// upgrader that migrates an existing file.
func (c *SIPConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &c.Config, &configupgrade.StructUpgrader{
		SimpleUpgrader: upgradeConfig,
		Blocks: [][]string{
			{"asterisk"},
			{"messages"},
			{"calls"},
		},
		Base: ExampleConfig,
	}
}

// applyDefaults fills in the values that have a sensible default because they
// are protocol constants rather than site facts.
func (c *Config) applyDefaults() {
	if c.Messages.UserEvent == "" {
		c.Messages.UserEvent = "SipMessage"
	}
	if c.Messages.MaxLength <= 0 {
		// SIP MESSAGE has no limit of its own, but a message that becomes an
		// SMS downstream is segmented past this.
		c.Messages.MaxLength = 1600
	}
	if c.Calls.Asterisk.ConferencePrefix == "" {
		c.Calls.Asterisk.ConferencePrefix = "sip-"
	}
	if c.Calls.LiveKit.TrunkName == "" {
		c.Calls.LiveKit.TrunkName = "matrix-sip-bridge"
	}
	if c.Calls.LiveKit.TrunkReconcileInterval <= 0 {
		c.Calls.LiveKit.TrunkReconcileInterval = 5 * time.Minute
	}
}
