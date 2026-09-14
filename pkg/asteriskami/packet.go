// Package asteriskami is a minimal client for the Asterisk Manager Interface.
//
// AMI is the bridge's only interface to Asterisk: outbound SMS goes out as a
// MessageSend action, inbound SMS arrives as a UserEvent fired by the dialplan,
// and call state is read from ConfbridgeJoin/ConfbridgeLeave events.
package asteriskami

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Packet is one AMI message: an ordered set of "Key: Value" lines terminated by
// a blank line. Keys are case-insensitive on the wire, so they are canonicalised
// to Title-Case on parse and looked up case-insensitively.
type Packet struct {
	// Fields holds every value seen for a key, in wire order. AMI repeats keys
	// (Variable:, ChanVariable:, Output:), so a plain map would lose data.
	Fields map[string][]string
	// Order is the canonicalised key names in first-seen order.
	Order []string
}

// NewPacket returns an empty packet ready for Set.
func NewPacket() *Packet {
	return &Packet{Fields: make(map[string][]string)}
}

// canonKey normalises an AMI header name for map lookup: "actionid" and
// "ActionID" are the same key.
func canonKey(k string) string {
	return strings.ToLower(strings.TrimSpace(k))
}

// Set replaces any existing values for key.
func (p *Packet) Set(key, value string) {
	ck := canonKey(key)
	if _, exists := p.Fields[ck]; !exists {
		p.Order = append(p.Order, key)
	}
	p.Fields[ck] = []string{value}
}

// Add appends a value, keeping any already present.
func (p *Packet) Add(key, value string) {
	ck := canonKey(key)
	if _, exists := p.Fields[ck]; !exists {
		p.Order = append(p.Order, key)
	}
	p.Fields[ck] = append(p.Fields[ck], value)
}

// Get returns the first value for key, or "" if absent.
func (p *Packet) Get(key string) string {
	v := p.Fields[canonKey(key)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// GetAll returns every value for key in wire order.
func (p *Packet) GetAll(key string) []string {
	return p.Fields[canonKey(key)]
}

// Has reports whether the key was present, even with an empty value.
func (p *Packet) Has(key string) bool {
	_, ok := p.Fields[canonKey(key)]
	return ok
}

// Event returns the Event header, which is set on asynchronous events only.
func (p *Packet) Event() string { return p.Get("Event") }

// Response returns the Response header, which is set on action replies only.
func (p *Packet) Response() string { return p.Get("Response") }

// ActionID correlates an action with its reply.
func (p *Packet) ActionID() string { return p.Get("ActionID") }

// IsSuccess reports whether an action reply was accepted. Asterisk answers
// "Success", and for list-producing actions "Success" followed by list events.
func (p *Packet) IsSuccess() bool {
	return strings.EqualFold(p.Response(), "Success")
}

// UserEventName returns the UserEvent header, the dialplan-chosen name of a
// UserEvent packet. It is empty for anything that is not a UserEvent.
func (p *Packet) UserEventName() string {
	if !strings.EqualFold(p.Event(), "UserEvent") {
		return ""
	}
	return p.Get("UserEvent")
}

// Marshal renders the packet in AMI wire format, CRLF-terminated with a
// trailing blank line.
func (p *Packet) Marshal() []byte {
	var b strings.Builder
	for _, key := range p.Order {
		for _, value := range p.Fields[canonKey(key)] {
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(sanitizeValue(value))
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// sanitizeValue removes CR and LF. A newline in a value would end the packet
// early and let the rest be read as headers, so message bodies must be scrubbed
// before they reach the socket.
func sanitizeValue(v string) string {
	if !strings.ContainsAny(v, "\r\n") {
		return v
	}
	r := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")
	return r.Replace(v)
}

// SortedKeys returns the canonical key names sorted, for stable test output.
func (p *Packet) SortedKeys() []string {
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ReadPacket reads one packet, consuming the terminating blank line.
//
// The AMI greeting line ("Asterisk Call Manager/x.y.z") has no colon and no
// blank line of its own; call ReadGreeting before the first ReadPacket.
func ReadPacket(r *bufio.Reader) (*Packet, error) {
	p := NewPacket()
	seenAny := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && seenAny {
				return p, nil
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !seenAny {
				// Tolerate stray blank lines between packets.
				continue
			}
			return p, nil
		}
		seenAny = true
		key, value, found := strings.Cut(line, ":")
		if !found {
			// A continuation line with no colon (Asterisk does this in
			// command output). Attach it to the previous key rather than
			// dropping it.
			if len(p.Order) > 0 {
				last := canonKey(p.Order[len(p.Order)-1])
				p.Fields[last] = append(p.Fields[last], line)
			}
			continue
		}
		p.Add(strings.TrimSpace(key), strings.TrimLeft(value, " "))
	}
}

// ReadGreeting consumes the single banner line Asterisk sends on connect and
// returns it.
func ReadGreeting(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "Asterisk Call Manager") {
		return line, fmt.Errorf("unexpected AMI greeting %q", line)
	}
	return line, nil
}
