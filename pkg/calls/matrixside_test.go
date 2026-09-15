package calls

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

// bridgev2 dereferences the source UserLogin unconditionally while creating a
// room, so a call that arrives before anyone has logged in must be refused
// here. Without this the bridge panicked inside portal creation on the first
// inbound call.
func TestPortalCreationNeedsTheStaticLogin(t *testing.T) {
	b := newBridgeSide(&bridgev2.Bridge{}, "sip", zerolog.Nop())
	login, err := b.sourceLogin()
	if err == nil {
		t.Fatalf("sourceLogin() = %v, want an error when no login is cached", login)
	}
	if login != nil {
		t.Errorf("sourceLogin() returned %v alongside an error", login)
	}
	if !strings.Contains(err.Error(), "sip") {
		t.Errorf("err = %v, want it to name the login", err)
	}
}

// The repair runs on every call, so it has to be able to tell that there is
// nothing to repair. Re-sending room state each time costs a database write
// and shows up in clients as the room changing.
func TestCallPowerLevelsApplied(t *testing.T) {
	applied := &event.PowerLevelsEventContent{Events: map[string]int{}}
	for evtType, level := range MembershipPowerLevels() {
		applied.Events[evtType.Type] = level
	}

	tests := []struct {
		name   string
		levels *event.PowerLevelsEventContent
		want   bool
	}{
		{"a repaired room needs nothing", applied, true},
		{"no power levels at all", nil, false},
		{"a room bridgev2 just created", &event.PowerLevelsEventContent{
			Events:          map[string]int{event.StateEncryption.Type: 100},
			StateDefaultPtr: ptr.Ptr(50),
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := callPowerLevelsApplied(tt.levels); got != tt.want {
				t.Errorf("callPowerLevelsApplied = %v, want %v", got, tt.want)
			}
		})
	}

	// One of the two names being right is not enough: a client that uses the
	// other still sees no call button.
	partial := &event.PowerLevelsEventContent{Events: map[string]int{
		CallMemberEventType.Type: 0,
	}}
	if callPowerLevelsApplied(partial) {
		t.Error("a room granting only one of the membership event names is not repaired")
	}
}
