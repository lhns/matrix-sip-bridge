package calls

import "testing"

func TestConferenceNameRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		portalID   string
		conference string
	}{
		{"default prefix", "sip-", "15551234567", "sip-15551234567"},
		{"underscore prefix", "matrix_", "442071234567", "matrix_442071234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subsystem{}
			s.cfg.Asterisk.ConferencePrefix = tt.prefix

			if got := s.conferenceFor(tt.portalID); got != tt.conference {
				t.Errorf("conferenceFor(%q) = %q, want %q", tt.portalID, got, tt.conference)
			}
			got, ok := s.portalIDFromConference(tt.conference)
			if !ok {
				t.Fatalf("portalIDFromConference(%q) did not match", tt.conference)
			}
			if got != tt.portalID {
				t.Errorf("portalIDFromConference(%q) = %q, want %q", tt.conference, got, tt.portalID)
			}
		})
	}
}

// Asterisk may host conferences the bridge knows nothing about. Reacting to
// those would create portal rooms for whatever their names happen to contain.
func TestPortalIDFromConferenceRejectsForeignRooms(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		conference string
	}{
		{"different prefix", "sip-", "meeting-1234"},
		{"prefix but nothing after it", "sip-", "sip-"},
		{"prefix in the middle", "sip-", "team-sip-1234"},
		{"empty conference", "sip-", ""},
		{"no prefix configured matches nothing", "", "sip-15551234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subsystem{}
			s.cfg.Asterisk.ConferencePrefix = tt.prefix
			if got, ok := s.portalIDFromConference(tt.conference); ok {
				t.Errorf("portalIDFromConference(%q) matched as %q, want no match", tt.conference, got)
			}
		})
	}
}

func TestNewCallIDIsUniqueAndLongEnough(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newCallID()
		// publishGhostMembership slices the first 8 characters for the device
		// ID, so anything shorter would panic at runtime.
		if len(id) != 32 {
			t.Fatalf("call ID %q has length %d, want 32", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate call ID %q", id)
		}
		seen[id] = true
	}
}

func TestWantedTrunk(t *testing.T) {
	s := &Subsystem{}
	s.cfg.LiveKit = LiveKitConfig{
		TrunkName:         "matrix-sip-bridge",
		TrunkAddress:      "pbx.example.com",
		TrunkNumber:       "+15551234567",
		TrunkAuthUsername: "bridge",
		TrunkAuthPassword: "secret",
	}
	trunk := s.wantedTrunk()
	if trunk.Name != "matrix-sip-bridge" || trunk.Address != "pbx.example.com" {
		t.Errorf("unexpected trunk identity: %+v", trunk)
	}
	if len(trunk.Numbers) != 1 || trunk.Numbers[0] != "+15551234567" {
		t.Errorf("Numbers = %v, want one entry", trunk.Numbers)
	}

	s.cfg.LiveKit.TrunkNumber = ""
	if got := s.wantedTrunk(); got.Numbers != nil {
		t.Errorf("Numbers = %v, want nil when no number is configured", got.Numbers)
	}
}

func TestCurrentTrunkIDBeforeReconcile(t *testing.T) {
	s := &Subsystem{}
	if _, err := s.currentTrunkID(); err == nil {
		t.Error("expected an error before the trunk has been reconciled")
	}
	empty := ""
	s.trunkID.Store(&empty)
	if _, err := s.currentTrunkID(); err == nil {
		t.Error("expected an error for an empty trunk ID")
	}
	id := "ST_abc123"
	s.trunkID.Store(&id)
	got, err := s.currentTrunkID()
	if err != nil || got != id {
		t.Errorf("currentTrunkID() = %q, %v; want %q, nil", got, err, id)
	}
}
