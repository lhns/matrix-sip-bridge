package calls

import (
	"encoding/json"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// RtcNotificationEventType is the MSC4075 event that makes a client ring.
//
// Publishing the RTC membership only makes a call joinable; nothing rings
// until this event arrives. mautrix-go has no MSC4075 support, so the content
// is built as raw JSON and the type is registered with the EventProcessor by
// hand, exactly as CallMemberEventType is.
var RtcNotificationEventType = event.Type{
	Type:  "m.rtc.notification",
	Class: event.MessageEventType,
}

// RtcDeclineEventType is the MSC4310 event a client sends when the user
// rejects the call, or when one of their other devices already did.
//
// Only the unstable name exists: the event is still behind the MSC prefix in
// the clients that send it, so a bridge listening for "m.rtc.decline" alone
// would never hear a rejection and would ring until the timeout.
var RtcDeclineEventType = event.Type{
	Type:  "org.matrix.msc4310.rtc.decline",
	Class: event.MessageEventType,
}

// RtcDeclineStableEventType is the name the decline event will take once
// MSC4310 lands. Both are subscribed to, because which one a client sends is
// not something the bridge can see.
var RtcDeclineStableEventType = event.Type{
	Type:  "m.rtc.decline",
	Class: event.MessageEventType,
}

// NotificationRing asks for an audible ring rather than a silent notification.
const NotificationRing = "ring"

// ringNotification builds the content of the event that makes a client ring.
//
// lifetime is the backstop that ends the ring: MSC4075 has no cancellation
// event, so a call that ends early is retracted by redacting this one, and a
// notification that outlived the bridge's own ring timeout would leave a
// phantom incoming call on every device that missed the redaction. It must
// therefore be the ring timeout, not a round number.
//
// The mentions are not cosmetic either. No homeserver ships a push rule for
// this event type, so the only thing that turns it into a push — and a ring on
// a phone that is not in the foreground — is .m.rule.is_user_mention matching
// m.mentions.user_ids.
func ringNotification(now time.Time, lifetime time.Duration, membership id.EventID, mention []id.UserID) *event.Content {
	raw := map[string]any{
		"notification_type": NotificationRing,
		"sender_ts":         now.UnixMilli(),
		"lifetime":          lifetime.Milliseconds(),
		"m.call.intent":     "audio",
		"m.mentions":        map[string]any{"user_ids": mentionList(mention)},
	}
	if membership != "" {
		raw["m.relates_to"] = map[string]any{
			"rel_type": "m.reference",
			"event_id": membership.String(),
		}
	}
	return &event.Content{Raw: raw}
}

// mentionList renders the user IDs as a JSON array that is never null, which
// is what the receiving parser expects of m.mentions.user_ids.
func mentionList(users []id.UserID) []string {
	out := make([]string, 0, len(users))
	for _, user := range users {
		out = append(out, user.String())
	}
	return out
}

// declineTarget returns the notification event a decline refers to.
//
// The relation is the whole content of the event; a decline that names no
// notification is not a rejection of anything the bridge sent and is ignored
// rather than guessed at.
func declineTarget(raw json.RawMessage) (id.EventID, bool) {
	var content struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if len(raw) == 0 {
		return "", false
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return "", false
	}
	if content.RelatesTo.RelType != "m.reference" || content.RelatesTo.EventID == "" {
		return "", false
	}
	return id.EventID(content.RelatesTo.EventID), true
}
