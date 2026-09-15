package calls

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"
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

// The identity the bridge hands to CreateSIPParticipant and the member ID it
// publishes in the RTC membership are two halves of one value. Element Call
// derives the identities it will accept from the membership list and discards
// every track it cannot match, so a disagreement here is a call that connects,
// reports every track subscribed and healthy, and is silent.
func TestParticipantIdentityMatchesTheMemberID(t *testing.T) {
	const (
		user   = "@sip_15551234567:example.com"
		device = "SIPABCD"
		opaque = "0123456789abcdef"
	)
	tests := []struct {
		scheme   IdentityScheme
		memberID string
		identity string
	}{
		{
			scheme:   IdentityUserDevice,
			memberID: user + ":" + device,
			identity: user + ":" + device,
		},
		{
			scheme:   IdentityHashed,
			memberID: opaque,
			identity: LiveKitIdentity(user, device, opaque),
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.scheme), func(t *testing.T) {
			memberID := MemberIDFor(tt.scheme, user, device, opaque)
			if memberID != tt.memberID {
				t.Errorf("MemberIDFor = %q, want %q", memberID, tt.memberID)
			}
			identity := ParticipantIdentityFor(tt.scheme, user, device, memberID)
			if identity != tt.identity {
				t.Errorf("ParticipantIdentityFor = %q, want %q", identity, tt.identity)
			}
		})
	}
}

// The scheme a deployment needs cannot be guessed from the room, so it is
// configured. This pins which one an unconfigured bridge emits.
func TestDefaultIdentitySchemeIsUserDevice(t *testing.T) {
	s := New(Config{}, nil, "", nil, nil, zerolog.Nop())
	if s.cfg.IdentityScheme != IdentityUserDevice {
		t.Errorf("default identity scheme = %q, want %q", s.cfg.IdentityScheme, IdentityUserDevice)
	}
	// The unhashed identity must be the member ID verbatim: it is compared as
	// a string by every other participant, not re-derived.
	const user, device = "@sip_15551234567:example.com", "SIPABCD"
	memberID := MemberIDFor(s.cfg.IdentityScheme, user, device, "ignored")
	if got := ParticipantIdentityFor(s.cfg.IdentityScheme, user, device, memberID); got != memberID {
		t.Errorf("identity %q is not the member ID %q", got, memberID)
	}
}

// The participant identity is now written by the INSERT that creates the call
// row, while the member ID is published later by publishGhostMembership. Both
// come from identityFor so that they cannot drift; a drift is inaudible on
// both sides and reports no error anywhere.
func TestIdentityForIsSelfConsistent(t *testing.T) {
	const (
		user   = id.UserID("@sip_15551234567:example.com")
		callID = "0123456789abcdef0123456789abcdef"
	)
	for _, scheme := range []IdentityScheme{IdentityUserDevice, IdentityHashed} {
		t.Run(string(scheme), func(t *testing.T) {
			s := New(Config{IdentityScheme: scheme}, nil, "", nil, nil, zerolog.Nop())
			ident := s.identityFor(user, callID)

			if ident.userID != user {
				t.Errorf("userID = %q, want %q", ident.userID, user)
			}
			if ident.deviceID != deviceIDFor(callID) {
				t.Errorf("deviceID = %q, want %q", ident.deviceID, deviceIDFor(callID))
			}
			wantMember := MemberIDFor(scheme, user.String(), ident.deviceID, callID)
			if ident.membershipID != wantMember {
				t.Errorf("membershipID = %q, want %q", ident.membershipID, wantMember)
			}
			want := ParticipantIdentityFor(scheme, user.String(), ident.deviceID, ident.membershipID)
			if ident.participant != want {
				t.Errorf("participant = %q, want %q", ident.participant, want)
			}
			// Derived twice for the same call, it has to come out the same:
			// the row is written before the membership is published.
			if again := s.identityFor(user, callID); again != ident {
				t.Errorf("identityFor is not deterministic: %+v then %+v", ident, again)
			}
		})
	}
}

// deviceIDFor runs inside a Matrix event handler, on a call ID that came back
// from the database rather than from newCallID. Slicing it took the handler
// down for every later event as well.
func TestDeviceIDForShortCallID(t *testing.T) {
	tests := []struct {
		callID string
		want   string
	}{
		{"", "SIP"},
		{"abc", "SIPABC"},
		{"abcdefgh", "SIPABCDEFGH"},
		{"abcdefghij", "SIPABCDEFGH"},
	}
	for _, tt := range tests {
		if got := deviceIDFor(tt.callID); got != tt.want {
			t.Errorf("deviceIDFor(%q) = %q, want %q", tt.callID, got, tt.want)
		}
	}
}
