package calls

import (
	"encoding/json"
	"time"

	"maunium.net/go/mautrix/event"
)

// CallMemberEventType is the MSC3401 / MatrixRTC membership state event.
//
// mautrix-go v0.30 has no MSC3401 support at all: the type is not in
// event.TypeMap, so Content.Parsed stays nil and Content.VeryRaw must be
// decoded by hand. It is also absent from bridgev2's inbound event whitelist,
// which is why the subsystem registers its own EventProcessor handler instead
// of receiving these through the portal event plumbing.
var CallMemberEventType = event.Type{
	Type:  "org.matrix.msc3401.call.member",
	Class: event.StateEventType,
}

// CallMemberStableEventType is the name the membership state event takes once
// MSC3401 lands. It is not sent by the bridge, only permitted: which of the
// two a client writes is the client's choice, and a portal that allows one but
// not the other offers no call button to half of them.
var CallMemberStableEventType = event.Type{
	Type:  "m.call.member",
	Class: event.StateEventType,
}

// MembershipPowerLevels is the power level override a portal room needs for
// its Matrix users to be able to start or join a call at all.
//
// Element only shows the call button in a room where the local user may send
// the RTC membership state event. A bridge portal has state_default 50 and
// leaves the owning user at users_default 0, so without this there is no way
// to place or answer a call — and no error either, just a missing button. A
// room Element itself creates carries exactly this override.
func MembershipPowerLevels() map[event.Type]int {
	return map[event.Type]int{
		CallMemberEventType:       0,
		CallMemberStableEventType: 0,
	}
}

// CallMemberContent is the subset of the membership content the bridge reads.
//
// The event has had two shapes: the original MSC3401 one with a top-level
// "memberships" array, and the current per-device one with the fields inline
// and the device ID in the state key. Both are accepted, because Element X and
// Element Web have not always agreed on which they send.
type CallMemberContent struct {
	// Inline (current) form.
	Application  string `json:"application,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	CreatedTS    int64  `json:"created_ts,omitempty"`
	ExpiresMS    int64  `json:"expires,omitempty"`
	MembershipID string `json:"membershipID,omitempty"`

	// Legacy (array) form.
	Memberships []CallMembership `json:"memberships,omitempty"`
}

// CallMembership is one entry of the legacy memberships array.
type CallMembership struct {
	DeviceID     string `json:"device_id,omitempty"`
	CreatedTS    int64  `json:"created_ts,omitempty"`
	ExpiresMS    int64  `json:"expires,omitempty"`
	MembershipID string `json:"membershipID,omitempty"`
}

// ParseCallMember decodes a call.member state event.
//
// An empty content object is how a client leaves the session: Matrix has no
// state deletion, so the redaction-equivalent is state with no fields. The
// second return value reports whether the event describes an active
// membership, which is the signal the bridge acts on.
//
// origin is the event's own timestamp, needed because "expires" is measured
// from when the membership was created and clients do not all send created_ts.
func ParseCallMember(raw json.RawMessage, origin time.Time) (*CallMemberContent, bool, error) {
	var c CallMemberContent
	if len(raw) == 0 {
		return &c, false, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, false, err
	}
	return &c, c.IsActive(time.Now(), origin), nil
}

// IsActive reports whether the content describes a live membership.
//
// A membership past its expiry is treated as gone: a client that crashes never
// sends the empty-content leave event, and acting on a stale membership would
// place a call to a phone nobody is waiting on.
func (c *CallMemberContent) IsActive(now, origin time.Time) bool {
	if len(c.Memberships) > 0 {
		for _, m := range c.Memberships {
			if !expired(m.CreatedTS, m.ExpiresMS, now, origin) {
				return true
			}
		}
		return false
	}
	// The inline form is only a membership if it names a device; an empty
	// object is a leave.
	if c.DeviceID == "" && c.CallID == "" && c.Application == "" {
		return false
	}
	return !expired(c.CreatedTS, c.ExpiresMS, now, origin)
}

// expired reports whether a membership has lapsed.
//
// "expires" is a DURATION in milliseconds from when the membership was
// created, not a deadline. Reading it as a Unix timestamp turns every real
// client's four-hour join into a 1970 date, i.e. into a leave, and the bridge
// hangs up on the user in the act of answering. The base is created_ts when
// the client sends one and the event's own timestamp otherwise.
//
// A zero or negative duration means the client set no expiry, which is not the
// same as having expired.
func expired(createdTS, expiresMS int64, now, origin time.Time) bool {
	if expiresMS <= 0 {
		return false
	}
	base := origin
	if createdTS > 0 {
		base = time.UnixMilli(createdTS)
	}
	if base.IsZero() {
		return false
	}
	return base.Add(time.Duration(expiresMS) * time.Millisecond).Before(now)
}

// FirstDeviceID returns the device ID of the membership, from whichever of the
// two content shapes carries it.
func (c *CallMemberContent) FirstDeviceID() string {
	if c.DeviceID != "" {
		return c.DeviceID
	}
	for _, m := range c.Memberships {
		if m.DeviceID != "" {
			return m.DeviceID
		}
	}
	return ""
}

// FirstMembershipID returns the MatrixRTC member ID, which together with the
// user ID and device ID derives the LiveKit participant identity.
func (c *CallMemberContent) FirstMembershipID() string {
	if c.MembershipID != "" {
		return c.MembershipID
	}
	for _, m := range c.Memberships {
		if m.MembershipID != "" {
			return m.MembershipID
		}
	}
	return ""
}
