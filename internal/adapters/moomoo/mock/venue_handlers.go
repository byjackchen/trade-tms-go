package mock

// venue_handlers.go wires the mock trading venue's Trd_* request handlers and
// the two trading pushes into the mock OpenD connection. The request handlers
// mirror the parent package's client surface exactly, so the native trading
// client round-trips against them byte-for-byte.

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdgetacclist"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdgetfunds"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdgetorderlist"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdgetpositionlist"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdplaceorder"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdunlocktrade"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdupdateorder"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdupdateorderfill"
)

// retErr is a Common.RetType_Failed (-1) helper for venue rejections.
func retErr() *int32 { v := int32(-1); return &v }

// handleTrd dispatches a Trd_* request frame. Returns (handled, error): handled
// is false for non-trading protos so the caller falls through to its default.
func (c *conn) handleTrd(ctx context.Context, f mo.Frame) (bool, error) {
	sn := f.Header.SerialNo
	switch f.Header.ProtoID {
	case mo.ProtoTrdGetAccList:
		return true, c.onTrdGetAccList(sn, f.Body)
	case mo.ProtoTrdUnlockTrade:
		return true, c.onTrdUnlockTrade(sn, f.Body)
	case mo.ProtoTrdGetFunds:
		return true, c.onTrdGetFunds(sn, f.Body)
	case mo.ProtoTrdGetPositionList:
		return true, c.onTrdGetPositionList(sn, f.Body)
	case mo.ProtoTrdGetOrderList:
		return true, c.onTrdGetOrderList(sn, f.Body)
	case mo.ProtoTrdPlaceOrder:
		return true, c.onTrdPlaceOrder(ctx, sn, f.Body)
	default:
		return false, nil
	}
}

// venue returns the server's trade venue, or nil if trading is not enabled.
func (c *conn) venue() *tradeVenue { return c.srv.venue }

func (c *conn) onTrdGetAccList(sn uint32, body []byte) error {
	v := c.venue()
	resp := &trdgetacclist.Response{RetType: retOK(), S2C: &trdgetacclist.S2C{}}
	if v != nil {
		v.mu.Lock()
		for _, acc := range v.accounts {
			resp.S2C.AccList = append(resp.S2C.AccList, &trdcommon.TrdAcc{
				TrdEnv:            proto.Int32(int32(acc.env)),
				AccID:             proto.Uint64(acc.accID),
				TrdMarketAuthList: []int32{mo.TrdMarketUS},
				AccType:           proto.Int32(int32(acc.accType)),
				SecurityFirm:      proto.Int32(int32(trdcommon.SecurityFirm_SecurityFirm_FutuInc)),
			})
		}
		v.mu.Unlock()
	}
	return c.reply(mo.ProtoTrdGetAccList, sn, resp)
}

func (c *conn) onTrdUnlockTrade(sn uint32, body []byte) error {
	var req trdunlocktrade.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Trd_UnlockTrade: %w", err)
	}
	// The mock accepts any non-empty password on unlock (it never holds a real
	// secret); a lock (unlock=false) always succeeds.
	resp := &trdunlocktrade.Response{RetType: retOK(), S2C: &trdunlocktrade.S2C{}}
	if req.GetC2S().GetUnlock() && req.GetC2S().GetPwdMD5() == "" {
		resp.RetType = retErr()
		resp.RetMsg = proto.String("unlock requires a password")
	}
	return c.reply(mo.ProtoTrdUnlockTrade, sn, resp)
}

func (c *conn) onTrdGetFunds(sn uint32, body []byte) error {
	var req trdgetfunds.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Trd_GetFunds: %w", err)
	}
	accID := req.GetC2S().GetHeader().GetAccID()
	v := c.venue()
	resp := &trdgetfunds.Response{RetType: retOK(), S2C: &trdgetfunds.S2C{
		Header: req.GetC2S().GetHeader(),
	}}
	if v != nil {
		v.mu.Lock()
		acc := v.accounts[accID]
		if acc != nil {
			marketVal := 0.0
			for _, p := range acc.positions {
				marketVal += p.qty * p.costPrice
			}
			resp.S2C.Funds = &trdcommon.Funds{
				Power:             proto.Float64(acc.power),
				TotalAssets:       proto.Float64(acc.cash + marketVal),
				Cash:              proto.Float64(acc.cash),
				MarketVal:         proto.Float64(marketVal),
				FrozenCash:        proto.Float64(0),
				DebtCash:          proto.Float64(0),
				AvlWithdrawalCash: proto.Float64(acc.cash),
				// availableFunds mirrors power so both the futures-style and the
				// equity-style buying-power readers see the same value.
				AvailableFunds: proto.Float64(acc.power),
				Currency:       proto.Int32(int32(trdcommon.Currency_Currency_USD)),
			}
		}
		v.mu.Unlock()
	}
	if resp.S2C.Funds == nil {
		resp.RetType = retErr()
		resp.RetMsg = proto.String(fmt.Sprintf("unknown account %d", accID))
	}
	return c.reply(mo.ProtoTrdGetFunds, sn, resp)
}

func (c *conn) onTrdGetPositionList(sn uint32, body []byte) error {
	var req trdgetpositionlist.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Trd_GetPositionList: %w", err)
	}
	accID := req.GetC2S().GetHeader().GetAccID()
	v := c.venue()
	resp := &trdgetpositionlist.Response{RetType: retOK(), S2C: &trdgetpositionlist.S2C{
		Header: req.GetC2S().GetHeader(),
	}}
	if v != nil {
		v.mu.Lock()
		acc := v.accounts[accID]
		if acc != nil {
			for _, p := range acc.positions {
				if p.qty == 0 {
					continue
				}
				side := trdcommon.PositionSide_PositionSide_Long
				absQty := p.qty
				if p.qty < 0 {
					side = trdcommon.PositionSide_PositionSide_Short
					absQty = -p.qty
				}
				resp.S2C.PositionList = append(resp.S2C.PositionList, &trdcommon.Position{
					PositionID:       proto.Uint64(p.positionID),
					PositionSide:     proto.Int32(int32(side)),
					Code:             proto.String(p.symbol),
					Name:             proto.String(p.symbol),
					Qty:              proto.Float64(absQty),
					CanSellQty:       proto.Float64(absQty),
					Price:            proto.Float64(p.costPrice),
					CostPrice:        proto.Float64(p.costPrice),
					AverageCostPrice: proto.Float64(p.costPrice),
					Val:              proto.Float64(absQty * p.costPrice),
					PlVal:            proto.Float64(0),
					TrdMarket:        proto.Int32(mo.TrdMarketUS),
					Currency:         proto.Int32(int32(trdcommon.Currency_Currency_USD)),
				})
			}
		}
		v.mu.Unlock()
	}
	return c.reply(mo.ProtoTrdGetPositionList, sn, resp)
}

func (c *conn) onTrdGetOrderList(sn uint32, body []byte) error {
	var req trdgetorderlist.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Trd_GetOrderList: %w", err)
	}
	accID := req.GetC2S().GetHeader().GetAccID()
	v := c.venue()
	resp := &trdgetorderlist.Response{RetType: retOK(), S2C: &trdgetorderlist.S2C{
		Header: req.GetC2S().GetHeader(),
	}}
	if v != nil {
		v.mu.Lock()
		for _, orders := range v.workingBySymbol {
			for _, o := range orders {
				if o.accID != accID {
					continue
				}
				resp.S2C.OrderList = append(resp.S2C.OrderList,
					orderToTrd(o, o.createTS, trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0, ""))
			}
		}
		v.mu.Unlock()
	}
	return c.reply(mo.ProtoTrdGetOrderList, sn, resp)
}

// onTrdPlaceOrder accepts or rejects an order per the documented model. On
// accept it replies with the venue orderID, pushes an immediate
// Trd_UpdateOrder(Submitted), and queues the order for a next-bar fill. On a
// deterministic reject it replies retType!=0 and pushes a terminal
// Trd_UpdateOrder(SubmitFailed).
func (c *conn) onTrdPlaceOrder(ctx context.Context, sn uint32, body []byte) error {
	var req trdplaceorder.Request
	if err := proto.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("mock: decode Trd_PlaceOrder: %w", err)
	}
	c2s := req.GetC2S()
	accID := c2s.GetHeader().GetAccID()
	symbol := c2s.GetCode()
	side := trdcommon.TrdSide(c2s.GetTrdSide())
	qty := c2s.GetQty()
	clientOrderID := c2s.GetRemark()

	v := c.venue()
	if v == nil {
		resp := &trdplaceorder.Response{RetType: retErr(), RetMsg: proto.String("trading not enabled")}
		return c.reply(mo.ProtoTrdPlaceOrder, sn, resp)
	}

	// Validate + classify a possible deterministic rejection.
	if reason, ok := c.venueRejectReason(ctx, v, accID, symbol, side, qty); ok {
		return c.rejectOrder(sn, v, mo.TrdEnv(c2s.GetHeader().GetTrdEnv()), accID, symbol, side, qty, clientOrderID, reason)
	}

	// Accept: assign a venue order id, reply, push Submitted, queue for fill.
	v.mu.Lock()
	orderID := v.nextIDLocked()
	o := &venueOrder{
		orderID:       orderID,
		clientOrderID: clientOrderID,
		env:           mo.TrdEnv(c2s.GetHeader().GetTrdEnv()),
		accID:         accID,
		symbol:        symbol,
		side:          side,
		qty:           qty,
		createTS:      c.srv.now(),
	}
	v.workingBySymbol[symbol] = append(v.workingBySymbol[symbol], o)
	v.mu.Unlock()

	resp := &trdplaceorder.Response{RetType: retOK(), S2C: &trdplaceorder.S2C{
		Header:    c2s.GetHeader(),
		OrderID:   proto.Uint64(orderID),
		OrderIDEx: proto.String(fmt.Sprintf("%d", orderID)),
	}}
	if err := c.reply(mo.ProtoTrdPlaceOrder, sn, resp); err != nil {
		return err
	}
	// Immediate accepted push (status Submitted) — mirrors a real venue.
	return c.pushTrdOrder(o, o.createTS, trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0, "")
}

// venueRejectReason returns a deterministic rejection reason if the order must
// be rejected (unknown symbol / insufficient buying power / market closed).
func (c *conn) venueRejectReason(ctx context.Context, v *tradeVenue, accID uint64, symbol string, side trdcommon.TrdSide, qty float64) (VenueRejectReason, bool) {
	v.mu.Lock()
	closed := v.marketClosed
	acc := v.accounts[accID]
	v.mu.Unlock()
	if closed {
		return RejectMarketClosed, true
	}
	if acc == nil {
		return RejectUnknownSymbol, true // unknown account collapses to a reject
	}
	// Unknown symbol: not present in the BarSource (no price to fill against).
	price := c.latestClose(ctx, symbol)
	if price <= 0 {
		return RejectUnknownSymbol, true
	}
	// Insufficient buying power on a BUY: notional exceeds available power.
	if side == trdcommon.TrdSide_TrdSide_Buy {
		v.mu.Lock()
		power := acc.power
		v.mu.Unlock()
		if qty*price > power {
			return RejectInsufficientBP, true
		}
	}
	return "", false
}

// latestClose returns the most-recent daily close for symbol from the bar
// source, or 0 if the symbol is unknown / has no bars.
func (c *conn) latestClose(ctx context.Context, symbol string) float64 {
	bars, err := c.srv.opts.Source.Bars(ctx, symbol, qotcommon.KLType_KLType_Day, time.Unix(0, 0).UTC(), c.srv.now())
	if err != nil || len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close.Float64()
}

// rejectOrder replies retType!=0 (errCode set, NO s2c — exactly as a real
// FutuOpenD signals a rejected write op) and pushes a terminal SubmitFailed
// Trd_UpdateOrder so the executor records the terminal state. The reply omits
// s2c (whose header is a required field) to stay proto2-valid without inventing
// a header on a rejection.
func (c *conn) rejectOrder(sn uint32, v *tradeVenue, env mo.TrdEnv, accID uint64, symbol string, side trdcommon.TrdSide, qty float64, clientOrderID string, reason VenueRejectReason) error {
	v.mu.Lock()
	orderID := v.nextIDLocked()
	v.mu.Unlock()
	o := &venueOrder{
		orderID:       orderID,
		clientOrderID: clientOrderID,
		env:           env,
		accID:         accID,
		symbol:        symbol,
		side:          side,
		qty:           qty,
		createTS:      c.srv.now(),
	}
	resp := &trdplaceorder.Response{
		RetType: retErr(),
		RetMsg:  proto.String(string(reason)),
		ErrCode: proto.Int32(1),
	}
	if err := c.reply(mo.ProtoTrdPlaceOrder, sn, resp); err != nil {
		return err
	}
	return c.pushTrdOrder(o, o.createTS, trdcommon.OrderStatus_OrderStatus_SubmitFailed, 0, 0, string(reason))
}

// pushTrdOrder writes a Trd_UpdateOrder push (serialNo 0) for one order.
func (c *conn) pushTrdOrder(o *venueOrder, t time.Time, status trdcommon.OrderStatus, fillQty, fillAvg float64, errMsg string) error {
	resp := &trdupdateorder.Response{
		RetType: retOK(),
		S2C: &trdupdateorder.S2C{
			Header: &trdcommon.TrdHeader{
				TrdEnv:    proto.Int32(int32(o.env)),
				AccID:     proto.Uint64(o.accID),
				TrdMarket: proto.Int32(mo.TrdMarketUS),
			},
			Order: orderToTrd(o, t, status, fillQty, fillAvg, errMsg),
		},
	}
	bodyB, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("mock: marshal Trd_UpdateOrder: %w", err)
	}
	return c.writeFrame(mo.EncodeFrame(mo.ProtoTrdUpdateOrder, 0, bodyB))
}

// pushTrdFill writes a Trd_UpdateOrderFill push (serialNo 0) for one execution.
func (c *conn) pushTrdFill(o *venueOrder, fillID uint64, price float64, t time.Time) error {
	resp := &trdupdateorderfill.Response{
		RetType: retOK(),
		S2C: &trdupdateorderfill.S2C{
			Header: &trdcommon.TrdHeader{
				TrdEnv:    proto.Int32(int32(o.env)),
				AccID:     proto.Uint64(o.accID),
				TrdMarket: proto.Int32(mo.TrdMarketUS),
			},
			OrderFill: fillToTrd(o, fillID, price, t),
		},
	}
	bodyB, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("mock: marshal Trd_UpdateOrderFill: %w", err)
	}
	return c.writeFrame(mo.EncodeFrame(mo.ProtoTrdUpdateOrderFill, 0, bodyB))
}
