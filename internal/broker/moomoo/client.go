package moomoo

// client.go is a native Go OpenD client. It speaks the moomoo wire protocol
// directly (no Python sidecar), implementing the P5 market-data + session
// surface: InitConnect(1001), GetGlobalState(1002), KeepAlive(1004),
// Qot_Sub(3001), Qot_RegQotPush(3002), Qot_GetSubInfo(3003),
// Qot_GetBasicQot(3004), Qot_GetKL(3006), Qot_UpdateKL(3007, push) and
// Qot_RequestHistoryKL(3103, paged). Trading (Trd_*) is intentionally absent
// (deferred to P6).
//
// Production-grade properties:
//   - request/response correlation by serialNo (a waiter map);
//   - push vs reply demux (3007 Qot_UpdateKL is delivered to a callback, never
//     to a waiter);
//   - a single reader goroutine, a single writer mutex, no shared-state races;
//   - periodic KeepAlive driven by the server's keepAliveInterval;
//   - auto-reconnect with exponential backoff + jitter, and re-subscribe of
//     the prior subscription set on every successful reconnect;
//   - ctx cancellation everywhere and a clean Close with no goroutine leaks;
//   - structured zerolog logs, with no secrets ever logged.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotupdatekl"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// DefaultMaxSubscriptions is the FutuOpenD per-connection subscription cap
// (the documented 100-quota limit). Overridable via TMS_MOOMOO_MAX_SUB.
const DefaultMaxSubscriptions = 100

// ClientVer is the InitConnect clientVer we advertise. The SDK encodes
// "major*100 + minor"; we report 100 (1.00) — OpenD does not gate market-data
// on this for localhost.
const ClientVer = 100

// clientID identifies this connection to OpenD; uniqueness per connection is
// all OpenD requires. We make it stable-but-unique with a process nonce.
const clientIDPrefix = "tms-go"

var (
	// ErrClosed is returned by request methods after Close.
	ErrClosed = errors.New("moomoo: client closed")
	// ErrNotConnected is returned when a request is attempted while the
	// transport is down (between reconnect attempts).
	ErrNotConnected = errors.New("moomoo: not connected")
	// ErrProtocol indicates a malformed or unexpected server reply.
	ErrProtocol = errors.New("moomoo: protocol error")
)

// KLineHandler receives real-time K-line pushes (Qot_UpdateKL). It is invoked
// from the client's single reader goroutine; a handler that blocks stalls all
// further pushes, so a handler must not block (offload to a channel/queue).
// bars are already converted to the domain type and stamped UTC.
type KLineHandler func(symbol string, kl qotcommon.KLType, bars []domain.Bar)

// Subscription records one (symbol, klType) the caller asked us to keep
// subscribed; the set is replayed on every reconnect.
type Subscription struct {
	Symbol string
	KLType qotcommon.KLType
}

// Options configures a Client.
type Options struct {
	// Addr is the OpenD endpoint, host:port. For a real local OpenD this is
	// 127.0.0.1:11111; from a container, host.docker.internal:11111; for the
	// mock, the mock's listen address. (TMS_MOOMOO_ADDR)
	Addr string
	// MaxSubscriptions caps how many (symbol,klType) pairs may be subscribed;
	// 0 means DefaultMaxSubscriptions. (TMS_MOOMOO_MAX_SUB)
	MaxSubscriptions int
	// DialTimeout bounds a single TCP connect attempt (default 10s).
	DialTimeout time.Duration
	// RequestTimeout bounds a single request/response round trip (default 12s,
	// matching the SDK's _sync_req_timeout).
	RequestTimeout time.Duration
	// MinBackoff / MaxBackoff bound the reconnect backoff (defaults 500ms/30s).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Logger is the structured logger; a disabled logger is used if nil.
	Logger zerolog.Logger
	// OnKLine, if set, receives real-time K-line pushes. Optional.
	OnKLine KLineHandler
	// OnTrdOrder, if set, receives Trd_UpdateOrder pushes (order status
	// changes). Invoked from the reader goroutine; must not block. Optional.
	OnTrdOrder TrdOrderHandler
	// OnTrdOrderFill, if set, receives Trd_UpdateOrderFill pushes (fill
	// notifications). Invoked from the reader goroutine; must not block. Optional.
	OnTrdOrderFill TrdOrderFillHandler
	// rng is an injectable jitter source for deterministic backoff in tests;
	// nil uses a time-seeded source.
	rng *rand.Rand
}

func (o *Options) withDefaults() {
	if o.MaxSubscriptions <= 0 {
		o.MaxSubscriptions = DefaultMaxSubscriptions
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 12 * time.Second
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 500 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
}

// waiter is a pending request awaiting its reply, keyed by serialNo.
type waiter struct {
	protoID ProtoID
	ch      chan Frame
}

// connState holds everything tied to one live TCP connection. It is replaced
// wholesale on reconnect, so a stale reader/keepalive goroutine cannot touch
// the new connection.
type connState struct {
	conn   net.Conn
	connID uint64        // OpenD-assigned connID from InitConnect
	kaSec  int           // server-advertised keepAliveInterval seconds
	done   chan struct{} // closed when this connection's reader exits
}

// Client is a reconnecting native OpenD client.
type Client struct {
	opts Options
	log  zerolog.Logger
	rng  *rand.Rand

	serial atomic.Uint32 // serialNo generator (monotone per process)

	// mu guards conn, waiters, subs, closed.
	mu      sync.Mutex
	conn    *connState
	writeMu sync.Mutex // serializes frame writes to the active conn
	waiters map[uint32]*waiter
	subs    map[Subscription]struct{}
	closed  bool

	// connected is closed once the first InitConnect handshake completes, so
	// callers can wait for readiness. It is replaced on each (re)connect cycle
	// via connReady.
	readyMu sync.Mutex
	ready   chan struct{}

	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup

	// trdOnce/trdSt lazily hold the Trd_* trading-surface state (push handlers +
	// idempotency map); see trd_client.go. Market-data-only callers never touch
	// it, so the Client zero value stays valid.
	trdOnce sync.Once
	trdSt   *trdState
}

// NewClient builds a client. It does not connect; call Start.
func NewClient(opts Options) *Client {
	opts.withDefaults()
	log := opts.Logger
	rng := opts.rng
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Client{
		opts:    opts,
		log:     log.With().Str("component", "moomoo-client").Str("addr", opts.Addr).Logger(),
		rng:     rng,
		waiters: make(map[uint32]*waiter),
		subs:    make(map[Subscription]struct{}),
		ready:   make(chan struct{}),
	}
}

// Start launches the connect/reconnect supervisor goroutine and returns
// immediately. Use Ready to block until the first handshake completes (or the
// context is cancelled).
func (c *Client) Start(ctx context.Context) {
	c.runCtx, c.runCancel = context.WithCancel(ctx)
	c.wg.Add(1)
	go c.supervise()
}

// Ready blocks until the client has completed at least one InitConnect
// handshake, or ctx/Close fires first. It may be called repeatedly.
func (c *Client) Ready(ctx context.Context) error {
	c.readyMu.Lock()
	ch := c.ready
	c.readyMu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.runCtx.Done():
		return ErrClosed
	}
}

// Close tears down the supervisor, the active connection, and all goroutines,
// then waits for them to exit. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cur := c.conn
	c.mu.Unlock()

	if c.runCancel != nil {
		c.runCancel()
	}
	if cur != nil && cur.conn != nil {
		_ = cur.conn.SetDeadline(time.Now())
		_ = cur.conn.Close()
	}
	c.wg.Wait()
	c.failAllWaiters(ErrClosed)
	return nil
}

// nextSerial returns a fresh, never-zero serial number. The SDK treats 0 as
// "auto-assign"; we always assign explicitly and skip 0 on wraparound.
func (c *Client) nextSerial() uint32 {
	for {
		s := c.serial.Add(1)
		if s != 0 {
			return s
		}
	}
}

// supervise is the reconnect loop. It connects, runs until the connection
// dies, then backs off and retries, until the run context is cancelled.
func (c *Client) supervise() {
	defer c.wg.Done()
	attempt := 0
	for {
		if c.runCtx.Err() != nil {
			return
		}
		err := c.connectOnce(c.runCtx)
		if c.runCtx.Err() != nil {
			return
		}
		attempt++
		backoff := c.backoffFor(attempt)
		c.log.Warn().Err(err).Int("attempt", attempt).Dur("backoff", backoff).
			Msg("moomoo connection lost; reconnecting")
		select {
		case <-time.After(backoff):
		case <-c.runCtx.Done():
			return
		}
	}
}

// backoffFor computes an exponential backoff with full jitter, capped at
// MaxBackoff.
func (c *Client) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := float64(c.opts.MinBackoff)
	exp := base * math.Pow(2, float64(attempt-1))
	capped := math.Min(exp, float64(c.opts.MaxBackoff))
	// Full jitter in [MinBackoff, capped].
	span := capped - base
	if span < 0 {
		span = 0
	}
	j := base + c.rng.Float64()*span
	return time.Duration(j)
}

// connectOnce dials, handshakes, re-subscribes, then blocks running the reader
// + keepalive until the connection fails or the run context is cancelled. It
// returns the error that ended the connection (nil only on clean shutdown).
func (c *Client) connectOnce(ctx context.Context) error {
	d := net.Dialer{Timeout: c.opts.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.opts.Addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
		_ = tcp.SetNoDelay(true)
	}

	cs := &connState{conn: conn, done: make(chan struct{})}

	// Install as the active connection before the handshake so the reader can
	// route the InitConnect reply through the waiter map.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return ErrClosed
	}
	c.conn = cs
	c.mu.Unlock()

	// Reader owns the lifetime of cs: it reads frames until error, then signals
	// done. We start it before the handshake so InitConnect's reply is read.
	readerErr := make(chan error, 1)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		readerErr <- c.readLoop(cs)
		close(cs.done)
	}()

	// Handshake.
	if err := c.handshake(ctx, cs); err != nil {
		_ = conn.Close()
		<-cs.done
		c.detach(cs)
		return fmt.Errorf("handshake: %w", err)
	}

	c.log.Info().Uint64("connID", cs.connID).Int("keepAliveSec", cs.kaSec).
		Msg("moomoo connected")
	c.signalReady()

	// Re-subscribe the prior set (best-effort; failure logs but does not drop
	// the connection — the caller can re-issue).
	c.resubscribe(ctx)

	// Keepalive goroutine, scoped to this connection.
	kaCtx, kaCancel := context.WithCancel(ctx)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.keepAliveLoop(kaCtx, cs)
	}()

	// Block until reader exits or ctx done.
	var endErr error
	select {
	case endErr = <-readerErr:
	case <-ctx.Done():
		endErr = ctx.Err()
	}
	kaCancel()
	_ = conn.Close()
	<-cs.done // ensure reader fully exited
	c.detach(cs)
	c.failConnWaiters(ErrNotConnected)
	c.resetReady()
	if endErr == nil {
		endErr = ErrNotConnected
	}
	return endErr
}

// detach clears cs as the active connection iff it is still the active one.
func (c *Client) detach(cs *connState) {
	c.mu.Lock()
	if c.conn == cs {
		c.conn = nil
	}
	c.mu.Unlock()
}

// signalReady closes the readiness channel once (idempotent across reconnects:
// resetReady installs a fresh channel after a drop).
func (c *Client) signalReady() {
	c.readyMu.Lock()
	select {
	case <-c.ready:
		// already closed
	default:
		close(c.ready)
	}
	c.readyMu.Unlock()
}

func (c *Client) resetReady() {
	c.readyMu.Lock()
	select {
	case <-c.ready:
		// was closed: install a fresh, open channel for the next cycle.
		c.ready = make(chan struct{})
	default:
		// still open: leave as is.
	}
	c.readyMu.Unlock()
}

// readLoop reads frames until error, routing replies to waiters and pushes to
// the K-line handler. It returns the terminating error (io.EOF on clean close).
func (c *Client) readLoop(cs *connState) error {
	fr := NewFrameReader(cs.conn)
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			return err
		}
		c.dispatch(cs, frame)
	}
}

// dispatch routes one frame: pushes (Qot_UpdateKL) to the handler, everything
// else to the serial-keyed waiter.
func (c *Client) dispatch(cs *connState, frame Frame) {
	switch frame.Header.ProtoID {
	case ProtoQotUpdateKL:
		c.handlePush(frame)
		return
	case ProtoTrdUpdateOrder:
		c.handleTrdOrderPush(frame)
		return
	case ProtoTrdUpdateOrderFill:
		c.handleTrdOrderFillPush(frame)
		return
	}
	// Reply path: hand to the matching waiter.
	c.mu.Lock()
	w := c.waiters[frame.Header.SerialNo]
	if w != nil {
		delete(c.waiters, frame.Header.SerialNo)
	}
	c.mu.Unlock()
	if w == nil {
		c.log.Debug().Uint32("serialNo", frame.Header.SerialNo).
			Str("proto", frame.Header.ProtoID.String()).
			Msg("moomoo reply with no waiter (timed out or stray)")
		return
	}
	// Non-blocking send: the waiter channel is buffered(1).
	select {
	case w.ch <- frame:
	default:
	}
}

// handlePush decodes a Qot_UpdateKL push and invokes the K-line handler.
func (c *Client) handlePush(frame Frame) {
	if c.opts.OnKLine == nil {
		return
	}
	var rsp qotupdatekl.Response
	if err := proto.Unmarshal(frame.Body, &rsp); err != nil {
		c.log.Error().Err(err).Msg("moomoo: decode Qot_UpdateKL push")
		return
	}
	s2c := rsp.GetS2C()
	if s2c == nil {
		return
	}
	symbol := SymbolForSecurity(s2c.GetSecurity())
	kl := qotcommon.KLType(s2c.GetKlType())
	klList := s2c.GetKlList()
	bars := make([]domain.Bar, 0, len(klList))
	for _, k := range klList {
		if k.GetIsBlank() {
			continue
		}
		b, err := BarFromKLine(symbol, kl, k)
		if err != nil {
			c.log.Warn().Err(err).Str("symbol", symbol).Msg("moomoo: skip bad push K-line")
			continue
		}
		bars = append(bars, b)
	}
	if len(bars) > 0 {
		c.opts.OnKLine(symbol, kl, bars)
	}
}

// activeConnID returns the OpenD-assigned connID of the live connection, used
// to populate moomoo write-op PacketIDs ({connID, serialNo}). ok is false when
// the transport is down.
func (c *Client) activeConnID() (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.conn == nil {
		return 0, false
	}
	return c.conn.connID, true
}

// roundTrip encodes req for protoID, sends it on the active connection, and
// waits for the matching reply (or ctx/timeout). It returns the reply body.
func (c *Client) roundTrip(ctx context.Context, protoID ProtoID, req proto.Message) ([]byte, error) {
	return c.roundTripSerial(ctx, protoID, c.nextSerial(), req)
}

// roundTripSerial is roundTrip with a caller-supplied serial number. Trading
// write ops (PlaceOrder/ModifyOrder) need the frame serialNo to MATCH the
// serialNo embedded in their PacketID anti-replay token, so they reserve a
// serial, build the PacketID with it, and route the frame through here.
func (c *Client) roundTripSerial(ctx context.Context, protoID ProtoID, serial uint32, req proto.Message) ([]byte, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("moomoo: marshal %s: %w", protoID, err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	cs := c.conn
	if cs == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	w := &waiter{protoID: protoID, ch: make(chan Frame, 1)}
	c.waiters[serial] = w
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.waiters, serial)
		c.mu.Unlock()
	}()

	frame := EncodeFrame(protoID, serial, body)
	if err := c.writeFrame(cs, frame); err != nil {
		return nil, err
	}

	reqCtx := ctx
	if c.opts.RequestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}

	select {
	case reply := <-w.ch:
		return reply.Body, nil
	case <-reqCtx.Done():
		return nil, fmt.Errorf("moomoo: %s round trip: %w", protoID, reqCtx.Err())
	case <-cs.done:
		return nil, ErrNotConnected
	case <-c.runCtx.Done():
		return nil, ErrClosed
	}
}

// writeFrame writes a full frame to cs.conn under the write mutex.
func (c *Client) writeFrame(cs *connState, frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Bound the write so a wedged peer cannot block us forever.
	if c.opts.RequestTimeout > 0 {
		_ = cs.conn.SetWriteDeadline(time.Now().Add(c.opts.RequestTimeout))
		defer cs.conn.SetWriteDeadline(time.Time{})
	}
	if _, err := cs.conn.Write(frame); err != nil {
		return fmt.Errorf("moomoo: write: %w", err)
	}
	return nil
}

// failAllWaiters resolves every pending waiter with err (used on Close).
func (c *Client) failAllWaiters(err error) {
	c.mu.Lock()
	ws := c.waiters
	c.waiters = make(map[uint32]*waiter)
	c.mu.Unlock()
	_ = err
	_ = ws // waiters select on cs.done / runCtx; nothing to push, GC handles it.
}

// failConnWaiters is a hook symmetric with failAllWaiters; waiters already
// unblock via cs.done, so this only clears the map.
func (c *Client) failConnWaiters(err error) {
	c.mu.Lock()
	c.waiters = make(map[uint32]*waiter)
	c.mu.Unlock()
	_ = err
}
