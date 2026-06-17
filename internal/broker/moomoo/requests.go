package moomoo

// requests.go implements the typed P5 request surface on top of Client's
// roundTrip: the session handshake, heartbeat, subscription management, and the
// market-data pulls (history K-line, cached K-line, basic quotes, sub-info).

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/getglobalstate"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/initconnect"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/keepalive"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotgetbasicqot"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotgetkl"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotgetsubinfo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotregqotpush"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotrequesthistorykl"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotsub"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// checkRet inspects a moomoo Response's retType (0 = success per
// Common.RetType_Succeed). proto2 retType is a pointer; absence is treated as
// failure to be safe.
func checkRet(proto2 string, retType *int32, retMsg *string) error {
	if retType == nil {
		return fmt.Errorf("%w: %s missing retType", ErrProtocol, proto2)
	}
	if *retType != 0 {
		msg := ""
		if retMsg != nil {
			msg = *retMsg
		}
		return fmt.Errorf("%w: %s retType=%d retMsg=%q", ErrProtocol, proto2, *retType, msg)
	}
	return nil
}

// handshake performs InitConnect on a freshly dialed connection and records the
// server's connID and keepAliveInterval. It is sent through roundTrip so the
// reply is correlated like any other request.
func (c *Client) handshake(ctx context.Context, cs *connState) error {
	req := &initconnect.Request{
		C2S: &initconnect.C2S{
			ClientVer: proto.Int32(ClientVer),
			ClientID:  proto.String(fmt.Sprintf("%s-%d", clientIDPrefix, time.Now().UnixNano())),
			// recvNotify=true so OpenD pushes market-state changes on this
			// connection (we register K-line pushes via Qot_Sub regardless).
			RecvNotify: proto.Bool(true),
			// pushProtoFmt=0 (Protobuf): keep pushes in the same wire format.
			PushProtoFmt:        proto.Int32(int32(protoFmtProtobuf)),
			ProgrammingLanguage: proto.String("Go"),
		},
	}
	body, err := c.roundTrip(ctx, ProtoInitConnect, req)
	if err != nil {
		return err
	}
	var rsp initconnect.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return fmt.Errorf("%w: decode InitConnect: %v", ErrProtocol, err)
	}
	if err := checkRet("InitConnect", rsp.RetType, rsp.RetMsg); err != nil {
		return err
	}
	s2c := rsp.GetS2C()
	if s2c == nil {
		return fmt.Errorf("%w: InitConnect missing s2c", ErrProtocol)
	}
	cs.connID = s2c.GetConnID()
	cs.kaSec = int(s2c.GetKeepAliveInterval())
	if cs.kaSec <= 0 {
		cs.kaSec = 10 // SDK default
	}
	return nil
}

// keepAliveLoop sends KeepAlive at the server-advertised interval until ctx is
// cancelled or a send fails (which tears down the connection by closing it,
// surfacing through the reader).
func (c *Client) keepAliveLoop(ctx context.Context, cs *connState) {
	interval := time.Duration(cs.kaSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cs.done:
			return
		case <-t.C:
			kaCtx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
			err := c.sendKeepAlive(kaCtx)
			cancel()
			if err != nil {
				c.log.Warn().Err(err).Msg("moomoo: keepalive failed; closing connection")
				_ = cs.conn.Close() // reader observes EOF -> reconnect
				return
			}
		}
	}
}

// sendKeepAlive issues one KeepAlive round trip.
func (c *Client) sendKeepAlive(ctx context.Context) error {
	req := &keepalive.Request{
		C2S: &keepalive.C2S{Time: proto.Int64(time.Now().Unix())},
	}
	body, err := c.roundTrip(ctx, ProtoKeepAlive, req)
	if err != nil {
		return err
	}
	var rsp keepalive.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return fmt.Errorf("%w: decode KeepAlive: %v", ErrProtocol, err)
	}
	return checkRet("KeepAlive", rsp.RetType, rsp.RetMsg)
}

// GlobalState is a decoded subset of GetGlobalState relevant to the live
// runner: per-market state + whether the quote server is logged in + server
// time.
type GlobalState struct {
	MarketUS   qotcommon.QotMarketState
	QotLogined bool
	TrdLogined bool
	ServerVer  int32
	ServerTime time.Time
	ConnID     uint64
}

// GetGlobalState pulls OpenD's global/market state.
func (c *Client) GetGlobalState(ctx context.Context) (GlobalState, error) {
	req := &getglobalstate.Request{
		C2S: &getglobalstate.C2S{UserID: proto.Uint64(0)}, // userID deprecated, 0
	}
	body, err := c.roundTrip(ctx, ProtoGetGlobalState, req)
	if err != nil {
		return GlobalState{}, err
	}
	var rsp getglobalstate.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return GlobalState{}, fmt.Errorf("%w: decode GetGlobalState: %v", ErrProtocol, err)
	}
	if err := checkRet("GetGlobalState", rsp.RetType, rsp.RetMsg); err != nil {
		return GlobalState{}, err
	}
	s2c := rsp.GetS2C()
	if s2c == nil {
		return GlobalState{}, fmt.Errorf("%w: GetGlobalState missing s2c", ErrProtocol)
	}
	return GlobalState{
		MarketUS:   qotcommon.QotMarketState(s2c.GetMarketUS()),
		QotLogined: s2c.GetQotLogined(),
		TrdLogined: s2c.GetTrdLogined(),
		ServerVer:  s2c.GetServerVer(),
		ServerTime: time.Unix(s2c.GetTime(), 0).UTC(),
		ConnID:     s2c.GetConnID(),
	}, nil
}

// Subscribe subscribes the given symbols at the given K-line type and registers
// the push on this connection (isRegOrUnRegPush=true). The subscription set is
// remembered and replayed on reconnect. It enforces the per-connection cap.
func (c *Client) Subscribe(ctx context.Context, symbols []string, kl qotcommon.KLType) error {
	subType, err := SubTypeForKLType(kl)
	if err != nil {
		return err
	}
	if err := c.reserveQuota(symbols, kl); err != nil {
		return err
	}
	if err := c.sendSub(ctx, symbols, subType, true /*sub*/, true /*regPush*/); err != nil {
		// Roll back the reservation on failure so a retry is not double-counted.
		c.releaseQuota(symbols, kl)
		return err
	}
	return nil
}

// Unsubscribe removes the given symbols at the given K-line type.
func (c *Client) Unsubscribe(ctx context.Context, symbols []string, kl qotcommon.KLType) error {
	subType, err := SubTypeForKLType(kl)
	if err != nil {
		return err
	}
	if err := c.sendSub(ctx, symbols, subType, false /*unsub*/, false); err != nil {
		return err
	}
	c.releaseQuota(symbols, kl)
	return nil
}

// reserveQuota records subscriptions, rejecting if the cap would be exceeded.
func (c *Client) reserveQuota(symbols []string, kl qotcommon.KLType) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Count distinct new ones.
	add := 0
	for _, s := range symbols {
		if _, ok := c.subs[Subscription{Symbol: s, KLType: kl}]; !ok {
			add++
		}
	}
	if len(c.subs)+add > c.opts.MaxSubscriptions {
		return fmt.Errorf("%w: subscribing %d would exceed cap %d (current %d)",
			domain.ErrInvalidArgument, add, c.opts.MaxSubscriptions, len(c.subs))
	}
	for _, s := range symbols {
		c.subs[Subscription{Symbol: s, KLType: kl}] = struct{}{}
	}
	return nil
}

func (c *Client) releaseQuota(symbols []string, kl qotcommon.KLType) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range symbols {
		delete(c.subs, Subscription{Symbol: s, KLType: kl})
	}
}

// Subscriptions returns a snapshot of the current subscription set.
func (c *Client) Subscriptions() []Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Subscription, 0, len(c.subs))
	for s := range c.subs {
		out = append(out, s)
	}
	return out
}

// sendSub issues a Qot_Sub for one (subType) over the given symbols.
func (c *Client) sendSub(ctx context.Context, symbols []string, subType qotcommon.SubType, sub, regPush bool) error {
	secs := make([]*qotcommon.Security, 0, len(symbols))
	for _, s := range symbols {
		secs = append(secs, SecurityForSymbol(s))
	}
	req := &qotsub.Request{
		C2S: &qotsub.C2S{
			SecurityList:         secs,
			SubTypeList:          []int32{int32(subType)},
			IsSubOrUnSub:         proto.Bool(sub),
			IsRegOrUnRegPush:     proto.Bool(regPush),
			RegPushRehabTypeList: []int32{int32(qotcommon.RehabType_RehabType_Forward)},
			IsFirstPush:          proto.Bool(true),
		},
	}
	body, err := c.roundTrip(ctx, ProtoQotSub, req)
	if err != nil {
		return err
	}
	var rsp qotsub.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return fmt.Errorf("%w: decode Qot_Sub: %v", ErrProtocol, err)
	}
	return checkRet("Qot_Sub", rsp.RetType, rsp.RetMsg)
}

// resubscribe replays the remembered subscription set after a reconnect,
// grouped by K-line type to minimize round trips. Best-effort: errors are
// logged, not fatal.
func (c *Client) resubscribe(ctx context.Context) {
	c.mu.Lock()
	byKL := make(map[qotcommon.KLType][]string)
	for s := range c.subs {
		byKL[s.KLType] = append(byKL[s.KLType], s.Symbol)
	}
	c.mu.Unlock()
	for kl, syms := range byKL {
		subType, err := SubTypeForKLType(kl)
		if err != nil {
			continue
		}
		if err := c.sendSub(ctx, syms, subType, true, true); err != nil {
			c.log.Warn().Err(err).Int("count", len(syms)).Msg("moomoo: re-subscribe failed")
		} else {
			c.log.Info().Int("count", len(syms)).Str("klType", kl.String()).Msg("moomoo: re-subscribed")
		}
	}
}

// RegisterPush (Qot_RegQotPush) registers or unregisters the push of an
// already-subscribed type on this connection. Subscribe already sets the push
// flag; this is exposed for callers that subscribe and register separately.
func (c *Client) RegisterPush(ctx context.Context, symbols []string, kl qotcommon.KLType, reg bool) error {
	subType, err := SubTypeForKLType(kl)
	if err != nil {
		return err
	}
	secs := make([]*qotcommon.Security, 0, len(symbols))
	for _, s := range symbols {
		secs = append(secs, SecurityForSymbol(s))
	}
	req := &qotregqotpush.Request{
		C2S: &qotregqotpush.C2S{
			SecurityList:  secs,
			SubTypeList:   []int32{int32(subType)},
			RehabTypeList: []int32{int32(qotcommon.RehabType_RehabType_Forward)},
			IsRegOrUnReg:  proto.Bool(reg),
			IsFirstPush:   proto.Bool(true),
		},
	}
	body, err := c.roundTrip(ctx, ProtoQotRegQotPush, req)
	if err != nil {
		return err
	}
	var rsp qotregqotpush.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return fmt.Errorf("%w: decode Qot_RegQotPush: %v", ErrProtocol, err)
	}
	return checkRet("Qot_RegQotPush", rsp.RetType, rsp.RetMsg)
}

// SubInfo is the decoded Qot_GetSubInfo quota view.
type SubInfo struct {
	TotalUsedQuota int32
	RemainQuota    int32
}

// GetSubInfo queries OpenD's current subscription quota.
func (c *Client) GetSubInfo(ctx context.Context) (SubInfo, error) {
	req := &qotgetsubinfo.Request{
		C2S: &qotgetsubinfo.C2S{IsReqAllConn: proto.Bool(false)},
	}
	body, err := c.roundTrip(ctx, ProtoQotGetSubInfo, req)
	if err != nil {
		return SubInfo{}, err
	}
	var rsp qotgetsubinfo.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return SubInfo{}, fmt.Errorf("%w: decode Qot_GetSubInfo: %v", ErrProtocol, err)
	}
	if err := checkRet("Qot_GetSubInfo", rsp.RetType, rsp.RetMsg); err != nil {
		return SubInfo{}, err
	}
	s2c := rsp.GetS2C()
	return SubInfo{
		TotalUsedQuota: s2c.GetTotalUsedQuota(),
		RemainQuota:    s2c.GetRemainQuota(),
	}, nil
}

// RequestHistoryKL pulls historical K-lines for one symbol over [begin, end]
// (inclusive), following moomoo's nextReqKey paging until exhausted. begin/end
// are UTC; they are rendered to moomoo's NY-local date strings. The returned
// bars are ordered as the server returns them (ascending time).
func (c *Client) RequestHistoryKL(ctx context.Context, symbol string, kl qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error) {
	beginStr := FormatKLTime(begin, kl)
	endStr := FormatKLTime(end, kl)
	var out []domain.Bar
	var nextKey []byte
	for {
		c2s := &qotrequesthistorykl.C2S{
			RehabType: proto.Int32(int32(qotcommon.RehabType_RehabType_Forward)),
			KlType:    proto.Int32(int32(kl)),
			Security:  SecurityForSymbol(symbol),
			BeginTime: proto.String(beginStr),
			EndTime:   proto.String(endStr),
		}
		if nextKey != nil {
			c2s.NextReqKey = nextKey
		}
		req := &qotrequesthistorykl.Request{C2S: c2s}
		body, err := c.roundTrip(ctx, ProtoQotRequestHistoryKL, req)
		if err != nil {
			return nil, err
		}
		var rsp qotrequesthistorykl.Response
		if err := proto.Unmarshal(body, &rsp); err != nil {
			return nil, fmt.Errorf("%w: decode Qot_RequestHistoryKL: %v", ErrProtocol, err)
		}
		if err := checkRet("Qot_RequestHistoryKL", rsp.RetType, rsp.RetMsg); err != nil {
			return nil, err
		}
		s2c := rsp.GetS2C()
		if s2c == nil {
			break
		}
		for _, k := range s2c.GetKlList() {
			if k.GetIsBlank() {
				continue
			}
			b, err := BarFromKLine(symbol, kl, k)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		nextKey = s2c.GetNextReqKey()
		if len(nextKey) == 0 {
			break
		}
	}
	return out, nil
}

// GetKL pulls the most recent reqNum cached K-lines for one symbol (Qot_GetKL).
// Unlike RequestHistoryKL this requires a prior Subscribe of the matching type
// (OpenD serves it from the subscription cache).
func (c *Client) GetKL(ctx context.Context, symbol string, kl qotcommon.KLType, reqNum int32) ([]domain.Bar, error) {
	req := &qotgetkl.Request{
		C2S: &qotgetkl.C2S{
			RehabType: proto.Int32(int32(qotcommon.RehabType_RehabType_Forward)),
			KlType:    proto.Int32(int32(kl)),
			Security:  SecurityForSymbol(symbol),
			ReqNum:    proto.Int32(reqNum),
		},
	}
	body, err := c.roundTrip(ctx, ProtoQotGetKL, req)
	if err != nil {
		return nil, err
	}
	var rsp qotgetkl.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return nil, fmt.Errorf("%w: decode Qot_GetKL: %v", ErrProtocol, err)
	}
	if err := checkRet("Qot_GetKL", rsp.RetType, rsp.RetMsg); err != nil {
		return nil, err
	}
	s2c := rsp.GetS2C()
	out := make([]domain.Bar, 0, len(s2c.GetKlList()))
	for _, k := range s2c.GetKlList() {
		if k.GetIsBlank() {
			continue
		}
		b, err := BarFromKLine(symbol, kl, k)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// BasicQuote is a decoded subset of BasicQot (latest price snapshot).
type BasicQuote struct {
	Symbol      string
	CurPrice    domain.Price
	OpenPrice   domain.Price
	HighPrice   domain.Price
	LowPrice    domain.Price
	LastClose   domain.Price
	Volume      int64
	IsSuspended bool
	UpdateTime  string
}

// GetBasicQot pulls latest basic quotes for the given symbols (Qot_GetBasicQot).
func (c *Client) GetBasicQot(ctx context.Context, symbols []string) ([]BasicQuote, error) {
	secs := make([]*qotcommon.Security, 0, len(symbols))
	for _, s := range symbols {
		secs = append(secs, SecurityForSymbol(s))
	}
	req := &qotgetbasicqot.Request{C2S: &qotgetbasicqot.C2S{SecurityList: secs}}
	body, err := c.roundTrip(ctx, ProtoQotGetBasicQot, req)
	if err != nil {
		return nil, err
	}
	var rsp qotgetbasicqot.Response
	if err := proto.Unmarshal(body, &rsp); err != nil {
		return nil, fmt.Errorf("%w: decode Qot_GetBasicQot: %v", ErrProtocol, err)
	}
	if err := checkRet("Qot_GetBasicQot", rsp.RetType, rsp.RetMsg); err != nil {
		return nil, err
	}
	s2c := rsp.GetS2C()
	out := make([]BasicQuote, 0, len(s2c.GetBasicQotList()))
	for _, q := range s2c.GetBasicQotList() {
		bq := BasicQuote{
			Symbol:      SymbolForSecurity(q.GetSecurity()),
			Volume:      q.GetVolume(),
			IsSuspended: q.GetIsSuspended(),
			UpdateTime:  q.GetUpdateTime(),
		}
		bq.CurPrice, _ = domain.PriceFromFloat64(q.GetCurPrice())
		bq.OpenPrice, _ = domain.PriceFromFloat64(q.GetOpenPrice())
		bq.HighPrice, _ = domain.PriceFromFloat64(q.GetHighPrice())
		bq.LowPrice, _ = domain.PriceFromFloat64(q.GetLowPrice())
		bq.LastClose, _ = domain.PriceFromFloat64(q.GetLastClosePrice())
		out = append(out, bq)
	}
	return out, nil
}
