package siptransport

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// contentTypeText is the only body chan_sip's ast_msg_tech accepts. Anything
// else is answered 415 there, so the bridge refuses it symmetrically rather
// than bridging a body the far end cannot have sent.
const contentTypeText = "text/plain"

// handleMessage turns an inbound SIP MESSAGE into a call of the registered
// handler.
func (t *Transport) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	h := t.onMessage.Load()
	if h == nil {
		t.respond(req, tx, 405, "Method Not Allowed")
		return
	}
	if ct := req.ContentType(); ct == nil || !isTextPlain(string(*ct)) {
		t.respond(req, tx, 415, "Unsupported Media Type")
		return
	}
	msg := InboundMessage{Body: string(req.Body())}
	if h := req.From(); h != nil {
		msg.From = h.Address.String()
	}
	if h := req.To(); h != nil {
		msg.To = h.Address.String()
	}
	// The handler runs inline: a MESSAGE transaction is short, and answering
	// before the bridge has accepted the text would lose messages on restart.
	if err := (*h)(sip.ServerTransactionContext(tx), msg); err != nil {
		t.log.Warn().Err(err).Msg("Rejecting inbound SIP MESSAGE")
		t.respond(req, tx, 500, "Server Internal Error")
		return
	}
	t.respond(req, tx, 200, "OK")
}

// SendMessage sends a SIP MESSAGE.
//
// to and from are SIP URIs. The body must be text/plain; chan_sip answers
// anything else 415.
func (t *Transport) SendMessage(ctx context.Context, to, from, body string) error {
	var recipient sip.Uri
	if err := sip.ParseUri(to, &recipient); err != nil {
		return fmt.Errorf("parse destination %q: %w", to, err)
	}
	req := sip.NewRequest(sip.MESSAGE, recipient)
	if from != "" {
		var fromURI sip.Uri
		if err := sip.ParseUri(from, &fromURI); err != nil {
			return fmt.Errorf("parse sender %q: %w", from, err)
		}
		req.AppendHeader(&sip.FromHeader{Address: fromURI, Params: sip.NewParams()})
	}
	req.AppendHeader(sip.NewHeader("Content-Type", contentTypeText))
	req.AppendHeader(t.allow)
	req.SetBody([]byte(body))
	req.SetTransport(strings.ToUpper(t.cfg.Transport))
	if dest := t.outboundDestination(); dest != "" {
		req.SetDestination(dest)
	}

	// chan_sip drops anything over SIP_MAX_PACKET_SIZE. Over UDP an
	// oversized message is lost without a response at all, so the size is
	// checked on the whole serialised request rather than on the body.
	if n := len(req.String()); n > maxPacketSize {
		return fmt.Errorf("message is %d bytes, over the %d-byte SIP packet limit", n, maxPacketSize)
	}

	res, err := t.cli.Do(ctx, req)
	if err != nil {
		return fmt.Errorf("send MESSAGE: %w", err)
	}
	if !res.IsSuccess() {
		return fmt.Errorf("MESSAGE rejected: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// outboundDestination is the SIP server outbound requests are sent to. With no
// registration configured the request URI's own host is used.
func (t *Transport) outboundDestination() string {
	if t.cfg.Register.Server != "" {
		return t.cfg.Register.Server
	}
	return ""
}

func isTextPlain(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(base), contentTypeText)
}
