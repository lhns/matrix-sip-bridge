package calls

import (
	"testing"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/event"
)

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
