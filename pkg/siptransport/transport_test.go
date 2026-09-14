package siptransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/rs/zerolog"
)

// The peer's credentials. A static Asterisk peer whose source address the
// server cannot match is matched on the From user part instead, and that path
// challenges every request -- INVITE and MESSAGE included, not just REGISTER.
const (
	testSIPPassword = "peer-password"
	testSIPRealm    = "asterisk"
	testSIPNonce    = "0123456789abcdef"
)

func testChallenge() string {
	return (&digest.Challenge{
		Realm:     testSIPRealm,
		Nonce:     testSIPNonce,
		Algorithm: "MD5",
		QOP:       []string{"auth"},
	}).String()
}

// verifyDigest recomputes the response the peer expects. The client's own
// cnonce and nonce count are echoed back, so the comparison is exact rather
// than a check that some Authorization header was present.
func verifyDigest(req *sip.Request) error {
	h := req.GetHeader("Authorization")
	if h == nil {
		return errors.New("no Authorization header")
	}
	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		return err
	}
	chal := &digest.Challenge{Realm: cred.Realm, Nonce: cred.Nonce, Algorithm: cred.Algorithm}
	if cred.QOP != "" {
		chal.QOP = []string{cred.QOP}
	}
	want, err := digest.Digest(chal, digest.Options{
		Method:   string(req.Method),
		URI:      cred.URI,
		Username: cred.Username,
		Password: testSIPPassword,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
	})
	if err != nil {
		return err
	}
	if want.Response != cred.Response {
		return fmt.Errorf("digest response mismatch for %s", req.Method)
	}
	return nil
}

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
	dlg  *sipgo.DialogServerCache
	addr string

	messages chan *sip.Request
	invites  chan *sip.Request

	// requireAuth makes the peer challenge every INVITE and MESSAGE once,
	// then verify the credentials on the retry.
	requireAuth atomic.Bool
	// authFailure records a retry whose digest did not verify, so a test
	// fails on a wrong password rather than on a timeout.
	authFailure atomic.Pointer[string]
}

// challenged answers an unauthenticated request with 401 and reports that it
// did. A retry carrying credentials is verified and allowed through.
func (p *fakePeer) challenged(req *sip.Request, tx sip.ServerTransaction) bool {
	if !p.requireAuth.Load() {
		return false
	}
	if req.GetHeader("Authorization") == nil {
		res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("WWW-Authenticate", testChallenge()))
		_ = tx.Respond(res)
		return true
	}
	if err := verifyDigest(req); err != nil {
		msg := err.Error()
		p.authFailure.Store(&msg)
		res := sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
		_ = tx.Respond(res)
		return true
	}
	return false
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
		invites:  make(chan *sip.Request, 4),
	}
	p.dlg = sipgo.NewDialogServerCache(cli, sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "pbx", Host: host, Port: port},
	})
	srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		if p.challenged(req, tx) {
			return
		}
		p.messages <- req
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		if p.challenged(req, tx) {
			return
		}
		dlg, err := p.dlg.ReadInvite(req, tx)
		if err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
			return
		}
		answer, err := answerSDP(req.Body(), host, 41000)
		if err != nil {
			_ = dlg.Respond(488, "Not Acceptable Here", nil)
			return
		}
		p.invites <- req
		_ = dlg.RespondSDP(answer)
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) { _ = p.dlg.ReadAck(req, tx) })
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) { _ = p.dlg.ReadBye(req, tx) })
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

// The INVITE handler owns the call setup — a database lookup, the LiveKit
// participant, the RTC membership — and all of it runs on the context this
// package hands it. sipgo's sip.ServerTransactionContext returns a context
// that is already cancelled, which killed every inbound call at its first
// database query, so the context is checked at each step of a call that goes
// all the way through to an answer.
func TestInboundCallSetupRunsOnALiveContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	type step struct {
		name string
		err  error
	}
	steps := make(chan step, 8)
	joined := make(chan struct{})
	setupDone := make(chan struct{})

	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(hctx context.Context, call *InboundCall) {
		steps <- step{"handler entry", hctx.Err()}
		// Stands in for the work beginInboundCall does before any SIP
		// response goes out: the sip_call lookup and insert, the portal room,
		// the ghost's RTC membership. It is the first thing the handler does,
		// and it is where the real call died.
		select {
		case <-hctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
		steps <- step{"after call setup", hctx.Err()}
		close(setupDone)

		if err := call.Ringing(); err != nil {
			t.Errorf("Ringing: %v", err)
			return
		}
		steps <- step{"after 180", hctx.Err()}

		<-joined
		if err := call.Answer(); err != nil {
			t.Errorf("Answer: %v", err)
			return
		}
		steps <- step{"after 200", hctx.Err()}
		close(steps)
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

	select {
	case <-setupDone:
	case <-time.After(testTimeout):
		t.Fatal("call setup never finished")
	}
	if len(provisional) == 0 || provisional[0].StatusCode != 180 {
		t.Fatalf("expected a 180 Ringing, got %v", provisional)
	}
	if dlg.InviteResponse.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", dlg.InviteResponse.StatusCode)
	}
	// The answer is not complete until the caller ACKs it, so this has to
	// happen before the handler's last step is readable.
	if err := dlg.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	for s := range steps {
		if s.err != nil {
			t.Errorf("context was %v at %q; call setup cannot use it", s.err, s.name)
		}
	}
	// The dialplan hangs the control leg up right after the answer; the
	// handler returning is what terminates the INVITE transaction.
	if err := dlg.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
}

// A call the bridge cannot set up has to be refused with a definitive status.
// Asterisk's parallel Dial() keeps ringing the other branches; a leg left
// hanging would hold the caller on a bridge that is not there.
func TestInboundCallSetupFailureIsRejectedDefinitively(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	legDone := make(chan struct{})
	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(hctx context.Context, call *InboundCall) {
		// Stands in for beginInboundCall failing.
		if err := call.Reject(500, "Server Internal Error"); err != nil {
			t.Errorf("Reject: %v", err)
		}
		select {
		case <-call.Done():
			close(legDone)
		case <-time.After(testTimeout):
			t.Error("leg was not closed by the rejection")
		}
	})

	peer := newFakePeer(t, ctx)
	_, _, err := peer.invite(ctx, bridgeAddr,
		map[string]string{"X-Conference": "sip-15551234567"},
		offerSDP("127.0.0.1", 40000), nil)
	if got := responseStatus(err); got != 500 {
		t.Fatalf("err = %v (status %d), want a 500 response", err, got)
	}
	<-legDone
}

// The MESSAGE handler writes to the database and queues a Matrix event on the
// context it is given, so it has the same requirement as the INVITE handler.
func TestInboundMessageHandlerRunsOnALiveContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	handlerErr := make(chan error, 1)
	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnMessage(func(hctx context.Context, msg InboundMessage) error {
		handlerErr <- hctx.Err()
		return nil
	})

	peer := newFakePeer(t, ctx)
	if res := peer.sendMessage(t, ctx, bridgeAddr, "text/plain", "hello"); res.StatusCode != 200 {
		t.Fatalf("MESSAGE response = %d, want 200", res.StatusCode)
	}
	if err := <-handlerErr; err != nil {
		t.Errorf("context was %v in the MESSAGE handler", err)
	}
}

// The dialplan's gosub hangs the leg up the moment it has the answer, and
// sipgo dispatches every inbound request on its own goroutine: the BYE can be
// handled before the ACK that was sent before it. sipgo reports that as a
// failed answer, and treating it as one would retract the Matrix side of a
// call that was in fact bridged.
func TestInboundCallAnswerSurvivesAByeBeforeTheAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	joined := make(chan struct{})
	answerErr := make(chan error, 1)
	tr, bridgeAddr := startBridge(t, ctx, nil)
	tr.OnInvite(func(hctx context.Context, call *InboundCall) {
		if err := call.Ringing(); err != nil {
			t.Errorf("Ringing: %v", err)
			return
		}
		<-joined
		answerErr <- call.Answer()
		<-call.Done()
	})

	peer := newFakePeer(t, ctx)
	var ringingOnce sync.Once
	dlg, _, err := peer.invite(ctx, bridgeAddr, nil, offerSDP("127.0.0.1", 40000),
		func(res *sip.Response) {
			if res.StatusCode == 180 {
				ringingOnce.Do(func() { close(joined) })
			}
		})
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	defer func() { _ = dlg.Close() }()
	if dlg.InviteResponse.StatusCode != 200 {
		t.Fatalf("final response = %d, want 200", dlg.InviteResponse.StatusCode)
	}

	// Sent through the dialog's transaction layer rather than dlg.Bye, which
	// refuses to run before the ACK. Holding the ACK back until the BYE has
	// been answered is what makes the ordering deterministic here.
	bye := sip.NewRequest(sip.BYE, dlg.InviteResponse.Contact().Address)
	bye.SetTransport("TCP")
	bye.SetDestination(bridgeAddr)
	byeTx, err := dlg.TransactionRequest(ctx, bye)
	if err != nil {
		t.Fatalf("BYE: %v", err)
	}
	defer byeTx.Terminate()
	select {
	case res := <-byeTx.Responses():
		if res.StatusCode != 200 {
			t.Fatalf("BYE response = %d, want 200", res.StatusCode)
		}
	case <-time.After(testTimeout):
		t.Fatal("the bridge never answered the BYE")
	}

	select {
	case err := <-answerErr:
		if err != nil {
			t.Fatalf("Answer: %v; the call was answered and then hung up, which is the normal end of the leg", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Answer never returned after the leg ended")
	}
	if err := dlg.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// An outbound INVITE that is challenged must be retried with the bridge's
// credentials. Asterisk challenges a static peer it could not match on its
// source address, so without this an outbound call is a 401 and nothing else.
func TestOutboundInviteAnswersADigestChallenge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	peer.requireAuth.Store(true)
	tr, _ := startBridge(t, ctx, func(c *Config) { c.Register.Password = testSIPPassword })

	call, err := tr.Invite(ctx, "sip:15551234567@"+peer.addr,
		map[string]string{"X-Conference": "sip-15551234567"})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	defer func() { _ = call.Hangup(ctx) }()

	select {
	case req := <-peer.invites:
		if req.GetHeader("Authorization") == nil {
			t.Error("the accepted INVITE carried no Authorization header")
		}
		if h := req.GetHeader("X-Conference"); h == nil || h.Value() != "sip-15551234567" {
			t.Errorf("X-Conference = %v, want it to survive the retry", h)
		}
	case <-time.After(testTimeout):
		t.Fatal("peer never accepted an INVITE")
	}
	if f := peer.authFailure.Load(); f != nil {
		t.Errorf("peer rejected the credentials: %s", *f)
	}
}

// The same challenge on a SIP MESSAGE, which travels the same unmatched-peer
// path as the INVITE does.
func TestOutboundMessageAnswersADigestChallenge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	peer.requireAuth.Store(true)
	tr, _ := startBridge(t, ctx, func(c *Config) { c.Register.Password = testSIPPassword })

	err := tr.SendMessage(ctx, "sip:15551234567@"+peer.addr, "sip:15559876543@example.com", "hi there")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	select {
	case req := <-peer.messages:
		if req.GetHeader("Authorization") == nil {
			t.Error("the accepted MESSAGE carried no Authorization header")
		}
		if string(req.Body()) != "hi there" {
			t.Errorf("body = %q, want it to survive the retry", req.Body())
		}
	case <-time.After(testTimeout):
		t.Fatal("peer never accepted a MESSAGE")
	}
	if f := peer.authFailure.Load(); f != nil {
		t.Errorf("peer rejected the credentials: %s", *f)
	}
}

// A challenge with no password configured has to name the missing setting:
// the alternative is a bare "401 Unauthorized" that reads as a server fault.
func TestChallengeWithoutAPasswordNamesTheSetting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	peer.requireAuth.Store(true)
	tr, _ := startBridge(t, ctx, nil)

	err := tr.SendMessage(ctx, "sip:15551234567@"+peer.addr, "sip:15559876543@example.com", "hi there")
	if err == nil {
		t.Fatal("expected the unauthenticated MESSAGE to fail")
	}
	if !strings.Contains(err.Error(), "sip.register.password") {
		t.Errorf("err = %v, want it to name sip.register.password", err)
	}
}

// The From user part is the bridge's SIP username, not the user agent's
// product name. It is what an Asterisk peer is matched on when the source
// address is a pod behind a Service, and a mismatch is a 401 no credential can
// answer.
func TestOutboundRequestsAreFromTheConfiguredUsername(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	peer := newFakePeer(t, ctx)
	tr, _ := startBridge(t, ctx, func(c *Config) { c.Username = "matrixbridge" })

	call, err := tr.Invite(ctx, "sip:15551234567@"+peer.addr, nil)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	defer func() { _ = call.Hangup(ctx) }()
	select {
	case req := <-peer.invites:
		if from := req.From(); from == nil || from.Address.User != "matrixbridge" {
			t.Errorf("INVITE From = %v, want user matrixbridge", from)
		}
	case <-time.After(testTimeout):
		t.Fatal("peer never received the INVITE")
	}

	if err := tr.SendMessage(ctx, "sip:15551234567@"+peer.addr, "", "hi there"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	select {
	case req := <-peer.messages:
		if from := req.From(); from == nil || from.Address.User != "matrixbridge" {
			t.Errorf("MESSAGE From = %v, want user matrixbridge", from)
		}
	case <-time.After(testTimeout):
		t.Fatal("peer never received the MESSAGE")
	}
}
