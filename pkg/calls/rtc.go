package calls

import (
	"encoding/json"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
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

// CallMemberContent is the subset of the membership content the bridge reads.
//
// The event has had two shapes: the original MSC3401 one with a top-level
// "memberships" array, and the current per-device one with the fields inline
// and the device ID in the state key. Both are accepted, because Element X and
// Element Web have not always agreed on which they send.
type CallMemberContent struct {
	// Inline (current) form.
	Application    string `json:"application,omitempty"`
	CallID         string `json:"call_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	Scope          string `json:"scope,omitempty"`
	ExpiresTS      int64  `json:"expires,omitempty"`
	CreatedTS      int64  `json:"created_ts,omitempty"`
	FocusActive    any    `json:"focus_active,omitempty"`
	MembershipID   string `json:"membershipID,omitempty"`
	LiveKitAliasV1 string `json:"livekit_alias,omitempty"`

	// Legacy (array) form.
	Memberships []CallMembership `json:"memberships,omitempty"`
}

// CallMembership is one entry of the legacy memberships array.
type CallMembership struct {
	Application  string `json:"application,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExpiresTS    int64  `json:"expires,omitempty"`
	MembershipID string `json:"membershipID,omitempty"`
}

// ParseCallMember decodes a call.member state event.
//
// An empty content object is how a client leaves the session: Matrix has no
// state deletion, so the redaction-equivalent is state with no fields. The
// second return value reports whether the event describes an active
// membership, which is the signal the bridge acts on.
func ParseCallMember(raw json.RawMessage) (*CallMemberContent, bool, error) {
	var c CallMemberContent
	if len(raw) == 0 {
		return &c, false, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, false, err
	}
	return &c, c.IsActive(time.Now()), nil
}

// IsActive reports whether the content describes a live membership.
//
// A membership that has passed its "expires" timestamp is treated as gone: a
// client that crashes never sends the empty-content leave event, and acting on
// a stale membership would place a call to a phone nobody is waiting on.
func (c *CallMemberContent) IsActive(now time.Time) bool {
	if len(c.Memberships) > 0 {
		for _, m := range c.Memberships {
			if !expired(m.ExpiresTS, now) {
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
	return !expired(c.ExpiresTS, now)
}

// expired reports whether a millisecond timestamp is in the past. A zero or
// negative value means the client did not set an expiry, which is not the same
// as having expired.
func expired(expiresMS int64, now time.Time) bool {
	if expiresMS <= 0 {
		return false
	}
	return time.UnixMilli(expiresMS).Before(now)
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

// ghostMembership builds the state event content the bridge publishes on behalf
// of a phone-number ghost, so that Matrix clients render the caller as a
// participant of the RTC session rather than an anonymous stream.
func ghostMembership(deviceID, membershipID string, expiry time.Duration) *event.Content {
	now := time.Now()
	return &event.Content{Raw: map[string]any{
		"application":  "m.call",
		"call_id":      "",
		"scope":        "m.room",
		"device_id":    deviceID,
		"membershipID": membershipID,
		"created_ts":   now.UnixMilli(),
		"expires":      now.Add(expiry).UnixMilli(),
		"foci_preferred": []any{
			map[string]any{"type": "livekit"},
		},
	}}
}

// leaveMembership is the empty content that ends a membership.
func leaveMembership() *event.Content {
	return &event.Content{Raw: map[string]any{}}
}

// rtcStateKey is the state key of a call.member event: "user_id_device_id" in
// the current form, or the bare user ID in the legacy one. The bridge writes
// the current form.
func rtcStateKey(userID id.UserID, deviceID string) string {
	if deviceID == "" {
		return userID.String()
	}
	return userID.String() + "_" + deviceID
}
