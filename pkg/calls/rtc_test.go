package calls

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

func TestParseCallMember(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	past := time.Now().Add(-time.Hour).UnixMilli()

	tests := []struct {
		name         string
		raw          string
		wantActive   bool
		wantDevice   string
		wantMemberID string
	}{
		{
			name:         "inline membership",
			raw:          `{"application":"m.call","call_id":"","scope":"m.room","device_id":"ABCDEF","membershipID":"m1","expires":` + itoa(future) + `}`,
			wantActive:   true,
			wantDevice:   "ABCDEF",
			wantMemberID: "m1",
		},
		{
			name:       "inline membership with no expiry is active",
			raw:        `{"application":"m.call","device_id":"ABCDEF"}`,
			wantActive: true,
			wantDevice: "ABCDEF",
		},
		{
			name:       "inline membership past its expiry",
			raw:        `{"application":"m.call","device_id":"ABCDEF","expires":` + itoa(past) + `}`,
			wantActive: false,
			wantDevice: "ABCDEF",
		},
		{
			name:         "legacy memberships array",
			raw:          `{"memberships":[{"application":"m.call","call_id":"","device_id":"XYZ","membershipID":"m2","expires":` + itoa(future) + `}]}`,
			wantActive:   true,
			wantDevice:   "XYZ",
			wantMemberID: "m2",
		},
		{
			name:       "legacy array with only expired entries",
			raw:        `{"memberships":[{"device_id":"XYZ","expires":` + itoa(past) + `}]}`,
			wantActive: false,
			wantDevice: "XYZ",
		},
		{
			name:       "legacy empty array is a leave",
			raw:        `{"memberships":[]}`,
			wantActive: false,
		},
		{
			name:       "empty object is a leave",
			raw:        `{}`,
			wantActive: false,
		},
		{
			name:       "absent content is a leave",
			raw:        ``,
			wantActive: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, active, err := ParseCallMember(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if active != tt.wantActive {
				t.Errorf("active = %v, want %v", active, tt.wantActive)
			}
			if got := content.FirstDeviceID(); got != tt.wantDevice {
				t.Errorf("FirstDeviceID() = %q, want %q", got, tt.wantDevice)
			}
			if got := content.FirstMembershipID(); got != tt.wantMemberID {
				t.Errorf("FirstMembershipID() = %q, want %q", got, tt.wantMemberID)
			}
		})
	}
}

func TestParseCallMemberRejectsGarbage(t *testing.T) {
	if _, _, err := ParseCallMember(json.RawMessage(`not json`)); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

func TestExpired(t *testing.T) {
	now := time.Unix(1000000, 0)
	tests := []struct {
		name    string
		expires int64
		want    bool
	}{
		{"zero means no expiry set", 0, false},
		{"negative means no expiry set", -1, false},
		{"in the past", now.Add(-time.Second).UnixMilli(), true},
		{"in the future", now.Add(time.Second).UnixMilli(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expired(tt.expires, now); got != tt.want {
				t.Errorf("expired(%d) = %v, want %v", tt.expires, got, tt.want)
			}
		})
	}
}

func TestRTCStateKey(t *testing.T) {
	tests := []struct {
		name   string
		user   string
		device string
		want   string
	}{
		{"with device", "@sip_15551234567:example.com", "SIPABCD", "@sip_15551234567:example.com_SIPABCD"},
		{"without device", "@sip_15551234567:example.com", "", "@sip_15551234567:example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rtcStateKey(idUser(tt.user), tt.device); got != tt.want {
				t.Errorf("rtcStateKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGhostMembershipIsRoomScoped(t *testing.T) {
	content := ghostMembership("SIPABCD", "m1", time.Hour)
	raw := content.Raw
	if raw["application"] != "m.call" {
		t.Errorf("application = %v, want m.call", raw["application"])
	}
	// A room-scoped call, the kind Element's native call button starts, has an
	// empty call_id. A non-empty one would put the ghost in a different
	// session from the Matrix user.
	if raw["call_id"] != "" {
		t.Errorf("call_id = %v, want empty", raw["call_id"])
	}
	if raw["scope"] != "m.room" {
		t.Errorf("scope = %v, want m.room", raw["scope"])
	}
	expires, ok := raw["expires"].(int64)
	if !ok || expires <= time.Now().UnixMilli() {
		t.Errorf("expires = %v, want a future timestamp", raw["expires"])
	}
}

func TestLeaveMembershipIsEmpty(t *testing.T) {
	if len(leaveMembership().Raw) != 0 {
		t.Error("a leave must be an empty content object")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func idUser(s string) id.UserID { return id.UserID(s) }
