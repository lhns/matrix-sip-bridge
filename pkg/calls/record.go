package calls

import (
	"context"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

// callEnd is why a call ended, beyond what the row itself says.
//
// The row distinguishes answered from unanswered and nothing else, so a
// decline and a failure -- which are the two outcomes a caller most needs
// explained -- have to be carried here from the path that knew.
type callEnd struct {
	// Declined means a Matrix user rejected the call rather than ignoring it.
	Declined bool
	// Failure is the short reason the bridge could not carry the call, e.g.
	// "no route". Empty means the call did not fail.
	Failure string
	// Quiet suppresses the record entirely. It is for the bookkeeping paths
	// that end a row whose call is long over -- a stale row discarded hours
	// later must not announce itself as a missed call.
	Quiet bool
}

// mediaFailureReason is the short half of "Call failed — ...".
//
// Only the missing trunk is worth naming: it is a deployment fault the reader
// can act on, where everything else is LiveKit or the SIP server declining to
// take this particular call.
func mediaFailureReason(err error) string {
	if errors.Is(err, ErrNoTrunk) {
		return "no route"
	}
	return "could not connect"
}

// callRecordBody renders the timeline record of a finished call.
//
// It deliberately never names the number: the record is posted in that
// number's own portal, where repeating it is noise, and keeping it out is also
// what keeps the rendered strings free of personal data.
func callRecordBody(call *database.Call, end callEnd, now time.Time) string {
	if end.Failure != "" {
		return "Call failed — " + end.Failure
	}
	answered := call.State == database.StateBridged
	switch {
	case answered && call.Direction == database.DirectionOutbound:
		return "Outgoing call — " + formatCallDuration(now.Sub(call.UpdatedAt))
	case answered:
		return "Incoming call — " + formatCallDuration(now.Sub(call.UpdatedAt))
	case end.Declined:
		return "Call declined"
	case call.Direction == database.DirectionOutbound:
		return "Outgoing call — no answer"
	default:
		return "Missed call"
	}
}

// formatCallDuration renders a call length the way a phone does. A call that
// happened at all is never reported as zero seconds, but the rounding is to
// the nearest second rather than upwards: the record is written moments after
// the call ended, and rounding up turned every duration into one second more
// than the clock said.
func formatCallDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Round(time.Second) / time.Second)
	if total == 0 && d > 0 {
		total = 1
	}
	h, m, sec := total/3600, (total/60)%60, total%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// postCallRecord puts the finished call in the timeline.
//
// Nothing about a call reached the room before this: a missed call left no
// trace at all. The message is m.text rather than m.notice on purpose --
// .m.rule.suppress_notices would drop the push and the unread badge, which is
// the whole point of the record.
//
// It is sent as the ghost so the room shows the caller as having called,
// and failures to send it are only logged: a call that has already ended must
// not be kept alive by its epitaph.
func (s *Subsystem) postCallRecord(ctx context.Context, call *database.Call, end callEnd) {
	if end.Quiet || !s.cfg.Notices.callEnded() {
		return
	}
	if end.Failure != "" && !s.cfg.Notices.callFailed() {
		return
	}
	if call.RoomID == "" {
		return
	}
	intent, err := s.mx.GhostIntent(ctx, call.PortalID)
	if err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Could not record the call in the room")
		return
	}
	content := &event.Content{Parsed: &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    callRecordBody(call, end, time.Now()),
	}}
	if _, err := intent.SendMessage(ctx, call.RoomID, event.EventMessage, content, nil); err != nil {
		s.log.Warn().Err(err).Str("call_id", call.CallID).
			Msg("Could not record the call in the room")
	}
}
