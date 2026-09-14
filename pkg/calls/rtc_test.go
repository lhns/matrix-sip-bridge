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

// The state key format is fiddly and getting it wrong means the event is
// refused by the server or ignored by clients. Without MSC3757 a state key
// starting with "@" must equal the sender, so the whole thing is pushed behind
// an underscore.
func TestRTCStateKey(t *testing.T) {
	const user = "@sip_15551234567:example.com"
	tests := []struct {
		name  string
		owned bool
		want  string
	}{
		{"ordinary room version", false, "_@sip_15551234567:example.com_SIPABCD_m.call#ROOM"},
		{"room version with owned state keys", true, "@sip_15551234567:example.com_SIPABCD_m.call#ROOM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rtcStateKey(idUser(user), "SIPABCD", SlotRoom, tt.owned); got != tt.want {
				t.Errorf("rtcStateKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSupportsOwnedStateKeys(t *testing.T) {
	tests := map[string]bool{
		"10":                    false,
		"11":                    false,
		"12":                    false,
		"org.matrix.msc3757.11": true,
		"org.matrix.msc3779.12": true,
		"org.example.custom":    false,
	}
	for version, want := range tests {
		if got := supportsOwnedStateKeys(id.RoomVersion(version)); got != want {
			t.Errorf("supportsOwnedStateKeys(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestGhostMembershipIsRoomScoped(t *testing.T) {
	const user = "@sip_15551234567:example.com"
	content := ghostMembership(idUser(user), "SIPABCD", "m1", time.Hour)
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
	// checkRtcMembershipData refuses a membership whose member.user_id is not
	// the sender, so the ghost must name itself here.
	member, ok := raw["member"].(map[string]any)
	if !ok {
		t.Fatalf("member = %v, want an object", raw["member"])
	}
	if member["user_id"] != user {
		t.Errorf("member.user_id = %v, want %q", member["user_id"], user)
	}
	if member["device_id"] != "SIPABCD" || member["id"] != "m1" {
		t.Errorf("member identity = %v/%v, want SIPABCD/m1", member["device_id"], member["id"])
	}
}

// The LiveKit identity must be the hash of exactly the triple written into the
// membership, or Element Call filters the ghost's audio track out and the call
// is silent as well as unattributed.
func TestGhostMembershipMatchesLiveKitIdentity(t *testing.T) {
	const user = "@sip_15551234567:example.com"
	member := ghostMembership(idUser(user), "SIPABCD", "m1", time.Hour).Raw["member"].(map[string]any)
	want := LiveKitIdentity(user, "SIPABCD", "m1")
	got := LiveKitIdentity(
		member["user_id"].(string),
		member["device_id"].(string),
		member["id"].(string),
	)
	if got != want {
		t.Errorf("identity from the published membership = %q, want %q", got, want)
	}
}

func TestLeaveMembershipIsEmpty(t *testing.T) {
	if len(leaveMembership().Raw) != 0 {
		t.Error("a leave must be an empty content object")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func idUser(s string) id.UserID { return id.UserID(s) }
