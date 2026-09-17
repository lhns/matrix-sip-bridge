package calls

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"
)

func TestParseCallMember(t *testing.T) {
	// "expires" is a duration from the membership's creation, so an event
	// sent an hour ago with a two-hour lifetime is still live and the same
	// event with a one-minute lifetime is not.
	origin := time.Now().Add(-time.Hour)
	const long = "7200000"
	const short = "60000"

	tests := []struct {
		name       string
		raw        string
		wantActive bool
	}{
		{
			name:       "inline membership",
			raw:        `{"application":"m.call","call_id":"","scope":"m.room","device_id":"ABCDEF","membershipID":"m1","expires":` + long + `}`,
			wantActive: true,
		},
		{
			name:       "inline membership with no expiry is active",
			raw:        `{"application":"m.call","device_id":"ABCDEF"}`,
			wantActive: true,
		},
		{
			name:       "inline membership past its expiry",
			raw:        `{"application":"m.call","device_id":"ABCDEF","expires":` + short + `}`,
			wantActive: false,
		},
		{
			name:       "legacy memberships array",
			raw:        `{"memberships":[{"application":"m.call","call_id":"","device_id":"XYZ","membershipID":"m2","expires":` + long + `}]}`,
			wantActive: true,
		},
		{
			name:       "legacy array with only expired entries",
			raw:        `{"memberships":[{"device_id":"XYZ","expires":` + short + `}]}`,
			wantActive: false,
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
			active, err := ParseCallMember(json.RawMessage(tt.raw), origin)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if active != tt.wantActive {
				t.Errorf("active = %v, want %v", active, tt.wantActive)
			}
		})
	}
}

func TestParseCallMemberRejectsGarbage(t *testing.T) {
	if _, err := ParseCallMember(json.RawMessage(`not json`), time.Now()); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

func TestExpired(t *testing.T) {
	now := time.Unix(1000000, 0)
	origin := now.Add(-time.Hour)
	tests := []struct {
		name      string
		createdTS int64
		expires   int64
		want      bool
	}{
		{"zero means no expiry set", 0, 0, false},
		{"negative means no expiry set", 0, -1, false},
		{"duration shorter than the event's age", 0, time.Minute.Milliseconds(), true},
		{"duration longer than the event's age", 0, (2 * time.Hour).Milliseconds(), false},
		{"created_ts wins over the event timestamp", now.UnixMilli(), time.Minute.Milliseconds(), false},
		// The value a client actually sends. Read as a Unix timestamp this is
		// 1970 and every join looks like a leave.
		{"a real four-hour membership is live", 0, 14400000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expired(tt.createdTS, tt.expires, now, origin); got != tt.want {
				t.Errorf("expired(%d, %d) = %v, want %v", tt.createdTS, tt.expires, got, tt.want)
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
	content := ghostMembership(time.Now(), idUser(user), "!portal:example.com", "SIPABCD", "m1",
		"https://matrix-rtc.example.com/livekit/jwt", time.Hour)
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
	// A duration, not a deadline: a client reading this as a Unix timestamp
	// would see 1970 and treat the ghost as gone the moment it arrives.
	if expires, ok := raw["expires"].(int64); !ok || expires != time.Hour.Milliseconds() {
		t.Errorf("expires = %v, want %d", raw["expires"], time.Hour.Milliseconds())
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

// The published membership must carry exactly the triple the participant
// identity was derived from, under either scheme, or Element Call filters the
// ghost's audio track out and the call is silent as well as unattributed.
func TestGhostMembershipMatchesLiveKitIdentity(t *testing.T) {
	const user = "@sip_15551234567:example.com"
	for _, scheme := range []IdentityScheme{IdentityUserDevice, IdentityHashed} {
		t.Run(string(scheme), func(t *testing.T) {
			memberID := MemberIDFor(scheme, user, "SIPABCD", "m1")
			want := ParticipantIdentityFor(scheme, user, "SIPABCD", memberID)
			member := ghostMembership(time.Now(), idUser(user), "!portal:example.com", "SIPABCD", memberID,
				"https://matrix-rtc.example.com/livekit/jwt", time.Hour).Raw["member"].(map[string]any)
			got := ParticipantIdentityFor(scheme,
				member["user_id"].(string),
				member["device_id"].(string),
				member["id"].(string),
			)
			if got != want {
				t.Errorf("identity from the published membership = %q, want %q", got, want)
			}
		})
	}
}

// focus_active and both livekit_* fields are mandatory in the receiving
// parsers. A membership missing any of them is discarded whole, the client
// concludes the room has no active call, and it neither rings nor shows the
// caller -- with nothing logged anywhere.
func TestGhostMembershipCarriesACompleteFocus(t *testing.T) {
	const roomID = "!portal:example.com"
	const jwtURL = "https://matrix-rtc.example.com/livekit/jwt"
	raw := ghostMembership(time.Now(), idUser("@sip_15551234567:example.com"), roomID, "SIPABCD", "m1",
		jwtURL, time.Hour).Raw

	active, ok := raw["focus_active"].(map[string]any)
	if !ok {
		t.Fatalf("focus_active = %v, want an object", raw["focus_active"])
	}
	if active["type"] != "livekit" || active["focus_selection"] == nil {
		t.Errorf("focus_active = %v, want a livekit focus with a selection", active)
	}

	foci, ok := raw["foci_preferred"].([]any)
	if !ok || len(foci) != 1 {
		t.Fatalf("foci_preferred = %v, want one entry", raw["foci_preferred"])
	}
	focus, ok := foci[0].(map[string]any)
	if !ok {
		t.Fatalf("foci_preferred[0] = %v, want an object", foci[0])
	}
	if focus["type"] != "livekit" {
		t.Errorf("focus type = %v, want livekit", focus["type"])
	}
	if focus["livekit_alias"] != roomID {
		t.Errorf("livekit_alias = %v, want %q", focus["livekit_alias"], roomID)
	}
	if focus["livekit_service_url"] != jwtURL {
		t.Errorf("livekit_service_url = %v, want %q", focus["livekit_service_url"], jwtURL)
	}
}

func TestLeaveMembershipIsEmpty(t *testing.T) {
	if len(leaveMembership().Raw) != 0 {
		t.Error("a leave must be an empty content object")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func idUser(s string) id.UserID { return id.UserID(s) }
