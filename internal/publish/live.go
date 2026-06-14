package publish

// live.go adds the paper/live trading Redis fan-out (P6 task 6): order, fill,
// position and account/buying-power updates the cockpit consumes. These are the
// hot mirror of the durable tms.{orders,fills,positions} tables — the cockpit
// reconstructs from PG on (re)connect and follows the streams live. Transport
// only: a publish failure is the caller's to swallow (PG is truth, decision 5).

import (
	"context"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Live trading topics (additive to the signal-mode data.* topics).
const (
	TopicLiveOrder    = "data.OrderUpdate"
	TopicLiveFill     = "data.FillUpdate"
	TopicLivePosition = "data.LivePositionUpdate"
	TopicLiveAccount  = "data.AccountUpdate"
)

// OrderEnvelope is the OrderUpdate wire shape for the cockpit blotter.
type OrderEnvelope struct {
	ClientOrderID string  `json:"client_order_id"`
	VenueOrderID  string  `json:"venue_order_id,omitempty"`
	StrategyID    string  `json:"strategy_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Qty           int64   `json:"qty"`
	FilledQty     int64   `json:"filled_qty"`
	AvgFillPx     float64 `json:"avg_fill_px"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason,omitempty"`
	TSEvent       int64   `json:"ts_event"`
	TSInit        int64   `json:"ts_init"`
}

// FillEnvelope is the FillUpdate wire shape.
type FillEnvelope struct {
	TradeID       string  `json:"trade_id"`
	ClientOrderID string  `json:"client_order_id"`
	VenueOrderID  string  `json:"venue_order_id,omitempty"`
	StrategyID    string  `json:"strategy_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Qty           int64   `json:"qty"`
	Price         float64 `json:"price"`
	Commission    float64 `json:"commission"`
	TSEvent       int64   `json:"ts_event"`
	TSInit        int64   `json:"ts_init"`
}

// LivePosition is one position row in the live position book snapshot.
type LivePosition struct {
	StrategyID  string  `json:"strategy_id"`
	Symbol      string  `json:"symbol"`
	SignedQty   int64   `json:"signed_qty"`
	AvgPx       float64 `json:"avg_px"`
	RealizedPnL float64 `json:"realized_pnl"`
}

// LivePositionEnvelope is the live position-book snapshot (the full book, not a
// delta — the cockpit replaces its book on each snapshot).
type LivePositionEnvelope struct {
	Positions []LivePosition `json:"positions"`
	TSEvent   int64          `json:"ts_event"`
	TSInit    int64          `json:"ts_init"`
}

// AccountEnvelope is the account/buying-power + day-PnL wire shape.
type AccountEnvelope struct {
	TotalAssets    float64 `json:"total_assets"`
	Cash           float64 `json:"cash"`
	AvailableFunds float64 `json:"available_funds"` // buying power
	MarketValue    float64 `json:"market_value"`
	DayPnL         float64 `json:"day_pnl"`
	TSEvent        int64   `json:"ts_event"`
	TSInit         int64   `json:"ts_init"`
}

// PublishOrder publishes one order-state update.
func (p *Publisher) PublishOrder(ctx context.Context, o domain.Order, filledQty domain.Qty, avgFillPx domain.Price, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	return p.publish(ctx, TopicLiveOrder, OrderEnvelope{
		ClientOrderID: o.ClientOrderID,
		VenueOrderID:  o.VenueOrderID,
		StrategyID:    o.StrategyID,
		Symbol:        o.Symbol,
		Side:          string(o.Side),
		Qty:           int64(o.Qty),
		FilledQty:     int64(filledQty),
		AvgFillPx:     avgFillPx.Float64(),
		Status:        string(o.Status),
		Reason:        o.Reason,
		TSEvent:       tsEventNS,
		TSInit:        p.nowNS(),
	})
}

// PublishFill publishes one execution.
func (p *Publisher) PublishFill(ctx context.Context, f domain.Fill) error {
	if p == nil {
		return nil
	}
	return p.publish(ctx, TopicLiveFill, FillEnvelope{
		TradeID:       f.TradeID,
		ClientOrderID: f.ClientOrderID,
		VenueOrderID:  f.VenueOrderID,
		StrategyID:    f.StrategyID,
		Symbol:        f.Symbol,
		Side:          string(f.Side),
		Qty:           int64(f.Qty),
		Price:         f.Price.Float64(),
		Commission:    f.Commission.Float64(),
		TSEvent:       f.TS.UTC().UnixNano(),
		TSInit:        p.nowNS(),
	})
}

// PublishLivePositions publishes the full live position-book snapshot.
func (p *Publisher) PublishLivePositions(ctx context.Context, positions []LivePosition, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	if positions == nil {
		positions = []LivePosition{}
	}
	return p.publish(ctx, TopicLivePosition, LivePositionEnvelope{
		Positions: positions,
		TSEvent:   tsEventNS,
		TSInit:    p.nowNS(),
	})
}

// PublishAccount publishes the account/buying-power + day-PnL snapshot.
func (p *Publisher) PublishAccount(ctx context.Context, env AccountEnvelope) error {
	if p == nil {
		return nil
	}
	if env.TSInit == 0 {
		env.TSInit = p.nowNS()
	}
	return p.publish(ctx, TopicLiveAccount, env)
}
