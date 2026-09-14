package calls

import (
	"strings"
	"testing"
)

// The two vectors below come from the MSC4195 appendix and are cross-checked
// against element-hq/lk-jwt-service. They are the whole point of this file: if
// they drift, the bridge silently joins a LiveKit room nobody else is in.
func TestLiveKitRoomName(t *testing.T) {
	tests := []struct {
		name     string
		roomID   string
		slot     string
		want     string
		wantOnly string
	}{
		{
			name:   "msc4195 test vector",
			roomID: "!roomid:example.com",
			slot:   "slot1234",
			want:   "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc",
		},
		{
			name:   "empty inputs are still hashed",
			roomID: "",
			slot:   "",
			want:   hashIdentifiers("", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LiveKitRoomName(tt.roomID, tt.slot); got != tt.want {
				t.Errorf("LiveKitRoomName(%q, %q) = %q, want %q", tt.roomID, tt.slot, got, tt.want)
			}
		})
	}
}

func TestLiveKitIdentity(t *testing.T) {
	got := LiveKitIdentity("@alice:example.com", "DEVICE123", "memberABC")
	want := "J+T45tGruxc+HrUOqJJlyQSV33m728Cme4+vt8/SWrU"
	if got != want {
		t.Errorf("LiveKitIdentity = %q, want %q", got, want)
	}
}

func TestHashFormat(t *testing.T) {
	got := LiveKitRoomName("!room:example.com", SlotRoom)
	if strings.Contains(got, "=") {
		t.Errorf("hash is padded: %q", got)
	}
	if strings.ContainsAny(got, "-_") {
		t.Errorf("hash uses the URL-safe alphabet, want standard: %q", got)
	}
	// 32 bytes of SHA-256 in unpadded base64.
	if len(got) != 43 {
		t.Errorf("hash length = %d, want 43", len(got))
	}
}

func TestHashDistinctAndDeterministic(t *testing.T) {
	inputs := [][2]string{
		{"!room1:example.com", SlotRoom},
		{"!room2:example.com", SlotRoom},
		{"!room1:example.com", "m.call#OTHER"},
		{"", ""},
	}
	seen := map[string][2]string{}
	for _, in := range inputs {
		got := LiveKitRoomName(in[0], in[1])
		if again := LiveKitRoomName(in[0], in[1]); again != got {
			t.Errorf("LiveKitRoomName is not deterministic for %v", in)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %v and %v both hash to %q", in, prev, got)
		}
		seen[got] = in
	}
}

// Go's encoding/json escapes <, > and & unless the encoder is told not to.
// A Matrix room ID cannot contain them, but the derivation is shared with
// identities and with any future MSC4195 hash, so escaping must be off
// unconditionally and the array must be compact.
func TestMarshalStrings(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{"room and slot", []string{"!roomid:example.com", "slot1234"}, `["!roomid:example.com","slot1234"]`},
		{"no spaces between elements", []string{"a", "b", "c"}, `["a","b","c"]`},
		{"ampersand is not escaped", []string{"a&b"}, `["a&b"]`},
		{"angle brackets are not escaped", []string{"c<d>e"}, `["c<d>e"]`},
		{"empty strings", []string{"", ""}, `["",""]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(marshalStrings(tt.parts))
			if got != tt.want {
				t.Errorf("marshalStrings(%q) = %s, want %s", tt.parts, got, tt.want)
			}
		})
	}
}
