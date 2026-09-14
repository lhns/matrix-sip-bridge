package asteriskami

import (
	"context"
	"fmt"
	"strings"
)

// MessageSend sends an out-of-call text message through Asterisk's message
// API. With a PJSIP "to" URI this becomes an outbound SIP MESSAGE.
//
// to and from are full technology URIs, not bare numbers, because Asterisk
// routes on the technology prefix: "pjsip:+15551234567@trunk". Building that
// URI is the caller's job, since the endpoint name is site-specific.
func (c *Client) MessageSend(ctx context.Context, to, from, body string) error {
	p := NewPacket()
	p.Set("Action", "MessageSend")
	p.Set("To", to)
	if from != "" {
		p.Set("From", from)
	}
	p.Set("Body", body)
	_, err := c.Action(ctx, p)
	return err
}

// Originate places a call leg and drops it into a dialplan extension.
// Async is always set: a synchronous Originate blocks the whole AMI session
// for the duration of the dial, which would stall every other action.
type OriginateRequest struct {
	Channel  string
	Context  string
	Exten    string
	Priority string
	CallerID string
	Timeout  int
	// Variables are set on the outbound channel before the dialplan runs.
	Variables map[string]string
}

// Originate sends an Originate action. The reply only confirms that Asterisk
// accepted the request; success or failure of the call itself arrives later as
// events.
func (c *Client) Originate(ctx context.Context, req OriginateRequest) error {
	if req.Priority == "" {
		req.Priority = "1"
	}
	p := NewPacket()
	p.Set("Action", "Originate")
	p.Set("Channel", req.Channel)
	p.Set("Context", req.Context)
	p.Set("Exten", req.Exten)
	p.Set("Priority", req.Priority)
	p.Set("Async", "true")
	if req.CallerID != "" {
		p.Set("CallerID", req.CallerID)
	}
	if req.Timeout > 0 {
		p.Set("Timeout", fmt.Sprintf("%d", req.Timeout))
	}
	for k, v := range req.Variables {
		p.Add("Variable", k+"="+v)
	}
	_, err := c.Action(ctx, p)
	return err
}

// Hangup ends a channel.
func (c *Client) Hangup(ctx context.Context, channel string) error {
	p := NewPacket()
	p.Set("Action", "Hangup")
	p.Set("Channel", channel)
	_, err := c.Action(ctx, p)
	return err
}

// ConfbridgeKick removes one channel from a conference.
func (c *Client) ConfbridgeKick(ctx context.Context, conference, channel string) error {
	p := NewPacket()
	p.Set("Action", "ConfbridgeKick")
	p.Set("Conference", conference)
	p.Set("Channel", channel)
	_, err := c.Action(ctx, p)
	return err
}

// AMI event and header names the bridge cares about.
const (
	EventUser           = "UserEvent"
	EventConfbridgeJoin = "ConfbridgeJoin"
	EventConfbridgeLeav = "ConfbridgeLeave"
	EventConfbridgeEnd  = "ConfbridgeEnd"
	EventHangup         = "Hangup"
)

// IsConfbridgeEvent reports whether a packet is one of the ConfBridge events
// the call subsystem tracks.
func IsConfbridgeEvent(p *Packet) bool {
	switch {
	case strings.EqualFold(p.Event(), EventConfbridgeJoin),
		strings.EqualFold(p.Event(), EventConfbridgeLeav),
		strings.EqualFold(p.Event(), EventConfbridgeEnd):
		return true
	}
	return false
}
