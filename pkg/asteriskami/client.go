package asteriskami

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Config is everything needed to reach one Asterisk manager interface.
// All of it is site-specific and therefore comes from the bridge config file.
type Config struct {
	Address  string        `yaml:"address"`
	Username string        `yaml:"username"`
	Secret   string        `yaml:"secret"`
	TLS      bool          `yaml:"tls"`
	Timeout  time.Duration `yaml:"timeout"`
	// Reconnect is the delay between reconnect attempts. It does not back off:
	// AMI outages are usually an Asterisk restart, and a fixed short retry
	// reconnects promptly without hammering.
	Reconnect time.Duration `yaml:"reconnect_interval"`
}

// ErrNotConnected is returned by Action when there is no live AMI socket.
var ErrNotConnected = errors.New("asterisk AMI is not connected")

// EventHandler is called for every asynchronous AMI event, on the reader
// goroutine. Handlers must not block: dispatch anything slow to a goroutine.
type EventHandler func(ctx context.Context, evt *Packet)

// Client is a reconnecting AMI client. Its zero value is not usable; use New.
type Client struct {
	cfg Config
	log zerolog.Logger

	handlersMu sync.RWMutex
	handlers   []EventHandler

	connMu sync.Mutex
	conn   net.Conn
	w      *bufio.Writer

	pendingMu sync.Mutex
	pending   map[string]chan *Packet

	actionSeq atomic.Uint64
	connected atomic.Bool
}

// New creates a client. It does not connect; call Run.
func New(cfg Config, log zerolog.Logger) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Reconnect <= 0 {
		cfg.Reconnect = 5 * time.Second
	}
	return &Client{
		cfg:     cfg,
		log:     log,
		pending: make(map[string]chan *Packet),
	}
}

// OnEvent registers an event handler. Register all handlers before Run.
func (c *Client) OnEvent(h EventHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers = append(c.handlers, h)
}

// Connected reports whether an authenticated AMI socket is currently up.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run connects, logs in and reads events until ctx is cancelled, reconnecting
// on any failure. It returns only when ctx is done.
func (c *Client) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.session(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn().Err(err).
				Dur("retry_in", c.cfg.Reconnect).
				Msg("AMI session ended, reconnecting")
		}
		c.connected.Store(false)
		c.failPending(errors.New("AMI connection lost"))
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.Reconnect):
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() {
		_ = conn.Close()
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
			c.w = nil
		}
		c.connMu.Unlock()
	}()

	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(c.cfg.Timeout))
	greeting, err := ReadGreeting(r)
	if err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	c.log.Debug().Str("greeting", greeting).Msg("Connected to Asterisk AMI")

	c.connMu.Lock()
	c.conn = conn
	c.w = bufio.NewWriter(conn)
	c.connMu.Unlock()

	if err := c.login(ctx, r); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	c.connected.Store(true)
	c.log.Info().Msg("Authenticated to Asterisk AMI")

	// Once logged in there is no fixed cadence of events, so no read deadline.
	// Liveness is instead detected by the periodic Ping in Heartbeat.
	_ = conn.SetReadDeadline(time.Time{})

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		pkt, err := ReadPacket(r)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		c.dispatch(ctx, pkt)
	}
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: c.cfg.Timeout}
	if c.cfg.TLS {
		return tls.DialWithDialer(d, "tcp", c.cfg.Address, nil)
	}
	return d.DialContext(ctx, "tcp", c.cfg.Address)
}

// login sends the Login action inline, before the reader loop starts, because
// the reply is the next packet on the wire.
func (c *Client) login(ctx context.Context, r *bufio.Reader) error {
	p := NewPacket()
	p.Set("Action", "Login")
	p.Set("Username", c.cfg.Username)
	p.Set("Secret", c.cfg.Secret)
	if err := c.writePacket(p); err != nil {
		return err
	}
	reply, err := ReadPacket(r)
	if err != nil {
		return err
	}
	if !reply.IsSuccess() {
		// Never log the secret, and never echo the reply verbatim: AMI reflects
		// the offending action back on some errors.
		return fmt.Errorf("login rejected: %s", reply.Get("Message"))
	}
	_ = ctx
	return nil
}

func (c *Client) dispatch(ctx context.Context, pkt *Packet) {
	if id := pkt.ActionID(); id != "" {
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		c.pendingMu.Unlock()
		if ok {
			// Non-blocking: the waiter has a buffered channel of one and only
			// wants the first reply. Follow-up list events fall through to the
			// event handlers.
			select {
			case ch <- pkt:
				return
			default:
			}
		}
	}
	if pkt.Event() == "" {
		return
	}
	c.handlersMu.RLock()
	handlers := c.handlers
	c.handlersMu.RUnlock()
	for _, h := range handlers {
		h(ctx, pkt)
	}
}

func (c *Client) writePacket(p *Packet) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.w == nil {
		return ErrNotConnected
	}
	if _, err := c.w.Write(p.Marshal()); err != nil {
		return err
	}
	return c.w.Flush()
}

// Action sends an action and waits for its reply.
func (c *Client) Action(ctx context.Context, p *Packet) (*Packet, error) {
	if !c.connected.Load() {
		return nil, ErrNotConnected
	}
	id := strconv.FormatUint(c.actionSeq.Add(1), 10)
	p.Set("ActionID", id)

	ch := make(chan *Packet, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.writePacket(p); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.cfg.Timeout):
		return nil, fmt.Errorf("timed out waiting for reply to %s", p.Get("Action"))
	case reply := <-ch:
		if !reply.IsSuccess() {
			return reply, fmt.Errorf("%s failed: %s", p.Get("Action"), reply.Get("Message"))
		}
		return reply, nil
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	_ = err
}

// Heartbeat pings AMI on the given interval so a half-open TCP connection is
// noticed. A failed ping closes the socket, which makes Run reconnect.
func (c *Client) Heartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.connected.Load() {
				continue
			}
			p := NewPacket()
			p.Set("Action", "Ping")
			if _, err := c.Action(ctx, p); err != nil && ctx.Err() == nil {
				c.log.Warn().Err(err).Msg("AMI ping failed, dropping connection")
				c.connMu.Lock()
				conn := c.conn
				c.connMu.Unlock()
				if conn != nil {
					_ = conn.Close()
				}
			}
		}
	}
}
