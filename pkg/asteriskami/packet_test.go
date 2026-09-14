package asteriskami

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func readAll(t *testing.T, wire string) []*Packet {
	t.Helper()
	r := bufio.NewReader(strings.NewReader(wire))
	var out []*Packet
	for {
		p, err := ReadPacket(r)
		if err != nil {
			return out
		}
		out = append(out, p)
	}
}

func TestReadPacket(t *testing.T) {
	tests := []struct {
		name  string
		wire  string
		event string
		want  map[string]string
	}{
		{
			name:  "inbound sms user event",
			wire:  "Event: UserEvent\r\nPrivilege: user,all\r\nChannel: PJSIP/trunk-00000001\r\nUserEvent: SipMessage\r\nFrom: +15551234567\r\nTo: +15559876543\r\nBody: hello there\r\n\r\n",
			event: "UserEvent",
			want: map[string]string{
				"UserEvent": "SipMessage",
				"From":      "+15551234567",
				"To":        "+15559876543",
				"Body":      "hello there",
			},
		},
		{
			name:  "confbridge join",
			wire:  "Event: ConfbridgeJoin\r\nConference: sip-15551234567\r\nChannel: PJSIP/trunk-0000000a\r\nCallerIDNum: +15551234567\r\nCallerIDName: Example Caller\r\nUniqueid: 1234567890.10\r\n\r\n",
			event: "ConfbridgeJoin",
			want: map[string]string{
				"Conference":  "sip-15551234567",
				"Channel":     "PJSIP/trunk-0000000a",
				"CallerIDNum": "+15551234567",
				"Uniqueid":    "1234567890.10",
			},
		},
		{
			name:  "case insensitive lookup",
			wire:  "Event: Hangup\r\nCHANNEL: PJSIP/trunk-1\r\nCause: 16\r\n\r\n",
			event: "Hangup",
			want:  map[string]string{"channel": "PJSIP/trunk-1", "cause": "16"},
		},
		{
			name:  "value containing a colon",
			wire:  "Event: UserEvent\r\nUserEvent: SipMessage\r\nBody: see http://example.com/x for details\r\n\r\n",
			event: "UserEvent",
			want:  map[string]string{"Body": "see http://example.com/x for details"},
		},
		{
			name:  "empty value",
			wire:  "Event: UserEvent\r\nUserEvent: SipMessage\r\nBody: \r\nFrom: +15551234567\r\n\r\n",
			event: "UserEvent",
			want:  map[string]string{"Body": "", "From": "+15551234567"},
		},
		{
			name:  "bare LF line endings",
			wire:  "Event: Hangup\nChannel: PJSIP/trunk-2\n\n",
			event: "Hangup",
			want:  map[string]string{"Channel": "PJSIP/trunk-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkts := readAll(t, tt.wire)
			if len(pkts) != 1 {
				t.Fatalf("got %d packets, want 1", len(pkts))
			}
			p := pkts[0]
			if p.Event() != tt.event {
				t.Errorf("Event() = %q, want %q", p.Event(), tt.event)
			}
			for k, v := range tt.want {
				if got := p.Get(k); got != v {
					t.Errorf("Get(%q) = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestReadPacketMultiple(t *testing.T) {
	wire := "Event: ConfbridgeJoin\r\nConference: sip-15551234567\r\n\r\n" +
		"Event: ConfbridgeLeave\r\nConference: sip-15551234567\r\n\r\n" +
		"Response: Success\r\nActionID: 7\r\nMessage: Message successfully sent\r\n\r\n"
	pkts := readAll(t, wire)
	if len(pkts) != 3 {
		t.Fatalf("got %d packets, want 3", len(pkts))
	}
	if pkts[0].Event() != "ConfbridgeJoin" || pkts[1].Event() != "ConfbridgeLeave" {
		t.Errorf("unexpected event order: %q, %q", pkts[0].Event(), pkts[1].Event())
	}
	if !pkts[2].IsSuccess() {
		t.Error("third packet should be a success response")
	}
	if pkts[2].ActionID() != "7" {
		t.Errorf("ActionID = %q, want 7", pkts[2].ActionID())
	}
	if pkts[2].Event() != "" {
		t.Errorf("a response packet should have no Event, got %q", pkts[2].Event())
	}
}

func TestRepeatedKeys(t *testing.T) {
	wire := "Event: Newchannel\r\nChanVariable: A=1\r\nChanVariable: B=2\r\nChanVariable: C=3\r\n\r\n"
	p := readAll(t, wire)[0]
	got := p.GetAll("ChanVariable")
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetAll = %v, want %v", got, want)
	}
	if p.Get("ChanVariable") != "A=1" {
		t.Errorf("Get should return the first value, got %q", p.Get("ChanVariable"))
	}
}

func TestUserEventName(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"user event", "Event: UserEvent\r\nUserEvent: SipMessage\r\n\r\n", "SipMessage"},
		{"not a user event", "Event: ConfbridgeJoin\r\nUserEvent: SipMessage\r\n\r\n", ""},
		{"user event with no name", "Event: UserEvent\r\n\r\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readAll(t, tt.wire)[0].UserEventName(); got != tt.want {
				t.Errorf("UserEventName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMarshal(t *testing.T) {
	p := NewPacket()
	p.Set("Action", "MessageSend")
	p.Set("To", "pjsip:+15551234567@trunk")
	p.Set("From", "+15559876543")
	p.Set("Body", "hello")
	got := string(p.Marshal())
	want := "Action: MessageSend\r\nTo: pjsip:+15551234567@trunk\r\nFrom: +15559876543\r\nBody: hello\r\n\r\n"
	if got != want {
		t.Errorf("Marshal() = %q, want %q", got, want)
	}
}

// A newline inside a value would terminate the packet early and let the rest of
// the body be reinterpreted as AMI headers. Every value is scrubbed on write.
func TestMarshalStripsNewlines(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"crlf injection", "hi\r\nAction: Command\r\nCommand: core reload", "hi Action: Command Command: core reload"},
		{"bare lf", "line one\nline two", "line one line two"},
		{"bare cr", "line one\rline two", "line one line two"},
		{"clean body untouched", "just a message", "just a message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPacket()
			p.Set("Action", "MessageSend")
			p.Set("Body", tt.body)
			wire := string(p.Marshal())
			if strings.Count(wire, "\r\n") != 3 {
				t.Errorf("expected exactly 3 CRLFs (2 headers + terminator), got wire %q", wire)
			}
			if !strings.Contains(wire, "Body: "+tt.want+"\r\n") {
				t.Errorf("Body not scrubbed as expected, wire = %q", wire)
			}
		})
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	p := NewPacket()
	p.Set("Event", "UserEvent")
	p.Set("UserEvent", "SipMessage")
	p.Set("From", "+15551234567")
	p.Set("Body", "round trip")

	back := readAll(t, string(p.Marshal()))
	if len(back) != 1 {
		t.Fatalf("got %d packets, want 1", len(back))
	}
	for _, k := range []string{"Event", "UserEvent", "From", "Body"} {
		if back[0].Get(k) != p.Get(k) {
			t.Errorf("round trip lost %q: %q != %q", k, back[0].Get(k), p.Get(k))
		}
	}
}

func TestIsConfbridgeEvent(t *testing.T) {
	tests := []struct {
		event string
		want  bool
	}{
		{"ConfbridgeJoin", true},
		{"ConfbridgeLeave", true},
		{"ConfbridgeEnd", true},
		{"confbridgejoin", true},
		{"ConfbridgeStart", false},
		{"Hangup", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			p := NewPacket()
			if tt.event != "" {
				p.Set("Event", tt.event)
			}
			if got := IsConfbridgeEvent(p); got != tt.want {
				t.Errorf("IsConfbridgeEvent(%q) = %v, want %v", tt.event, got, tt.want)
			}
		})
	}
}

func TestReadGreeting(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Asterisk Call Manager/9.0.0\r\nEvent: FullyBooted\r\n\r\n"))
	greeting, err := ReadGreeting(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if greeting != "Asterisk Call Manager/9.0.0" {
		t.Errorf("greeting = %q", greeting)
	}
	p, err := ReadPacket(r)
	if err != nil {
		t.Fatalf("reading after greeting: %v", err)
	}
	if p.Event() != "FullyBooted" {
		t.Errorf("event after greeting = %q", p.Event())
	}
}

func TestReadGreetingRejectsGarbage(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("HTTP/1.1 404 Not Found\r\n"))
	if _, err := ReadGreeting(r); err == nil {
		t.Error("expected an error for a non-AMI greeting")
	}
}
