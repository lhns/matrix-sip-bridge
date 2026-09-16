package siptransport

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo/sip"
	"maunium.net/go/mautrix/id"

	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// sanitiseCaller reduces an MXID to what is safe in a quoted SIP display name.
// An MXID cannot contain a quote, a backslash or a newline, so anything that
// does is not one and is dropped whole rather than escaped: a display name is
// a claim the far end may act on, and a half-repaired one is worse than none.
func sanitiseCaller(u id.UserID) string {
	s := string(u)
	if s == "" || len(s) > 255 {
		return ""
	}
	if strings.ContainsAny(s, "\"\\\r\n") {
		return ""
	}
	return s
}

// contentTypeText is the only body chan_sip's ast_msg_tech accepts. Anything
// else is answered 415 there, so the bridge refuses it symmetrically rather
// than bridging a body the far end cannot have sent.
const contentTypeText = "text/plain"

// handleMessage turns an inbound SIP MESSAGE into a call of the registered
// handler.
func (t *Transport) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	h := t.onMessage.Load()
	if h == nil {
		t.metrics.Message(metrics.DirectionInbound, metrics.MessageRejectedNoHandler)
		t.respond(req, tx, 405, "Method Not Allowed")
		return
	}
	if ct := req.ContentType(); ct == nil || !isTextPlain(string(*ct)) {
		t.metrics.Message(metrics.DirectionInbound, metrics.MessageRejectedMediaType)
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
	// The context is the transport's, bounded by the transaction timeout; see
	// Transport.baseCtx for why it is not the transaction's own.
	ctx, cancel := context.WithTimeout(t.baseCtx, messageHandlerTimeout)
	defer cancel()
	// Only the two refusals above are counted here. Everything past this line
	// is the handler's own outcome -- including the messages it drops while
	// still asking for a 200 -- and it counts them itself, so that one message
	// never lands in two buckets.
	if err := (*h)(ctx, msg); err != nil {
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
//
// caller is the Matrix user who sent it, carried in the From DISPLAY NAME
// rather than a header: the SIP server matches this peer on the From user part,
// so that half is not ours to use, while the display name is free and every
// dialplan already strips it. It also rides an out-of-call MESSAGE, which a
// custom header is not guaranteed to on every SIP stack.
func (t *Transport) SendMessage(ctx context.Context, to, from, body string, caller id.UserID) error {
	var recipient sip.Uri
	if err := sip.ParseUri(to, &recipient); err != nil {
		return fmt.Errorf("parse destination %q: %w", to, err)
	}
	req := sip.NewRequest(sip.MESSAGE, recipient)
	var fromHeader *sip.FromHeader
	if from != "" {
		var fromURI sip.Uri
		if err := sip.ParseUri(from, &fromURI); err != nil {
			return fmt.Errorf("parse sender %q: %w", from, err)
		}
		fromHeader = &sip.FromHeader{Address: fromURI, Params: sip.NewParams()}
	} else {
		fromHeader = t.fromHeader()
	}
	// An MXID cannot contain a quote or a backslash, and sipgo does not escape
	// either, so a sanitised one cannot break out of the quoted display name.
	if c := sanitiseCaller(caller); c != "" {
		fromHeader.DisplayName = c
	}
	req.AppendHeader(fromHeader)
	req.AppendHeader(sip.NewHeader("Content-Type", contentTypeText))
	req.AppendHeader(t.allow)
	req.SetBody([]byte(body))
	req.SetTransport(strings.ToUpper(t.cfg.Transport))
	if dest := t.outboundDestination(); dest != "" {
		req.SetDestination(dest)
	}

	// Checked on the whole serialised request rather than on the body,
	// because it is the packet that is capped; see maxPacketSize.
	if n := len(req.String()); n > maxPacketSize {
		t.metrics.Message(metrics.DirectionOutbound, metrics.MessageTooLong)
		return fmt.Errorf("message is %d bytes, over the %d-byte SIP packet limit", n, maxPacketSize)
	}

	res, err := t.doWithDigest(ctx, req)
	if err != nil {
		t.metrics.Message(metrics.DirectionOutbound, metrics.MessageError)
		return fmt.Errorf("send MESSAGE: %w", err)
	}
	if !res.IsSuccess() {
		// A trunk that carries calls but refuses text answers every MESSAGE
		// the same way forever. The status code stays out of the label and
		// goes in the returned error, which is what the caller logs.
		t.metrics.Message(metrics.DirectionOutbound, metrics.MessageRejected)
		return fmt.Errorf("MESSAGE rejected: %d %s", res.StatusCode, res.Reason)
	}
	t.metrics.Message(metrics.DirectionOutbound, metrics.MessageSent)
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
