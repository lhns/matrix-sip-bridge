package siptransport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// InboundCall is the control leg of a call the SIP server routed to the
// bridge.
//
// It is not the call. The far end never talks to this process: the dialplan
// dials the bridge as one leg of a parallel Dial(), and the gosub that runs
// after the bridge answers sets GOSUB_RESULT=CONTINUE, which hangs this leg up
// and lets the caller fall through into the conference. The leg lives a few
// hundred milliseconds and carries no useful RTP. Its end is therefore not a
// signal that the call ended.
type InboundCall struct {
	t   *Transport
	dlg *sipgo.DialogServerSession

	from       string
	to         string
	conference string
	offer      []byte

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// From is the caller's URI as the SIP server presented it.
func (c *InboundCall) From() string { return c.from }

// To is the URI the call was placed to.
func (c *InboundCall) To() string { return c.to }

// Conference is the value of the configured conference header, naming the
// conference the caller will land in.
func (c *InboundCall) Conference() string { return c.conference }

// Header returns an arbitrary header from the INVITE.
func (c *InboundCall) Header(name string) string {
	h := c.dlg.InviteRequest.GetHeader(name)
	if h == nil {
		return ""
	}
	return h.Value()
}

// Done is closed when the leg is gone, by BYE, CANCEL or transaction failure.
func (c *InboundCall) Done() <-chan struct{} { return c.done }

// Ringing sends 180. It tells the SIP server the bridge is a live branch of
// the Dial() without committing to answering.
func (c *InboundCall) Ringing() error {
	return c.dlg.Respond(180, "Ringing", nil)
}

// Answer sends 200 OK with a real SDP answer.
//
// Answering is the commitment: Dial() hangs up every other branch with
// ANSWERED_ELSEWHERE the instant one answers, so this must not be called
// speculatively. The SDP must be a genuine answer — a 200 with no body sets
// SIP_PENDINGBYE in chan_sip and tears the call down, and c=0.0.0.0 or
// a=inactive is read as hold.
func (c *InboundCall) Answer() error {
	if len(c.offer) == 0 {
		_ = c.Reject(488, "Not Acceptable Here")
		return errors.New("INVITE carried no SDP offer")
	}
	answer, err := answerSDP(c.offer, c.t.mediaHost, c.t.cfg.MediaPort)
	if err != nil {
		_ = c.Reject(488, "Not Acceptable Here")
		return err
	}
	return c.dlg.RespondSDP(answer)
}

// Reject answers with a failure status and ends the leg.
func (c *InboundCall) Reject(code int, reason string) error {
	err := c.dlg.Respond(code, reason, nil)
	c.finish()
	return err
}

// finish releases the leg's resources exactly once.
func (c *InboundCall) finish() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	_ = c.dlg.Close()
	close(c.done)
}

// handleInvite builds an InboundCall and hands it to the registered handler.
func (t *Transport) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	h := t.onInvite.Load()
	if h == nil {
		t.respond(req, tx, 503, "Service Unavailable")
		return
	}
	dlg, err := t.dlg.ReadInvite(req, tx)
	if err != nil {
		t.log.Warn().Err(err).Msg("Failed to read INVITE")
		t.respond(req, tx, 400, "Bad Request")
		return
	}
	call := &InboundCall{
		t:          t,
		dlg:        dlg,
		conference: strings.TrimSpace(headerValue(req, t.cfg.ConferenceHeader)),
		offer:      req.Body(),
		done:       make(chan struct{}),
	}
	if f := req.From(); f != nil {
		call.from = f.Address.String()
	}
	if to := req.To(); to != nil {
		call.to = to.Address.String()
	}
	// OnStateReplay, not OnState: ReadInvite already wired CANCEL and
	// transaction termination to the dialog state, and either can have fired
	// before this line.
	dlg.OnStateReplay(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			call.finish()
		}
	})

	// The handler runs inline. sipgo dispatches every request on its own
	// goroutine and calls TerminateGracefully on the server transaction the
	// moment the handler returns, so a handler that returns early would kill
	// the leg it is supposed to be holding open.
	defer call.finish()
	(*h)(sip.ServerTransactionContext(tx), call)
}

func (t *Transport) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	if err := t.dlg.ReadAck(req, tx); err != nil {
		t.log.Debug().Err(err).Msg("Unmatched ACK")
	}
}

func (t *Transport) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	if err := t.dlg.ReadBye(req, tx); err != nil {
		t.log.Debug().Err(err).Msg("Unmatched BYE")
		t.respond(req, tx, 481, "Call/Transaction Does Not Exist")
	}
}

func headerValue(req *sip.Request, name string) string {
	if name == "" {
		return ""
	}
	h := req.GetHeader(name)
	if h == nil {
		return ""
	}
	return h.Value()
}

// OutboundCall is the control leg of a call the bridge asked the SIP server to
// place. Like the inbound one it carries no media: the dialplan reads the
// conference header, dials the number into that conference and hangs this leg
// up.
type OutboundCall struct {
	dlg  *sipgo.DialogClientSession
	done chan struct{}
	once sync.Once
}

// Done is closed when the control leg ends.
func (c *OutboundCall) Done() <-chan struct{} { return c.done }

// Hangup ends the control leg. It does not end the call: the conference and
// the LiveKit participant outlive it.
func (c *OutboundCall) Hangup(ctx context.Context) error {
	err := c.dlg.Bye(ctx)
	c.finish()
	return err
}

func (c *OutboundCall) finish() {
	c.once.Do(func() {
		_ = c.dlg.Close()
		close(c.done)
	})
}

// Invite places an outbound control leg, carrying the given headers.
//
// It returns once the leg has been answered, which is the SIP server
// confirming it accepted the request, not the far end picking up.
func (t *Transport) Invite(ctx context.Context, to string, headers map[string]string) (*OutboundCall, error) {
	var recipient sip.Uri
	if err := sip.ParseUri(to, &recipient); err != nil {
		return nil, fmt.Errorf("parse destination %q: %w", to, err)
	}
	req := sip.NewRequest(sip.INVITE, recipient)
	req.SetBody(offerSDP(t.mediaHost, t.cfg.MediaPort))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(t.allow)
	for name, value := range headers {
		req.AppendHeader(sip.NewHeader(name, value))
	}
	req.SetTransport(strings.ToUpper(t.cfg.Transport))
	if dest := t.outboundDestination(); dest != "" {
		req.SetDestination(dest)
	}
	dlg, err := t.dua.WriteInvite(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send INVITE: %w", err)
	}
	call := &OutboundCall{dlg: dlg, done: make(chan struct{})}
	if err := dlg.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		call.finish()
		return nil, fmt.Errorf("INVITE not answered: %w", err)
	}
	if err := dlg.Ack(ctx); err != nil {
		call.finish()
		return nil, fmt.Errorf("send ACK: %w", err)
	}
	dlg.OnStateReplay(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			call.finish()
		}
	})
	return call, nil
}
