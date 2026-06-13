// Package mock implements a protocol-faithful in-repo OpenD TCP server: it
// speaks the IDENTICAL moomoo wire framing and protobuf messages as the real
// FutuOpenD, serving InitConnect / GetGlobalState / KeepAlive / Qot_Sub /
// Qot_GetSubInfo / Qot_GetBasicQot / Qot_GetKL / Qot_RequestHistoryKL from a
// pluggable BarSource (our Postgres bars or an in-memory fixture), and PUSHING
// Qot_UpdateKL on a CONTROLLABLE clock so a test can deterministically replay a
// day of stored bars as if they were live ticks.
//
// This is PERMANENT test infrastructure and the deterministic gate driver. The
// real-vs-mock switch is config (TMS_MOOMOO_ADDR): point the native client at
// this server's Addr() and it cannot tell the difference at the wire level.
//
// It is intentionally minimal but faithful: same header layout (magic 'FT',
// protoID, fmt, ver, serialNo, bodyLen, SHA-1(body), reserved), same protobuf
// request/response messages, same reply-serialNo-echo semantics, and pushes
// carry serialNo 0 (the SDK does not correlate pushes).
package mock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/getglobalstate"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/initconnect"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/keepalive"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotgetbasicqot"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotgetkl"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotgetsubinfo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotregqotpush"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotrequesthistorykl"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotsub"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotupdatekl"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// BarSource supplies historical bars to the mock server. A Postgres-backed
// implementation (PGBarSource) drives klines from tms.bars_daily/bars_intraday;
// an in-memory one (MemBarSource) serves fixtures in unit tests.
type BarSource interface {
	// Bars returns symbol's bars at the given K-line width over [begin, end]
	// inclusive, ascending by ts (UTC). Unknown symbols return an empty slice.
	Bars(ctx context.Context, symbol string, kl qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error)
}

// Options configures a mock server.
type Options struct {
	// Listen is the TCP listen address; ":0" picks a free port (read it back
	// via Addr()). Defaults to "127.0.0.1:0".
	Listen string
	// Source supplies bars for HistoryKL / GetKL / push replay. Required.
	Source BarSource
	// KeepAliveInterval is advertised to clients in the InitConnect reply
	// (seconds). Defaults to 10.
	KeepAliveInterval int
	// ServerVer is reported in InitConnect / GetGlobalState. Defaults to 900.
	ServerVer int32
	// Logger is the structured logger (disabled if zero value).
	Logger zerolog.Logger
	// Now, if set, supplies the server's notion of "now" for GetGlobalState's
	// time field; defaults to time.Now. Tests pin it for determinism.
	Now func() time.Time
}

// Server is a running mock OpenD.
type Server struct {
	opts    Options
	log     zerolog.Logger
	ln      net.Listener
	now     func() time.Time
	connSeq uint64

	mu         sync.Mutex
	conns      map[*conn]struct{}
	closed     bool
	wg         sync.WaitGroup
	acceptDone chan struct{}
}

// conn is one accepted client connection and its subscription registry.
type conn struct {
	srv    *Server
	nc     net.Conn
	connID uint64

	writeMu sync.Mutex

	subMu sync.Mutex
	// subs maps a subscribed (symbol,klType) to whether push is registered on
	// this connection. The push-replay driver consults it.
	subs map[mo.Subscription]bool
}

// New creates a mock server and begins listening immediately (so Addr is
// valid on return) but does not accept until Serve is called.
func New(opts Options) (*Server, error) {
	if opts.Source == nil {
		return nil, errors.New("mock: Options.Source is required")
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.KeepAliveInterval <= 0 {
		opts.KeepAliveInterval = 10
	}
	if opts.ServerVer == 0 {
		opts.ServerVer = 900
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("mock: listen %s: %w", opts.Listen, err)
	}
	return &Server{
		opts:       opts,
		log:        opts.Logger.With().Str("component", "moomoo-mock").Logger(),
		ln:         ln,
		now:        now,
		conns:      make(map[*conn]struct{}),
		acceptDone: make(chan struct{}),
	}, nil
}

// Addr returns the actual listen address (host:port), valid after New.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve accepts connections until ctx is cancelled or Close is called. It
// blocks; run it in a goroutine. It returns nil on graceful shutdown.
func (s *Server) Serve(ctx context.Context) error {
	// Close the listener when ctx is done so Accept unblocks.
	go func() {
		select {
		case <-ctx.Done():
			s.closeListener()
		case <-s.acceptDone:
		}
	}()
	defer close(s.acceptDone)

	for {
		nc, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mock: accept: %w", err)
		}
		s.connSeq++
		c := &conn{
			srv:    s,
			nc:     nc,
			connID: s.connSeq,
			subs:   make(map[mo.Subscription]bool),
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.serve(ctx)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
		}()
	}
}

func (s *Server) closeListener() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		_ = s.ln.Close()
	}
	s.mu.Unlock()
}

// Close stops accepting, closes all connections, and waits for handlers to
// exit. Idempotent.
func (s *Server) Close() error {
	s.closeListener()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.nc.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// DropConns force-closes every live client connection WITHOUT stopping the
// listener, simulating a transient network/server hiccup. The server keeps
// accepting, so a reconnecting client lands on a fresh connection and replays
// its subscriptions. Returns the number of connections dropped.
func (s *Server) DropConns() int {
	s.mu.Lock()
	n := 0
	for c := range s.conns {
		_ = c.nc.Close()
		n++
	}
	s.mu.Unlock()
	return n
}

// Conns returns the number of live client connections (for tests).
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// serve reads frames and dispatches each to a handler until the connection
// closes or ctx is cancelled.
func (c *conn) serve(ctx context.Context) {
	defer c.nc.Close()
	go func() {
		<-ctx.Done()
		_ = c.nc.Close()
	}()
	fr := mo.NewFrameReader(c.nc)
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				c.srv.log.Debug().Err(err).Msg("mock: read frame")
			}
			return
		}
		if err := c.handle(ctx, frame); err != nil {
			c.srv.log.Warn().Err(err).Str("proto", frame.Header.ProtoID.String()).Msg("mock: handle")
			return
		}
	}
}

// reply encodes resp for protoID and writes it echoing the request serialNo.
func (c *conn) reply(protoID mo.ProtoID, serialNo uint32, resp proto.Message) error {
	body, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("mock: marshal %s reply: %w", protoID, err)
	}
	return c.writeFrame(mo.EncodeFrame(protoID, serialNo, body))
}

func (c *conn) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.nc.Write(frame)
	return err
}

// retOK is a Common.RetType_Succeed (0) helper.
func retOK() *int32 { v := int32(0); return &v }

// handle dispatches one request frame to its protocol handler.
func (c *conn) handle(ctx context.Context, f mo.Frame) error {
	sn := f.Header.SerialNo
	switch f.Header.ProtoID {
	case mo.ProtoInitConnect:
		return c.onInitConnect(sn, f.Body)
	case mo.ProtoGetGlobalState:
		return c.onGetGlobalState(sn, f.Body)
	case mo.ProtoKeepAlive:
		return c.onKeepAlive(sn, f.Body)
	case mo.ProtoQotSub:
		return c.onQotSub(sn, f.Body)
	case mo.ProtoQotRegQotPush:
		return c.onRegQotPush(sn, f.Body)
	case mo.ProtoQotGetSubInfo:
		return c.onGetSubInfo(sn, f.Body)
	case mo.ProtoQotGetBasicQot:
		return c.onGetBasicQot(ctx, sn, f.Body)
	case mo.ProtoQotGetKL:
		return c.onGetKL(ctx, sn, f.Body)
	case mo.ProtoQotRequestHistoryKL:
		return c.onRequestHistoryKL(ctx, sn, f.Body)
	default:
		// Unknown/unsupported proto: stay protocol-faithful by ignoring (real
		// OpenD would reject; for P5's market-data surface this never happens).
		c.srv.log.Debug().Str("proto", f.Header.ProtoID.String()).Msg("mock: unsupported proto ignored")
		return nil
	}
}

func (c *conn) onInitConnect(sn uint32, body []byte) error {
	var req initconnect.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode InitConnect: %w", err)
	}
	resp := &initconnect.Response{
		RetType: retOK(),
		S2C: &initconnect.S2C{
			ServerVer:         proto.Int32(c.srv.opts.ServerVer),
			LoginUserID:       proto.Uint64(1),
			ConnID:            proto.Uint64(c.connID),
			ConnAESKey:        proto.String("0000000000000000"), // 16 bytes; unused (no encryption)
			KeepAliveInterval: proto.Int32(int32(c.srv.opts.KeepAliveInterval)),
		},
	}
	return c.reply(mo.ProtoInitConnect, sn, resp)
}

func (c *conn) onGetGlobalState(sn uint32, body []byte) error {
	resp := &getglobalstate.Response{
		RetType: retOK(),
		S2C: &getglobalstate.S2C{
			MarketHK:       proto.Int32(int32(qotcommon.QotMarketState_QotMarketState_Closed)),
			MarketUS:       proto.Int32(int32(qotcommon.QotMarketState_QotMarketState_Morning)),
			MarketSH:       proto.Int32(int32(qotcommon.QotMarketState_QotMarketState_Closed)),
			MarketSZ:       proto.Int32(int32(qotcommon.QotMarketState_QotMarketState_Closed)),
			MarketHKFuture: proto.Int32(int32(qotcommon.QotMarketState_QotMarketState_Closed)),
			QotLogined:     proto.Bool(true),
			TrdLogined:     proto.Bool(false),
			ServerVer:      proto.Int32(c.srv.opts.ServerVer),
			ServerBuildNo:  proto.Int32(1),
			Time:           proto.Int64(c.srv.now().Unix()),
			ConnID:         proto.Uint64(c.connID),
		},
	}
	return c.reply(mo.ProtoGetGlobalState, sn, resp)
}

func (c *conn) onKeepAlive(sn uint32, body []byte) error {
	var req keepalive.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode KeepAlive: %w", err)
	}
	resp := &keepalive.Response{
		RetType: retOK(),
		S2C:     &keepalive.S2C{Time: proto.Int64(c.srv.now().Unix())},
	}
	return c.reply(mo.ProtoKeepAlive, sn, resp)
}

func (c *conn) onQotSub(sn uint32, body []byte) error {
	var req qotsub.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Qot_Sub: %w", err)
	}
	c2s := req.GetC2S()
	sub := c2s.GetIsSubOrUnSub()
	regPush := c2s.GetIsRegOrUnRegPush()
	c.subMu.Lock()
	for _, st := range c2s.GetSubTypeList() {
		kl, ok := klTypeForSubType(qotcommon.SubType(st))
		if !ok {
			continue
		}
		for _, sec := range c2s.GetSecurityList() {
			key := mo.Subscription{Symbol: sec.GetCode(), KLType: kl}
			if sub {
				c.subs[key] = regPush || c.subs[key]
			} else {
				delete(c.subs, key)
			}
		}
	}
	c.subMu.Unlock()
	return c.reply(mo.ProtoQotSub, sn, &qotsub.Response{RetType: retOK(), S2C: &qotsub.S2C{}})
}

func (c *conn) onRegQotPush(sn uint32, body []byte) error {
	var req qotregqotpush.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Qot_RegQotPush: %w", err)
	}
	c2s := req.GetC2S()
	reg := c2s.GetIsRegOrUnReg()
	c.subMu.Lock()
	for _, st := range c2s.GetSubTypeList() {
		kl, ok := klTypeForSubType(qotcommon.SubType(st))
		if !ok {
			continue
		}
		for _, sec := range c2s.GetSecurityList() {
			key := mo.Subscription{Symbol: sec.GetCode(), KLType: kl}
			if _, exists := c.subs[key]; exists {
				c.subs[key] = reg
			}
		}
	}
	c.subMu.Unlock()
	return c.reply(mo.ProtoQotRegQotPush, sn, &qotregqotpush.Response{RetType: retOK(), S2C: &qotregqotpush.S2C{}})
}

func (c *conn) onGetSubInfo(sn uint32, body []byte) error {
	c.subMu.Lock()
	used := int32(len(c.subs))
	c.subMu.Unlock()
	resp := &qotgetsubinfo.Response{
		RetType: retOK(),
		S2C: &qotgetsubinfo.S2C{
			TotalUsedQuota: proto.Int32(used),
			RemainQuota:    proto.Int32(int32(mo.DefaultMaxSubscriptions) - used),
		},
	}
	return c.reply(mo.ProtoQotGetSubInfo, sn, resp)
}

func (c *conn) onGetBasicQot(ctx context.Context, sn uint32, body []byte) error {
	var req qotgetbasicqot.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Qot_GetBasicQot: %w", err)
	}
	var quotes []*qotcommon.BasicQot
	for _, sec := range req.GetC2S().GetSecurityList() {
		symbol := sec.GetCode()
		// Derive the latest basic quote from the most recent daily bar.
		bars, err := c.srv.opts.Source.Bars(ctx, symbol, qotcommon.KLType_KLType_Day,
			time.Unix(0, 0).UTC(), c.srv.now())
		if err != nil {
			return fmt.Errorf("mock: source bars %s: %w", symbol, err)
		}
		bq := basicQotFromBars(symbol, bars)
		quotes = append(quotes, bq)
	}
	resp := &qotgetbasicqot.Response{
		RetType: retOK(),
		S2C:     &qotgetbasicqot.S2C{BasicQotList: quotes},
	}
	return c.reply(mo.ProtoQotGetBasicQot, sn, resp)
}

func (c *conn) onGetKL(ctx context.Context, sn uint32, body []byte) error {
	var req qotgetkl.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Qot_GetKL: %w", err)
	}
	c2s := req.GetC2S()
	kl := qotcommon.KLType(c2s.GetKlType())
	symbol := c2s.GetSecurity().GetCode()
	reqNum := int(c2s.GetReqNum())
	bars, err := c.srv.opts.Source.Bars(ctx, symbol, kl, time.Unix(0, 0).UTC(), c.srv.now())
	if err != nil {
		return fmt.Errorf("mock: source bars %s: %w", symbol, err)
	}
	// Qot_GetKL returns the most-recent reqNum bars.
	if reqNum > 0 && len(bars) > reqNum {
		bars = bars[len(bars)-reqNum:]
	}
	resp := &qotgetkl.Response{
		RetType: retOK(),
		S2C: &qotgetkl.S2C{
			Security: mo.SecurityForSymbol(symbol),
			KlList:   klinesFromBars(bars, kl),
		},
	}
	return c.reply(mo.ProtoQotGetKL, sn, resp)
}

func (c *conn) onRequestHistoryKL(ctx context.Context, sn uint32, body []byte) error {
	var req qotrequesthistorykl.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Qot_RequestHistoryKL: %w", err)
	}
	c2s := req.GetC2S()
	kl := qotcommon.KLType(c2s.GetKlType())
	symbol := c2s.GetSecurity().GetCode()
	begin, err := mo.ParseKLTime(c2s.GetBeginTime())
	if err != nil {
		return fmt.Errorf("mock: parse beginTime: %w", err)
	}
	end, err := mo.ParseKLTime(c2s.GetEndTime())
	if err != nil {
		return fmt.Errorf("mock: parse endTime: %w", err)
	}
	bars, err := c.srv.opts.Source.Bars(ctx, symbol, kl, begin, end)
	if err != nil {
		return fmt.Errorf("mock: source bars %s: %w", symbol, err)
	}
	resp := &qotrequesthistorykl.Response{
		RetType: retOK(),
		S2C: &qotrequesthistorykl.S2C{
			Security: mo.SecurityForSymbol(symbol),
			KlList:   klinesFromBars(bars, kl),
			// No paging: the mock returns the full window in one reply
			// (nextReqKey empty). A future enhancement could chunk it.
		},
	}
	return c.reply(mo.ProtoQotRequestHistoryKL, sn, resp)
}

// PushKLine pushes one or more bars to every connection that has registered a
// push for (symbol, kl), as a Qot_UpdateKL frame with serialNo 0. This is the
// controllable-clock driver: a test calls it per simulated tick to replay a
// stored day of bars as live updates. It returns the number of connections the
// push was delivered to.
func (s *Server) PushKLine(symbol string, kl qotcommon.KLType, bars []domain.Bar) (int, error) {
	s.mu.Lock()
	targets := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		c.subMu.Lock()
		reg := c.subs[mo.Subscription{Symbol: symbol, KLType: kl}]
		c.subMu.Unlock()
		if reg {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()

	resp := &qotupdatekl.Response{
		RetType: retOK(),
		S2C: &qotupdatekl.S2C{
			RehabType: proto.Int32(int32(qotcommon.RehabType_RehabType_Forward)),
			KlType:    proto.Int32(int32(kl)),
			Security:  mo.SecurityForSymbol(symbol),
			KlList:    klinesFromBars(bars, kl),
		},
	}
	frameBody, err := proto.Marshal(resp)
	if err != nil {
		return 0, fmt.Errorf("mock: marshal Qot_UpdateKL: %w", err)
	}
	frame := mo.EncodeFrame(mo.ProtoQotUpdateKL, 0, frameBody)
	n := 0
	for _, c := range targets {
		if err := c.writeFrame(frame); err != nil {
			s.log.Debug().Err(err).Msg("mock: push write failed")
			continue
		}
		n++
	}
	return n, nil
}
