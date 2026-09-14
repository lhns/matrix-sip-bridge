package siptransport

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Config describes the bridge as a SIP endpoint.
//
// The bridge is a UAS that the SIP server dials, not a remote control for one
// particular PBX. Everything here is site-specific.
type Config struct {
	// Listen is the address the bridge accepts SIP on, "host:port".
	Listen string `yaml:"listen"`
	// Transport is "tcp" or "udp". TCP is the default and should stay that
	// way; see maxPacketSize.
	Transport string `yaml:"transport"`
	// PublicAddress is the "host:port" other SIP elements reach the bridge on,
	// used in Contact and Via. Defaults to Listen, which is wrong behind NAT.
	PublicAddress string `yaml:"public_address"`

	// Username and Domain form the bridge's own SIP identity,
	// "sip:<username>@<domain>".
	Username string `yaml:"username"`
	Domain   string `yaml:"domain"`

	// Register controls whether the bridge REGISTERs. A SIP server that has
	// the bridge as a static endpoint needs no registration.
	Register RegisterConfig `yaml:"register"`

	// MediaAddress and MediaPort are what the bridge writes into SDP. No
	// socket is ever opened on them: the bridge has no media stack. See
	// sdp.go for why the answer must nonetheless be a real, non-held one on
	// a non-zero port. MediaAddress defaults to the host of PublicAddress.
	MediaAddress string `yaml:"media_address"`
	MediaPort    int    `yaml:"media_port"`

	// ConferenceHeader names the header the dialplan puts the conference name
	// in, e.g. SIPAddHeader(X-Conference: sip-15551234567).
	ConferenceHeader string `yaml:"conference_header"`
}

// RegisterConfig is the bridge's registration with the SIP server.
type RegisterConfig struct {
	Enabled bool `yaml:"enabled"`
	// Server is the "host:port" REGISTER is sent to.
	Server   string `yaml:"server"`
	Password string `yaml:"password"`
	// Expiry is the requested registration lifetime. It is refreshed at half
	// this interval.
	Expiry time.Duration `yaml:"expiry"`
}

// ApplyDefaults fills in the values that are protocol constants rather than
// site facts.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:5060"
	}
	if c.Transport == "" {
		c.Transport = "tcp"
	}
	if c.PublicAddress == "" {
		c.PublicAddress = c.Listen
	}
	if c.Username == "" {
		c.Username = "matrix-sip-bridge"
	}
	if c.ConferenceHeader == "" {
		c.ConferenceHeader = "X-Conference"
	}
	if c.MediaPort <= 0 {
		c.MediaPort = 40000
	}
	if c.Register.Expiry <= 0 {
		c.Register.Expiry = 5 * time.Minute
	}
}

// Validate reports the mistakes that would otherwise show up as a silently
// dead endpoint.
func (c *Config) Validate() error {
	if _, _, err := splitHostPort(c.Listen); err != nil {
		return fmt.Errorf("sip.listen %q: %w", c.Listen, err)
	}
	if _, _, err := splitHostPort(c.PublicAddress); err != nil {
		return fmt.Errorf("sip.public_address %q: %w", c.PublicAddress, err)
	}
	switch strings.ToLower(c.Transport) {
	case "tcp", "udp":
	default:
		return fmt.Errorf("sip.transport %q is not tcp or udp", c.Transport)
	}
	if c.MediaPort <= 0 || c.MediaPort > 65535 {
		return fmt.Errorf("sip.media_port %d is not a port", c.MediaPort)
	}
	if c.Domain == "" {
		return fmt.Errorf("sip.domain is required")
	}
	if c.Register.Enabled && c.Register.Server == "" {
		return fmt.Errorf("sip.register.enabled needs sip.register.server")
	}
	return nil
}

// publicHostPort is the address the bridge advertises to the SIP server.
func (c *Config) publicHostPort() (string, int) {
	host, port, _ := splitHostPort(c.PublicAddress)
	return host, port
}

// mediaHost is the address written into SDP. It must be an IP literal, so a
// hostname is resolved once at startup rather than per call.
func (c *Config) mediaHost() string {
	if c.MediaAddress != "" {
		return c.MediaAddress
	}
	host, _ := c.publicHostPort()
	return host
}

func splitHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("bad port %q", portStr)
	}
	return host, port, nil
}
