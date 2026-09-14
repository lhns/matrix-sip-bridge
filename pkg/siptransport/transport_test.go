package siptransport

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
)

// These tests run a real SIP conversation between the bridge's transport and a
// second sipgo user agent standing in for the SIP server, over a loopback TCP
// listener. Nothing here is mocked at the protocol level, because the failures
// this code has to avoid — a 200 OK with no SDP, an SDP that reads as hold, a
// MESSAGE that is silently not routed — are all wire-level.

const testTimeout = 10 * time.Second

// fakePeer is the SIP server the bridge talks to.
type fakePeer struct {
	ua   *sipgo.UserAgent
	srv  *sipgo.Server
	cli  *sipgo.Client
	dua  *sipgo.DialogUA
	addr string

	messages chan *sip.Request
}

func newFakePeer(t *testing.T, ctx context.Context) *fakePeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	host, port, _ := splitHostPort(addr)

	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname(host))
	if err != nil {
		t.Fatalf("peer ua: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("peer server: %v", err)
	}
	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname(host))
	if err != nil {
		t.Fatalf("peer client: %v", err)
	}
	p := &fakePeer{
		ua:   ua,
		srv:  srv,
		cli:  cli,
		addr: addr,
		dua: &sipgo.DialogUA{Client: cli, ContactHDR: sip.ContactHeader{
			Address: sip.Uri{Scheme: "sip", User: "pbx", Host: host, Port: port},
		}},
		messages: make(chan *sip.Request, 4),
	}
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		p.messages <- req
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	go func() { _ = srv.ServeTCP(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = cli.Close()
		_ = ua.Close()
		_ = ln.Close()
	})
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return p
}

// startBridge brings up a Transport on a loopback listener and returns it with
// its address.
func startBridge(t *testing.T, ctx context.Context, tune func(*Config)) (*Transport, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	cfg := Config{
		Listen:        addr,
		Transport:     "tcp",
		PublicAddress: addr,
		Username:      "matrix-sip-bridge",
		Domain:        "example.com",
		MediaAddress:  "127.0.0.1",
	}
	if tune != nil {
		tune(&cfg)
	}
	tr, err := New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	go func() { _ = tr.RunWithListener(ctx, ln) }()
	t.Cleanup(tr.Close)
	waitFor(t, func() bool { return tr.listening.Load() })
	return tr, addr
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// invite sends an INVITE from the peer to the bridge and returns the dialog
// plus every provisional response seen on the way to the final one.
func (p *fakePeer) invite(ctx context.Context, bridgeAddr string, headers map[string]string, body []byte, onProvisional func(*sip.Response)) (*sipgo.DialogClientSession, []*sip.Response, error) {
	host, port, _ := splitHostPort(bridgeAddr)
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "15551234567", Host: host, Port: port})
	req.SetBody(body)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}
	req.SetTransport("TCP")
	req.SetDestination(bridgeAddr)

	dlg, err := p.dua.WriteInvite(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var provisional []*sip.Response
	err = dlg.WaitAnswer(ctx, sipgo.AnswerOptions{OnResponse: func(res *sip.Response) error {
		if res.IsProvisional() && res.StatusCode > 100 {
			provisional = append(provisional, res)
			if onProvisional != nil {
				onProvisional(res)
			}
		}
		return nil
	}})
	return dlg, provisional, err
}

// A caller offering G.711 must get a 180 and then a 200 whose SDP is a real
// answer: chan_sip treats a bodyless 200 as a reason to tear the call down,
// and c=0.0.0.0 or a=inactive as hold.
func TestInboundCallRingsThenAnswersWithLiveSDP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The bridge must not answer until the Matrix side has joined, so the
	// handler waits for the test to release it. That is also what makes the
	// 180 observable: answering immediately can put both responses on the
	// wire before the caller starts reading them.
	joined := make(chan struct{})
	answered := make(chan *InboundCall, 1)
	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(ctx context.Context, call *InboundCall) {
		if err := call.Ringing(); err != nil {
			t.Errorf("Ringing: %v", err)
			return
		}
		// Published before answering: the answer does not complete until the
		// caller ACKs, and the caller only does that once the test has the
		// leg in hand.
		answered <- call
		<-joined
		if err := call.Answer(); err != nil {
			t.Errorf("Answer: %v", err)
			return
		}
		<-call.Done()
	})

	peer := newFakePeer(t, ctx)
	var ringingOnce sync.Once
	dlg, provisional, err := peer.invite(ctx, bridgeAddr,
		map[string]string{"X-Conference": "sip-15551234567"},
		offerSDP("127.0.0.1", 40000),
		func(res *sip.Response) {
			if res.StatusCode == 180 {
				ringingOnce.Do(func() { close(joined) })
			}
		})
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	defer func() { _ = dlg.Close() }()

	if len(provisional) == 0 || provisional[0].StatusCode != 180 {
		t.Fatalf("expected a 180 Ringing before the answer, got %v", provisional)
	}
	res := dlg.InviteResponse
	if res.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", res.StatusCode)
	}
	sdp := string(res.Body())
	if sdp == "" {
		t.Fatal("200 OK carried no SDP; chan_sip sets SIP_PENDINGBYE and hangs up")
	}
	for _, hold := range []string{"c=IN IP4 0.0.0.0", "a=inactive", "a=sendonly"} {
		if strings.Contains(sdp, hold) {
			t.Errorf("answer SDP contains %q, which chan_sip parses as hold:\n%s", hold, sdp)
		}
	}
	if !strings.Contains(sdp, "a=sendrecv") {
		t.Errorf("answer SDP is not sendrecv:\n%s", sdp)
	}

	call := <-answered
	if call.Conference() != "sip-15551234567" {
		t.Errorf("Conference() = %q, want the X-Conference value", call.Conference())
	}
	if !strings.Contains(call.From(), "@127.0.0.1") {
		t.Errorf("From() = %q, want the peer's URI", call.From())
	}

	// The dialplan hangs the control leg up right after the answer. That is
	// the normal end of the leg, and the bridge must observe it.
	if err := dlg.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	if err := dlg.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	select {
	case <-call.Done():
	case <-time.After(testTimeout):
		t.Fatal("leg did not report done after BYE")
	}
}

// Nobody joined on the Matrix side, so the bridge must decline its branch of
// the Dial() rather than answer it.
func TestInboundCallCanBeRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(ctx context.Context, call *InboundCall) {
		_ = call.Ringing()
		_ = call.Reject(480, "Temporarily Unavailable")
	})

	peer := newFakePeer(t, ctx)
	_, _, err := peer.invite(ctx, bridgeAddr, nil, offerSDP("127.0.0.1", 40000), nil)
	if got := responseStatus(err); got != 480 {
		t.Fatalf("err = %v (status %d), want a 480 response", err, got)
	}
}

// An offer with no G.711 is answered 488 rather than with an SDP naming a
// codec the bridge did not negotiate.
func TestInboundCallWithNoCommonCodecIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(ctx context.Context, call *InboundCall) {
		if err := call.Answer(); err == nil {
			t.Error("expected Answer to refuse an offer with no common codec")
		}
	})

	opusOnly := []byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\n" +
		"t=0 0\r\nm=audio 40000 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n")
	peer := newFakePeer(t, ctx)
	_, _, err := peer.invite(ctx, bridgeAddr, nil, opusOnly, nil)
	if got := responseStatus(err); got != 488 {
		t.Fatalf("err = %v (status %d), want a 488 response", err, got)
	}
}

func TestInboundMessageIsDeliveredAndAcknowledged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	got := make(chan InboundMessage, 1)
	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnMessage(func(ctx context.Context, msg InboundMessage) error {
		got <- msg
		return nil
	})

	peer := newFakePeer(t, ctx)
	res := peer.sendMessage(t, ctx, bridgeAddr, "text/plain", "hello")
	if res.StatusCode != 200 {
		t.Fatalf("MESSAGE response = %d %s, want 200", res.StatusCode, res.Reason)
	}
	if allow := res.GetHeader("Allow"); allow == nil || !strings.Contains(allow.Value(), "MESSAGE") {
		t.Error("response does not advertise MESSAGE in Allow; chan_sip will not route text to the bridge")
	}
	msg := <-got
	if msg.Body != "hello" {
		t.Errorf("Body = %q, want %q", msg.Body, "hello")
	}
	if msg.From != "sip:15551234567@pbx.example.com" {
		t.Errorf("From = %q, want the peer's URI without its tag", msg.From)
	}
}

// chan_sip's ast_msg_tech only handles text/plain, and answers anything else
// 415. The bridge does the same rather than bridging a body the far end could
// not have produced.
func TestInboundMessageRejectsNonTextBodies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnMessage(func(ctx context.Context, msg InboundMessage) error {
		t.Error("handler should not have been called for a non-text body")
		return nil
	})

	peer := newFakePeer(t, ctx)
	res := peer.sendMessage(t, ctx, bridgeAddr, "application/json", `{"body":"hi"}`)
	if res.StatusCode != 415 {
		t.Fatalf("response = %d, want 415", res.StatusCode)
	}
}

func TestOutboundMessageReachesThePeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	tr, _ := startBridge(t, ctx, nil)

	to := "sip:15551234567@" + peer.addr
	if err := tr.SendMessage(ctx, to, "sip:15559876543@example.com", "hi there"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	select {
	case req := <-peer.messages:
		if string(req.Body()) != "hi there" {
			t.Errorf("body = %q", req.Body())
		}
		if ct := req.ContentType(); ct == nil || string(*ct) != "text/plain" {
			t.Errorf("Content-Type = %v, want text/plain", ct)
		}
		if from := req.From(); from == nil || from.Address.User != "15559876543" {
			t.Errorf("From = %v", from)
		}
	case <-time.After(testTimeout):
		t.Fatal("peer never received the MESSAGE")
	}
}

// A body that would push the request over chan_sip's SIP_MAX_PACKET_SIZE is
// refused here rather than written to a socket that drops it without a reply.
func TestOutboundMessageRefusesOversizedBodies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	tr, _ := startBridge(t, ctx, nil)

	err := tr.SendMessage(ctx, "sip:15551234567@"+peer.addr, "sip:15559876543@example.com",
		strings.Repeat("x", maxPacketSize))
	if err == nil {
		t.Fatal("expected an oversized message to be refused")
	}
	if !strings.Contains(err.Error(), "packet limit") {
		t.Errorf("err = %v, want it to name the packet limit", err)
	}
}

func (p *fakePeer) sendMessage(t *testing.T, ctx context.Context, bridgeAddr, contentType, body string) *sip.Response {
	t.Helper()
	host, port, _ := splitHostPort(bridgeAddr)
	req := sip.NewRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", User: "matrix-sip-bridge", Host: host, Port: port})
	req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	req.AppendHeader(sip.NewHeader("From", "<sip:15551234567@pbx.example.com>;tag=peertag"))
	req.SetBody([]byte(body))
	req.SetTransport("TCP")
	req.SetDestination(bridgeAddr)
	res, err := p.cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("send MESSAGE: %v", err)
	}
	return res
}

// responseStatus digs the SIP status out of a failed INVITE.
func responseStatus(err error) int {
	var byValue sipgo.ErrDialogResponse
	if errors.As(err, &byValue) && byValue.Res != nil {
		return byValue.Res.StatusCode
	}
	var byPointer *sipgo.ErrDialogResponse
	if errors.As(err, &byPointer) && byPointer.Res != nil {
		return byPointer.Res.StatusCode
	}
	return 0
}
