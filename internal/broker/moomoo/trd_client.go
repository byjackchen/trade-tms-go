package moomoo

// trd_client.go implements the native Trd_* trading surface on *Client, making
// it satisfy TradeClient. It reuses the P5 framing/codec/roundTrip and the push
// demux exactly as the market-data surface, adding only the trade protocol IDs.
//
// Push demux: Trd_UpdateOrder (2208) and Trd_UpdateOrderFill (2218) are
// delivered to the registered TrdOrderHandler / TrdOrderFillHandler (the
// executor), NEVER to a serialNo waiter — symmetric with Qot_UpdateKL.
//
// Idempotent submission: PlaceOrder stamps the Trd_PlaceOrder remark with the
// caller's ClientOrderID and remembers ClientOrderID -> venueOrderID, so a
// retried PlaceOrder for a ClientOrderID we have already submitted returns the
// known venue id WITHOUT issuing a second order (reconnect/retry can never
// double-submit). A pushed update for that order can then correlate by remark
// (clientOrderID) even before the submit reply is processed.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/common"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetacclist"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetfunds"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetorderfilllist"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetorderlist"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetpositionlist"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdplaceorder"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdunlocktrade"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdupdateorder"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdupdateorderfill"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// trdState holds the trading-surface mutable state on a Client (push handlers +
// the idempotency map). Embedded into Client via a once-initialised pointer so
// the existing Client zero value keeps working for market-data-only callers.
type trdState struct {
	mu          sync.Mutex
	orderH      TrdOrderHandler
	fillH       TrdOrderFillHandler
	submittedCO map[string]string // clientOrderID -> venueOrderID (idempotency)
}

func (c *Client) trd() *trdState {
	c.trdOnce.Do(func() {
		c.trdSt = &trdState{submittedCO: make(map[string]string)}
		// Bridge the Options push hooks (set at construction by the executor) into
		// the registered handlers, so either wiring path works.
		if c.opts.OnTrdOrder != nil {
			c.trdSt.orderH = c.opts.OnTrdOrder
		}
		if c.opts.OnTrdOrderFill != nil {
			c.trdSt.fillH = c.opts.OnTrdOrderFill
		}
	})
	return c.trdSt
}

// SubscribeOrderUpdates registers the order-update push handler.
func (c *Client) SubscribeOrderUpdates(h TrdOrderHandler) error {
	st := c.trd()
	st.mu.Lock()
	st.orderH = h
	st.mu.Unlock()
	return nil
}

// SubscribeFillUpdates registers the fill push handler.
func (c *Client) SubscribeFillUpdates(h TrdOrderFillHandler) error {
	st := c.trd()
	st.mu.Lock()
	st.fillH = h
	st.mu.Unlock()
	return nil
}

// handleTrdOrderPush decodes a Trd_UpdateOrder push and invokes the handler.
func (c *Client) handleTrdOrderPush(frame Frame) {
	st := c.trd()
	st.mu.Lock()
	h := st.orderH
	st.mu.Unlock()
	if h == nil {
		return
	}
	var rsp trdupdateorder.Response
	if err := proto.Unmarshal(frame.Body, &rsp); err != nil {
		c.log.Error().Err(err).Msg("moomoo: decode Trd_UpdateOrder push")
		return
	}
	s2c := rsp.GetS2C()
	if s2c == nil || s2c.GetOrder() == nil {
		return
	}
	upd, err := orderUpdateFromTrd(s2c.GetOrder())
	if err != nil {
		c.log.Warn().Err(err).Msg("moomoo: skip bad Trd_UpdateOrder push")
		return
	}
	h(upd)
}

// handleTrdOrderFillPush decodes a Trd_UpdateOrderFill push and invokes the
// handler.
func (c *Client) handleTrdOrderFillPush(frame Frame) {
	st := c.trd()
	st.mu.Lock()
	h := st.fillH
	st.mu.Unlock()
	if h == nil {
		return
	}
	var rsp trdupdateorderfill.Response
	if err := proto.Unmarshal(frame.Body, &rsp); err != nil {
		c.log.Error().Err(err).Msg("moomoo: decode Trd_UpdateOrderFill push")
		return
	}
	s2c := rsp.GetS2C()
	if s2c == nil || s2c.GetOrderFill() == nil {
		return
	}
	fu, err := fillUpdateFromTrd(s2c.GetOrderFill())
	if err != nil {
		c.log.Warn().Err(err).Msg("moomoo: skip bad Trd_UpdateOrderFill push")
		return
	}
	h(fu)
}

// orderUpdateFromTrd normalises a trdcommon.Order into an OrderUpdate. The
// remark carries the client order id (set by PlaceOrder); fillQty / fillAvgPrice
// are cumulative.
func orderUpdateFromTrd(o *trdcommon.Order) (OrderUpdate, error) {
	side, err := sideFromTrd(o.GetTrdSide())
	if err != nil {
		return OrderUpdate{}, err
	}
	qty, err := domain.QtyFromFloat64Trunc(o.GetQty())
	if err != nil {
		return OrderUpdate{}, fmt.Errorf("moomoo: order %s qty: %w", venueID(o.GetOrderID(), o.GetOrderIDEx()), err)
	}
	dealt, err := domain.QtyFromFloat64Trunc(o.GetFillQty())
	if err != nil {
		return OrderUpdate{}, fmt.Errorf("moomoo: order %s fillQty: %w", venueID(o.GetOrderID(), o.GetOrderIDEx()), err)
	}
	avg, err := domain.PriceFromFloat64(o.GetFillAvgPrice())
	if err != nil {
		avg = domain.Price(0)
	}
	var updNs int64
	if ts, terr := trdInstant(o.GetUpdateTimestamp(), o.GetUpdateTime()); terr == nil {
		updNs = ts.UnixNano()
	}
	return OrderUpdate{
		ClientOrderID: o.GetRemark(),
		VenueOrderID:  venueID(o.GetOrderID(), o.GetOrderIDEx()),
		Symbol:        o.GetCode(),
		Side:          side,
		RawStatus:     o.GetOrderStatus(),
		OrderQty:      qty,
		DealtQty:      dealt,
		DealtAvgPrice: avg,
		LastErrMsg:    o.GetLastErrMsg(),
		UpdateTimeNs:  updNs,
	}, nil
}

// fillUpdateFromTrd normalises a trdcommon.OrderFill into a FillUpdate (per-
// execution qty/price).
func fillUpdateFromTrd(f *trdcommon.OrderFill) (FillUpdate, error) {
	side, err := sideFromTrd(f.GetTrdSide())
	if err != nil {
		return FillUpdate{}, err
	}
	qty, err := domain.QtyFromFloat64Trunc(f.GetQty())
	if err != nil {
		return FillUpdate{}, fmt.Errorf("moomoo: fill %s qty: %w", venueID(f.GetFillID(), f.GetFillIDEx()), err)
	}
	px, err := domain.PriceFromFloat64(f.GetPrice())
	if err != nil {
		px = domain.Price(0)
	}
	var updNs int64
	if ts, terr := trdInstant(f.GetUpdateTimestamp(), f.GetCreateTime()); terr == nil {
		updNs = ts.UnixNano()
	}
	return FillUpdate{
		FillID:       venueID(f.GetFillID(), f.GetFillIDEx()),
		VenueOrderID: venueID(f.GetOrderID(), f.GetOrderIDEx()),
		Symbol:       f.GetCode(),
		Side:         side,
		Qty:          qty,
		Price:        px,
		UpdateTimeNs: updNs,
	}, nil
}

// venueID prefers the string orderIDEx (the stable id moomoo now returns), else
// the legacy numeric id.
func venueID(num uint64, ex string) string {
	if ex != "" {
		return ex
	}
	if num == 0 {
		return ""
	}
	return fmt.Sprintf("%d", num)
}

// trdHeader builds a Trd_Common.TrdHeader for (env, accID) over the US market.
func trdHeader(env TrdEnv, accID uint64) *trdcommon.TrdHeader {
	return &trdcommon.TrdHeader{
		TrdEnv:    proto.Int32(int32(env)),
		AccID:     proto.Uint64(accID),
		TrdMarket: proto.Int32(TrdMarketUS),
	}
}

// packetID builds the anti-replay PacketID from the connection id + a fresh
// serial. moomoo requires it on write operations (PlaceOrder).
func (c *Client) packetID() *common.PacketID {
	c.mu.Lock()
	connID := uint64(0)
	if c.conn != nil {
		connID = c.conn.connID
	}
	c.mu.Unlock()
	return &common.PacketID{
		ConnID:   proto.Uint64(connID),
		SerialNo: proto.Uint32(c.nextSerial()),
	}
}

// GetAccList lists trading accounts for env (TradeClient).
func (c *Client) GetAccList(ctx context.Context, env TrdEnv) ([]TradeAccount, error) {
	req := &trdgetacclist.Request{C2S: &trdgetacclist.C2S{
		UserID:      proto.Uint64(0),
		TrdCategory: proto.Int32(int32(trdcommon.TrdCategory_TrdCategory_Security)),
		// Return CONSOLIDATED accounts too (综合账户, the HK/US/SG/AU account
		// system). Without this a user's funded margin/consolidated account is
		// omitted and only non-consolidated sub-accounts (often empty) are listed.
		NeedGeneralSecAccount: proto.Bool(true),
	}}
	body, err := c.roundTrip(ctx, ProtoTrdGetAccList, req)
	if err != nil {
		return nil, err
	}
	var rsp trdgetacclist.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return nil, fmt.Errorf("%w: decode Trd_GetAccList: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_GetAccList", rsp.RetType, rsp.RetMsg); cerr != nil {
		return nil, cerr
	}
	var out []TradeAccount
	for _, a := range rsp.GetS2C().GetAccList() {
		if TrdEnv(a.GetTrdEnv()) != env {
			continue
		}
		out = append(out, TradeAccount{
			AccID:        a.GetAccID(),
			TrdEnv:       TrdEnv(a.GetTrdEnv()),
			SecurityFirm: a.GetSecurityFirm(),
		})
	}
	return out, nil
}

// UnlockTrade unlocks the REAL account (no-op for SIMULATE) (TradeClient).
// securityFirm identifies the account's broker entity; OpenD requires it on a
// real unlock (else "missing required parameter securityFirm"). 0 omits it.
func (c *Client) UnlockTrade(ctx context.Context, env TrdEnv, password string, securityFirm int32) error {
	if env != TrdEnvReal {
		return nil // paper account requires no unlock
	}
	if password == "" {
		return fmt.Errorf("%w: UnlockTrade(REAL) requires a password", domain.ErrInvalidArgument)
	}
	sum := md5.Sum([]byte(password))
	c2s := &trdunlocktrade.C2S{
		Unlock: proto.Bool(true),
		PwdMD5: proto.String(hex.EncodeToString(sum[:])),
	}
	if securityFirm > 0 {
		c2s.SecurityFirm = proto.Int32(securityFirm)
	}
	req := &trdunlocktrade.Request{C2S: c2s}
	body, err := c.roundTrip(ctx, ProtoTrdUnlockTrade, req)
	if err != nil {
		return err
	}
	var rsp trdunlocktrade.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return fmt.Errorf("%w: decode Trd_UnlockTrade: %v", ErrProtocol, uerr)
	}
	return checkRet("Trd_UnlockTrade", rsp.RetType, rsp.RetMsg)
}

// PlaceOrder submits one order idempotently on req.ClientOrderID (TradeClient).
func (c *Client) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error) {
	if err := req.Validate(); err != nil {
		return PlaceOrderResult{}, err
	}
	st := c.trd()
	// Idempotency: a retry of a ClientOrderID we already submitted returns the
	// known venue id WITHOUT a second order.
	st.mu.Lock()
	if v, ok := st.submittedCO[req.ClientOrderID]; ok {
		st.mu.Unlock()
		return PlaceOrderResult{VenueOrderID: v}, nil
	}
	st.mu.Unlock()

	trdSide, err := trdSideForDomain(req.Side)
	if err != nil {
		return PlaceOrderResult{}, err
	}
	ot := int32(trdcommon.OrderType_OrderType_Market)
	if req.Type == domain.OrderTypeLimit {
		ot = int32(trdcommon.OrderType_OrderType_Normal)
	}
	tif := int32(trdcommon.TimeInForce_TimeInForce_GTC)
	if req.TIF == domain.TIFDay {
		tif = int32(trdcommon.TimeInForce_TimeInForce_DAY)
	}
	c2s := &trdplaceorder.C2S{
		PacketID:    c.packetID(),
		Header:      trdHeader(req.TrdEnv, req.AccID),
		TrdSide:     proto.Int32(trdSide),
		OrderType:   proto.Int32(ot),
		Code:        proto.String(req.Symbol),
		Qty:         proto.Float64(float64(req.Qty)),
		Price:       proto.Float64(req.Price.Float64()),
		SecMarket:   proto.Int32(TrdSecMarketUS),
		Remark:      proto.String(req.ClientOrderID),
		TimeInForce: proto.Int32(tif),
	}
	body, err := c.roundTrip(ctx, ProtoTrdPlaceOrder, &trdplaceorder.Request{C2S: c2s})
	if err != nil {
		return PlaceOrderResult{}, err
	}
	var rsp trdplaceorder.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return PlaceOrderResult{}, fmt.Errorf("%w: decode Trd_PlaceOrder: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_PlaceOrder", rsp.RetType, rsp.RetMsg); cerr != nil {
		// A retType!=0 on a PLACE is a venue BUSINESS rejection (the order was not
		// accepted: insufficient buying power, market closed, unknown symbol, ...).
		// Tag it with ErrOrderRejected so the manual-desk API surfaces it as a clean
		// 4xx (an expected operator outcome), not a 500 with a leaked protocol string
		// (finding 4). The reason text is preserved from the venue's retMsg.
		return PlaceOrderResult{}, fmt.Errorf("%w: %v", ErrOrderRejected, cerr)
	}
	vid := venueID(rsp.GetS2C().GetOrderID(), rsp.GetS2C().GetOrderIDEx())
	st.mu.Lock()
	st.submittedCO[req.ClientOrderID] = vid
	st.mu.Unlock()
	return PlaceOrderResult{VenueOrderID: vid}, nil
}

// CancelOrder requests a working order be cancelled at the broker (TradeClient).
//
// The cancel is a Trd_ModifyOrder(op=Cancel) round-trip; its compiled protobuf is
// NOT yet wired into this build, so the wire client returns ErrUnsupported rather
// than silently pretending to cancel. The manual desk surfaces this honestly (a
// 503/501 to the operator) instead of leaving a working real order the operator
// believes is cancelled. The in-memory MockVenue implements the full cancel path,
// so the manual-desk cancel lifecycle is proven against the deterministic gate.
//
// SAFETY: a no-op-but-success here would be a latent foot-gun on a real account
// (an operator told "cancelled" while the order keeps working); failing loud is
// the safe default until the modify-order proto is generated + conformance-tested.
func (c *Client) CancelOrder(_ context.Context, _ uint64, _ TrdEnv, _ string) error {
	return fmt.Errorf("%w: Trd_ModifyOrder (cancel) is not wired in this build", ErrUnsupported)
}

// GetOrderList returns current orders for (acc, env) (TradeClient).
func (c *Client) GetOrderList(ctx context.Context, accID uint64, env TrdEnv) ([]OrderUpdate, error) {
	req := &trdgetorderlist.Request{C2S: &trdgetorderlist.C2S{Header: trdHeader(env, accID)}}
	body, err := c.roundTrip(ctx, ProtoTrdGetOrderList, req)
	if err != nil {
		return nil, err
	}
	var rsp trdgetorderlist.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return nil, fmt.Errorf("%w: decode Trd_GetOrderList: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_GetOrderList", rsp.RetType, rsp.RetMsg); cerr != nil {
		return nil, cerr
	}
	var out []OrderUpdate
	for _, o := range rsp.GetS2C().GetOrderList() {
		upd, oerr := orderUpdateFromTrd(o)
		if oerr != nil {
			c.log.Warn().Err(oerr).Msg("moomoo: skip bad order in Trd_GetOrderList")
			continue
		}
		out = append(out, upd)
	}
	return out, nil
}

// GetPositionList returns broker positions for (acc, env) (TradeClient).
func (c *Client) GetPositionList(ctx context.Context, accID uint64, env TrdEnv) ([]BrokerPosition, error) {
	req := &trdgetpositionlist.Request{C2S: &trdgetpositionlist.C2S{Header: trdHeader(env, accID)}}
	body, err := c.roundTrip(ctx, ProtoTrdGetPositionList, req)
	if err != nil {
		return nil, err
	}
	var rsp trdgetpositionlist.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return nil, fmt.Errorf("%w: decode Trd_GetPositionList: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_GetPositionList", rsp.RetType, rsp.RetMsg); cerr != nil {
		return nil, cerr
	}
	var out []BrokerPosition
	for _, p := range rsp.GetS2C().GetPositionList() {
		bp, perr := brokerPositionFromTrd(p)
		if perr != nil {
			c.log.Warn().Err(perr).Msg("moomoo: skip bad position in Trd_GetPositionList")
			continue
		}
		out = append(out, bp)
	}
	return out, nil
}

// GetFunds returns the funds/buying-power snapshot for (acc, env) (TradeClient).
func (c *Client) GetFunds(ctx context.Context, accID uint64, env TrdEnv) (Funds, error) {
	// Currency is REQUIRED for consolidated (综合) accounts (OpenD rejects the
	// query with "missing required parameter currency" otherwise) and ignored for
	// others. The client trades US (TrdMarketUS header), so request USD.
	req := &trdgetfunds.Request{C2S: &trdgetfunds.C2S{
		Header:   trdHeader(env, accID),
		Currency: proto.Int32(int32(trdcommon.Currency_Currency_USD)),
	}}
	body, err := c.roundTrip(ctx, ProtoTrdGetFunds, req)
	if err != nil {
		return Funds{}, err
	}
	var rsp trdgetfunds.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return Funds{}, fmt.Errorf("%w: decode Trd_GetFunds: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_GetFunds", rsp.RetType, rsp.RetMsg); cerr != nil {
		return Funds{}, cerr
	}
	f := rsp.GetS2C().GetFunds()
	total, _ := domain.MoneyFromFloat64(f.GetTotalAssets())
	cash, _ := domain.MoneyFromFloat64(f.GetCash())
	avail, _ := domain.MoneyFromFloat64(f.GetAvailableFunds())
	mv, _ := domain.MoneyFromFloat64(f.GetMarketVal())
	return Funds{TotalAssets: total, Cash: cash, AvailableFunds: avail, MarketValue: mv}, nil
}

// GetOrderFillList returns historical fills for (acc, env) (TradeClient).
func (c *Client) GetOrderFillList(ctx context.Context, accID uint64, env TrdEnv) ([]FillUpdate, error) {
	req := &trdgetorderfilllist.Request{C2S: &trdgetorderfilllist.C2S{Header: trdHeader(env, accID)}}
	body, err := c.roundTrip(ctx, ProtoTrdGetOrderFillList, req)
	if err != nil {
		return nil, err
	}
	var rsp trdgetorderfilllist.Response
	if uerr := proto.Unmarshal(body, &rsp); uerr != nil {
		return nil, fmt.Errorf("%w: decode Trd_GetOrderFillList: %v", ErrProtocol, uerr)
	}
	if cerr := checkRet("Trd_GetOrderFillList", rsp.RetType, rsp.RetMsg); cerr != nil {
		return nil, cerr
	}
	var out []FillUpdate
	for _, f := range rsp.GetS2C().GetOrderFillList() {
		fu, ferr := fillUpdateFromTrd(f)
		if ferr != nil {
			c.log.Warn().Err(ferr).Msg("moomoo: skip bad fill in Trd_GetOrderFillList")
			continue
		}
		out = append(out, fu)
	}
	return out, nil
}

// TradeClient returns the native Trd_* trading surface (the client itself). It
// lets a caller that holds the *Client through a narrower market-data interface
// obtain the trading surface for paper/live execution.
func (c *Client) TradeClient() TradeClient { return c }

// Compile-time assertion: *Client implements TradeClient.
var _ TradeClient = (*Client)(nil)
