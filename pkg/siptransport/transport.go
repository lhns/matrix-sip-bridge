// Package siptransport makes the bridge a SIP endpoint.
//
// It is a user agent, not a remote control for one PBX: the SIP server dials
// the bridge like any other endpoint, and call policy stays in the server's
// own routing. The bridge has no media stack. An inbound call leg is a control
// leg that exists to be answered and then hung up by the dialplan; the audio
// goes SIP server <-> livekit-sip <-> LiveKit and never through this process.
package siptransport

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/rs/zerolog"

	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// maxPacketSize is chan_sip's SIP_MAX_PACKET_SIZE. A larger request is
// refused outright rather than written to a socket that will drop it.
const maxPacketSize = 20480

// messageHandlerTimeout bounds an inbound MESSAGE handler. Past Timer F the
// far end has given up on the transaction, so a reply after that reaches
// nobody and the handler is only holding the connection open.
const messageHandlerTimeout = 32 * time.Second

// MessageHandler is called for each inbound SIP MESSAGE that passed the
// content-type check. Returning an error makes the bridge reply 500.
type MessageHandler func(ctx context.Context, msg InboundMessage) error

// InviteHandler is called for each inbound INVITE. It owns the leg: it must
// eventually Answer or Reject it. It runs on its own goroutine.
type InviteHandler func(ctx context.Context, call *InboundCall)

// InboundMessage is a SIP MESSAGE as it arrived. The values are the raw URIs
// from the request; normalising them is the caller's job.
type InboundMessage struct {
	From string
	To   string
	Body string
}

// Transport is the bridge's SIP user agent.
type Transport struct {
	cfg Config
	log zerolog.Logger
	// metrics may be nil; every Recorder method tolerates that.
	metrics *metrics.Recorder

	ua  *sipgo.UserAgent
	srv *sipgo.Server
	cli *sipgo.Client
	dlg *sipgo.DialogServerCache
	dua *sipgo.DialogUA

	contact   sip.ContactHeader
	mediaHost string
	allow     sip.Header

	onMessage atomic.Pointer[MessageHandler]
	onInvite  atomic.Pointer[InviteHandler]

	listening  atomic.Bool
	registered atomic.Bool

	// baseCtx is the transport's own lifetime, and the parent of every
	// context handed to a request handler.
	//
	// sipgo's sip.ServerTransactionContext cannot be used for that: it
	// cancels the context it just created whenever the termination hook was
	// registered successfully, so it returns a context that is already done.
	// Handlers then ran their whole setup on a dead context.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	closeOnce sync.Once
}

// New builds the user agent and registers the request handlers. It does not
// open a socket; Run does that.
func New(cfg Config, log zerolog.Logger, rec *metrics.Recorder) (*Transport, error) {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	mediaHost, err := resolveHost(cfg.mediaHost())
	if err != nil {
		return nil, fmt.Errorf("sip.media_address %q: %w", cfg.mediaHost(), err)
	}

	slogger := slog.New(slog.NewTextHandler(zerologWriter{log}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("matrix-sip-bridge"),
		sipgo.WithUserAgentHostname(cfg.Domain),
	)
	if err != nil {
		return nil, fmt.Errorf("create SIP user agent: %w", err)
	}
	publicHost, publicPort := cfg.publicHostPort()
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(slogger))
	if err != nil {
		return nil, fmt.Errorf("create SIP server: %w", err)
	}
	cli, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname(publicHost),
		sipgo.WithClientLogger(slogger),
	)
	if err != nil {
		return nil, fmt.Errorf("create SIP client: %w", err)
	}

	contact := sip.ContactHeader{Address: sip.Uri{
		Scheme: "sip",
		User:   cfg.Username,
		Host:   publicHost,
		Port:   publicPort,
	}}
	baseCtx, baseCancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:        cfg,
		log:        log,
		metrics:    rec,
		ua:         ua,
		srv:        srv,
		cli:        cli,
		dlg:        sipgo.NewDialogServerCache(cli, contact),
		dua:        &sipgo.DialogUA{Client: cli, ContactHDR: contact},
		contact:    contact,
		mediaHost:  mediaHost,
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}
	srv.OnInvite(t.handleInvite)
	srv.OnAck(t.handleAck)
	srv.OnBye(t.handleBye)
	srv.OnMessage(t.handleMessage)
	srv.OnOptions(t.handleOptions)
	// chan_sip only routes a text message to an endpoint whose Allow lists
	// MESSAGE, so the header has to go on every response the bridge sends.
	t.allow = sip.NewHeader("Allow", strings.Join(srv.RegisteredMethods(), ", "))
	return t, nil
}

// fromHeader is the bridge's own SIP identity, with a fresh dialog tag.
//
// It has to be built from sip.username rather than left to the user agent,
// which would put its own product name in the user part. A SIP server that
// cannot match the bridge on its source address -- a pod address behind a
// Service -- matches on the From user part instead, and a mismatch there is a
// 401 that no password can answer.
func (t *Transport) fromHeader() *sip.FromHeader {
	h := &sip.FromHeader{
		Address: sip.Uri{Scheme: "sip", User: t.cfg.Username, Host: t.cfg.Domain},
		Params:  sip.NewParams(),
	}
	h.Params.Add("tag", sip.GenerateTagN(16))
	return h
}

// OnMessage sets the inbound SIP MESSAGE handler.
func (t *Transport) OnMessage(h MessageHandler) { t.onMessage.Store(&h) }

// OnInvite sets the inbound call handler.
func (t *Transport) OnInvite(h InviteHandler) { t.onInvite.Store(&h) }

// Ready reports whether the endpoint is usable: listening, and registered if
// registration is configured.
func (t *Transport) Ready() bool {
	if !t.listening.Load() {
		return false
	}
	return !t.cfg.Register.Enabled || t.registered.Load()
}

// Run opens the configured listener and serves until ctx is done.
func (t *Transport) Run(ctx context.Context) error {
	if strings.EqualFold(t.cfg.Transport, "udp") {
		conn, err := net.ListenPacket("udp", t.cfg.Listen)
		if err != nil {
			return fmt.Errorf("listen udp %s: %w", t.cfg.Listen, err)
		}
		return t.serve(ctx, func() error { return t.srv.ServeUDP(conn) }, conn)
	}
	ln, err := net.Listen("tcp", t.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen tcp %s: %w", t.cfg.Listen, err)
	}
	return t.RunWithListener(ctx, ln)
}

// RunWithListener serves on a listener the caller opened. Tests use it to
// bind an ephemeral port without racing on it.
func (t *Transport) RunWithListener(ctx context.Context, ln net.Listener) error {
	return t.serve(ctx, func() error { return t.srv.ServeTCP(ln) }, ln)
}

func (t *Transport) serve(ctx context.Context, serve func() error, closer io.Closer) error {
	t.listening.Store(true)
	// The one moment worth a timestamp: everything before it is the startup
	// window the readiness endpoint covers, and a listener that never binds
	// leaves the gauge at zero.
	t.metrics.SIPListening()
	defer t.listening.Store(false)
	go func() {
		<-ctx.Done()
		t.Close()
		_ = closer.Close()
	}()
	go t.registerLoop(ctx)
	err := serve()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Close releases the user agent. Run's context cancellation does this.
func (t *Transport) Close() {
	t.closeOnce.Do(func() {
		t.baseCancel()
		_ = t.srv.Close()
		_ = t.cli.Close()
		_ = t.ua.Close()
	})
}

func (t *Transport) handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	t.respond(req, tx, 200, "OK")
}

// respond replies to a non-dialog request, always advertising Allow.
func (t *Transport) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	res.AppendHeader(t.allow)
	if err := tx.Respond(res); err != nil {
		t.log.Warn().Err(err).Int("status", code).Msg("Failed to send SIP response")
	}
}

// resolveHost turns a hostname into an IP literal. SDP carries addresses, not
// names, so this has to happen before the first answer rather than during one.
func resolveHost(host string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("no address configured")
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	if len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", fmt.Errorf("no addresses for %q", host)
}

// defaultRegisterRefresh is the refresh interval for a registration whose
// expiry is too small to halve. Config.ApplyDefaults gives Expiry five
// minutes, so this is a floor against a nonsensical config rather than a
// default anyone meets: without it such a config REGISTERs in a tight loop.
const defaultRegisterRefresh = 150 * time.Second

// registerLoop keeps the bridge registered with the SIP server.
func (t *Transport) registerLoop(ctx context.Context) {
	if !t.cfg.Register.Enabled {
		return
	}
	refresh := t.cfg.Register.Expiry / 2
	if refresh <= 0 {
		refresh = defaultRegisterRefresh
	}
	// One timer, reset each pass rather than a fresh time.After in the select:
	// a time.After timer is not stopped when the other arm wins, so a shutdown
	// left one live for the rest of the refresh interval.
	timer := time.NewTimer(refresh)
	defer timer.Stop()
	for {
		if err := t.register(ctx, t.cfg.Register.Expiry); err != nil {
			t.registered.Store(false)
			if ctx.Err() != nil {
				return
			}
			t.log.Warn().Err(err).Msg("SIP registration failed")
		} else {
			t.registered.Store(true)
		}
		timer.Reset(refresh)
		select {
		case <-ctx.Done():
			// Best-effort de-registration; the server expires it anyway.
			deregCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = t.register(deregCtx, 0)
			cancel()
			return
		case <-timer.C:
		}
	}
}

func (t *Transport) register(ctx context.Context, expiry time.Duration) error {
	recipient := sip.Uri{Scheme: "sip", Host: t.cfg.Domain}
	req := sip.NewRequest(sip.REGISTER, recipient)
	req.AppendHeader(t.fromHeader())
	req.AppendHeader(&t.contact)
	req.AppendHeader(t.allow)
	exp := sip.ExpiresHeader(expiry / time.Second)
	req.AppendHeader(&exp)
	req.SetTransport(strings.ToUpper(t.cfg.Transport))
	req.SetDestination(t.cfg.Register.Server)

	res, err := t.cli.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}
	if isChallenge(res) {
		res, err = t.registerWithDigest(ctx, req, res)
		if err != nil {
			return err
		}
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("REGISTER rejected: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// isChallenge reports whether a response is a digest challenge the bridge can
// answer.
func isChallenge(res *sip.Response) bool {
	return res.StatusCode == 401 || res.StatusCode == 407
}

// authorize copies req with an Authorization header answering the challenge.
//
// sip.username and sip.register.password are the bridge's only SIP
// credentials, and they are not only for REGISTER: a static peer whose source
// address the server cannot match -- a pod address behind a Service -- is
// matched on the From user part instead, and that path always challenges.
func (t *Transport) authorize(req *sip.Request, challenge *sip.Response) (*sip.Request, error) {
	header, authHeader := "WWW-Authenticate", "Authorization"
	if challenge.StatusCode == 407 {
		header, authHeader = "Proxy-Authenticate", "Proxy-Authorization"
	}
	h := challenge.GetHeader(header)
	if h == nil {
		return nil, fmt.Errorf("%d response without %s", challenge.StatusCode, header)
	}
	if t.cfg.Register.Password == "" {
		return nil, fmt.Errorf("%d %s and sip.register.password is not set", challenge.StatusCode, challenge.Reason)
	}
	chal, err := digest.ParseChallenge(h.Value())
	if err != nil {
		return nil, fmt.Errorf("parse digest challenge: %w", err)
	}
	cred, err := digest.Digest(chal, digest.Options{
		Method:   string(req.Method),
		URI:      req.Recipient.Host,
		Username: t.cfg.Username,
		Password: t.cfg.Register.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("compute digest: %w", err)
	}
	authed := req.Clone()
	// The Via carries the branch of the first transaction; the transport layer
	// regenerates it for the retry.
	authed.RemoveHeader("Via")
	authed.AppendHeader(sip.NewHeader(authHeader, cred.String()))
	return authed, nil
}

// doWithDigest sends a non-dialog request and answers a digest challenge once.
func (t *Transport) doWithDigest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	res, err := t.cli.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if !isChallenge(res) {
		return res, nil
	}
	authed, err := t.authorize(req, res)
	if err != nil {
		return nil, err
	}
	return t.cli.Do(ctx, authed, sipgo.ClientRequestIncreaseCSEQ, sipgo.ClientRequestAddVia)
}

func (t *Transport) registerWithDigest(ctx context.Context, req *sip.Request, challenge *sip.Response) (*sip.Response, error) {
	authed, err := t.authorize(req, challenge)
	if err != nil {
		return nil, err
	}
	return t.cli.Do(ctx, authed, sipgo.ClientRequestIncreaseCSEQ, sipgo.ClientRequestAddVia)
}

// zerologWriter lets sipgo's slog output land in the bridge's log.
type zerologWriter struct{ log zerolog.Logger }

func (w zerologWriter) Write(p []byte) (int, error) {
	w.log.Debug().Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
