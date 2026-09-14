package calls

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestRingNotificationContent(t *testing.T) {
	now := time.UnixMilli(1700000000000)
	content := ringNotification(now, 45*time.Second, "$membership", []id.UserID{"@alice:example.com"})

	raw, err := json.Marshal(content.Raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		NotificationType string `json:"notification_type"`
		SenderTS         int64  `json:"sender_ts"`
		Lifetime         int64  `json:"lifetime"`
		Intent           string `json:"m.call.intent"`
		Mentions         struct {
			UserIDs []string `json:"user_ids"`
		} `json:"m.mentions"`
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.NotificationType != NotificationRing {
		t.Errorf("notification_type = %q, want %q", got.NotificationType, NotificationRing)
	}
	if got.SenderTS != now.UnixMilli() {
		t.Errorf("sender_ts = %d, want %d", got.SenderTS, now.UnixMilli())
	}
	if got.Lifetime != 45000 {
		t.Errorf("lifetime = %d, want 45000", got.Lifetime)
	}
	if got.Intent != "audio" {
		t.Errorf("m.call.intent = %q, want audio", got.Intent)
	}
	if len(got.Mentions.UserIDs) != 1 || got.Mentions.UserIDs[0] != "@alice:example.com" {
		t.Errorf("m.mentions.user_ids = %v", got.Mentions.UserIDs)
	}
	if got.RelatesTo.RelType != "m.reference" || got.RelatesTo.EventID != "$membership" {
		t.Errorf("m.relates_to = %+v", got.RelatesTo)
	}
}

// A client that receives user_ids: null has nobody to notify, and an absent
// mentions object gets no push rule match at all.
func TestRingNotificationAlwaysCarriesAMentionsArray(t *testing.T) {
	content := ringNotification(time.Now(), time.Minute, "", nil)
	raw, err := json.Marshal(content.Raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"m.mentions":{"user_ids":[]}`; !strings.Contains(string(raw), want) {
		t.Errorf("content %s does not contain %s", raw, want)
	}
	if strings.Contains(string(raw), "m.relates_to") {
		t.Errorf("content %s should omit m.relates_to when there is no membership event", raw)
	}
}

func TestDeclineTarget(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want id.EventID
		ok   bool
	}{
		{
			name: "reference relation",
			raw:  `{"m.relates_to":{"rel_type":"m.reference","event_id":"$notify"}}`,
			want: "$notify",
			ok:   true,
		},
		{"wrong relation type", `{"m.relates_to":{"rel_type":"m.replace","event_id":"$notify"}}`, "", false},
		{"no event id", `{"m.relates_to":{"rel_type":"m.reference"}}`, "", false},
		{"no relation", `{}`, "", false},
		{"empty content", ``, "", false},
		{"garbage", `not json`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := declineTarget(json.RawMessage(tt.raw))
			if ok != tt.ok {
				t.Fatalf("declineTarget(%s) ok = %v, want %v", tt.raw, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("declineTarget(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// The power levels bridgev2 gives a fresh portal: state_default 50, the bot
// far above it, the ghost at 50, and the owning Matrix user nowhere in the
// users map. Without the override that user cannot send the RTC membership,
// which is what Element uses to decide whether to offer a call at all.
func TestMembershipPowerLevelsLetAPlainUserStartACall(t *testing.T) {
	const user id.UserID = "@alice:example.com"
	pl := &event.PowerLevelsEventContent{
		Events: map[string]int{
			event.StateTombstone.Type:  100,
			event.StateServerACL.Type:  100,
			event.StateEncryption.Type: 100,
		},
		Users:           map[id.UserID]int{"@sipbot:example.com": 9001},
		StateDefaultPtr: intPtr(50),
	}
	for evtType := range MembershipPowerLevels() {
		if pl.GetUserLevel(user) >= pl.GetEventLevel(evtType) {
			t.Fatalf("test setup is wrong: %s is already sendable", evtType.Type)
		}
	}

	overrides := &bridgev2.PowerLevelOverrides{Events: MembershipPowerLevels()}
	if !overrides.Apply("@sipbot:example.com", pl) {
		t.Fatal("applying the override changed nothing")
	}
	for evtType := range MembershipPowerLevels() {
		if pl.GetUserLevel(user) < pl.GetEventLevel(evtType) {
			t.Errorf("%s still needs power level %d; the call button stays hidden",
				evtType.Type, pl.GetEventLevel(evtType))
		}
	}
	// Lowering one event must not lower the room.
	if pl.StateDefault() != 50 {
		t.Errorf("state_default = %d, want 50", pl.StateDefault())
	}
	if pl.GetEventLevel(event.StateEncryption) != 100 {
		t.Error("the override must not make the room encryptable")
	}
}

func intPtr(i int) *int { return &i }

// A notification that outlives the ring leaves a phantom incoming call on
// every device, because nothing retracts it: the lifetime is the only thing
// that stops the ring, so it has to be the bridge's own ring timeout.
func TestRingNotificationLifetimeIsTheRingTimeout(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "pkg", "calls", "membership.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || fun.Name != "ringNotification" || len(call.Args) < 2 {
			return true
		}
		found = true
		if rendered := render(call.Args[1]); rendered != "s.cfg.RingTimeout" {
			t.Errorf("ringNotification lifetime is %s, want s.cfg.RingTimeout", rendered)
		}
		return true
	})
	if !found {
		t.Error("membership.go no longer builds a ring notification")
	}
}

// render prints a selector expression such as s.cfg.RingTimeout.
func render(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return render(e.X) + "." + e.Sel.Name
	default:
		return "?"
	}
}
