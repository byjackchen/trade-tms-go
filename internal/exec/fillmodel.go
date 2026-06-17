package exec

// fillmodel.go defines the swappable fill model: the single documented rule for
// WHEN a market order fills and at WHAT price(s), given the bar it is evaluated
// against (locked decision 3). A fill may break into several price LEGS, so the
// model returns a slice of FillLeg rather than one price.
//
// Two profiles ship:
//
//   - close-fill (the same-bar close-fill profile): a market order
//     submitted in on_bar(T) fills within bar T (same ts) against the bar's
//     decomposed LAST-price ticks. The matching posts the bar's CLOSE
//     tick with the depth `close_tick_vol = compute_bar_quarter_sizes(volume)`
//     (quarter = max(volume//4, 1); close = (3*quarter >= volume) ? 1 :
//     volume - 3*quarter — see closeTickVolume below). The
//     marketable order consumes that depth at the close price; any RESIDUAL
//     quantity walks one price increment adverse to the order side
//     (BUY -> close + increment, SELL -> close - increment) — the L1 book has a
//     single level, so the rest fills at exactly one increment away:
//       qty<=close_tick_vol            -> all at close;
//       qty> close_tick_vol            -> close_tick_vol at close, rest at
//                                         close±increment.
//     Zero slippage parameter, zero commission (zero-fee equity, §7.1). This is
//     the zero-cost deterministic GATE profile.
//
//   - realistic (the production default, [IMPROVE]): fills the WHOLE order at
//     the NEXT bar's open with a configurable slippage (bps, adverse) and a
//     configurable commission (per-share or bps). No volume model — a single
//     leg — because next-open execution is the conservative, non-look-ahead
//     behavior a live system actually gets.

import (
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// PriceIncrement is the equity price increment (tick size): 0.01
// (price_precision=2, price_increment=0.01, §7.2). It is the increment the
// close-fill model walks residual quantity by.
var PriceIncrement = domain.MustPrice("0.01")

// FillTiming says when a pending order fills relative to bars.
type FillTiming uint8

const (
	// FillThisBar fills against the bar currently being processed (close-fill:
	// same bar as submission).
	FillThisBar FillTiming = iota
	// FillNextBar defers the fill to the next bar for the same symbol
	// (realistic: next-open).
	FillNextBar
)

// FillLeg is one priced portion of an order's execution. An order may fill in
// several legs at increasing/decreasing prices (depth walking).
type FillLeg struct {
	Qty   domain.Qty // positive
	Price domain.Price
}

// FillModel decides the fill legs and timing for an order against a bar. It is
// pure (no state) so it is trivially deterministic and swappable.
type FillModel interface {
	// Name identifies the profile (for logs / run metadata).
	Name() string
	// Timing reports whether orders fill on the submission bar or the next bar.
	Timing() FillTiming
	// Fill returns the price legs for order against bar (the legs sum to
	// order.Qty). For next-bar models, bar is the NEXT bar being filled against.
	Fill(order domain.Order, bar domain.Bar) ([]FillLeg, error)
	// Commission returns the commission for filling qty shares at price.
	Commission(qty domain.Qty, price domain.Price) (domain.Money, error)
}

// ---------------------------------------------------------------------------
// close-fill
// ---------------------------------------------------------------------------

// CloseFillModel is the zero-cost deterministic profile: fill against the
// bar's close-tick depth at the close price, residual one increment adverse,
// same bar, no commission.
type CloseFillModel struct{}

// Name returns "close-fill".
func (CloseFillModel) Name() string { return "close-fill" }

// Timing returns FillThisBar.
func (CloseFillModel) Timing() FillTiming { return FillThisBar }

// Fill returns the close-tick depth at the bar close plus a residual leg one
// price increment adverse to the side. Non-market orders are rejected (the
// profile models market orders only).
func (CloseFillModel) Fill(order domain.Order, bar domain.Bar) ([]FillLeg, error) {
	if order.Type != domain.OrderTypeMarket {
		return nil, fmt.Errorf("%w: close-fill fills MARKET orders only, got %s",
			domain.ErrInvalidArgument, order.Type)
	}
	if order.Qty <= 0 {
		return nil, fmt.Errorf("%w: close-fill order %s non-positive qty %d",
			domain.ErrInvalidArgument, order.ClientOrderID, order.Qty)
	}
	closeTick := closeTickVolume(bar.Volume)
	// When the bar has no volume, the whole order fills at the close (there is
	// no depth to exhaust; a degenerate zero-volume bar should not slip).
	if closeTick <= 0 {
		return []FillLeg{{Qty: order.Qty, Price: bar.Close}}, nil
	}
	if int64(order.Qty) <= closeTick {
		return []FillLeg{{Qty: order.Qty, Price: bar.Close}}, nil
	}
	// First leg: close-tick depth at the close price.
	atClose := domain.Qty(closeTick)
	residual := order.Qty - atClose
	residualPx, err := residualPrice(bar.Close, order.Side)
	if err != nil {
		return nil, err
	}
	return []FillLeg{
		{Qty: atClose, Price: bar.Close},
		{Qty: residual, Price: residualPx},
	}, nil
}

// Commission returns zero (zero-fee equity instrument, §7.1).
func (CloseFillModel) Commission(domain.Qty, domain.Price) (domain.Money, error) {
	return 0, nil
}

// closeTickVolume returns the volume assigned to the bar's CLOSE tick when a
// bar is decomposed into O/H/L/C ticks for matching. It splits the bar volume
// into quarters with min_size = 1 share (equity quantity_precision = 0):
//
//	quarter = volume // 4
//	quarter = max(quarter, 1)                 // round-down to size_increment,
//	                                          // then floor at min_size
//	if 3*quarter >= volume:  close = 1        // underflow guard -> min_size
//	else:                    close = volume - 3*quarter   (>= 1)
//
// The naive `volume - 3*floor(volume/4)` is WRONG for small volumes: e.g.
// volume=3 gives 3 naively but the correct close tick is 1 (quarter floors to 1,
// and 3*1 >= 3 trips the underflow guard). The golden depth-walk tests pin this
// down across volume/qty/side permutations.
func closeTickVolume(volume int64) int64 {
	if volume <= 0 {
		return 0
	}
	const minSize = 1 // equity size_increment = 1 share (quantity_precision 0)
	quarter := volume / 4
	if quarter < minSize {
		quarter = minSize
	}
	threeQuarters := 3 * quarter
	if threeQuarters >= volume {
		return minSize
	}
	return volume - threeQuarters
}

// residualPrice returns the price one increment adverse to side from close:
// BUY walks up (close + increment), SELL walks down (close - increment).
func residualPrice(closePx domain.Price, side domain.OrderSide) (domain.Price, error) {
	switch side {
	case domain.OrderSideBuy:
		p, err := closePx.Add(PriceIncrement)
		if err != nil {
			return 0, fmt.Errorf("residual buy price: %w", err)
		}
		return p, nil
	case domain.OrderSideSell:
		p, err := closePx.Sub(PriceIncrement)
		if err != nil {
			return 0, fmt.Errorf("residual sell price: %w", err)
		}
		return p, nil
	default:
		return 0, fmt.Errorf("%w: residual price for invalid side %q", domain.ErrInvalidArgument, side)
	}
}

// ---------------------------------------------------------------------------
// realistic
// ---------------------------------------------------------------------------

// RealisticModel is the production default ([IMPROVE]): next-bar-open fill with
// configurable slippage (bps, adverse) and commission. Commission is per-share
// when CommissionPerShare != 0, otherwise bps of notional; if both are zero the
// model is cost-free but still next-bar.
type RealisticModel struct {
	// SlippageBps is applied adversely to the fill: BUY fills higher, SELL
	// lower, by SlippageBps/10000 of the next-open price.
	SlippageBps float64
	// CommissionPerShare is a flat per-share commission in USD (e.g. 0.005).
	CommissionPerShare domain.Money
	// CommissionBps is commission as basis points of notional, used only when
	// CommissionPerShare == 0.
	CommissionBps float64
}

// Name returns "realistic".
func (RealisticModel) Name() string { return "realistic" }

// Timing returns FillNextBar.
func (RealisticModel) Timing() FillTiming { return FillNextBar }

// Fill returns a single leg: the whole order at the next bar's open adjusted by
// adverse slippage. bar is the next bar being filled against.
func (m RealisticModel) Fill(order domain.Order, bar domain.Bar) ([]FillLeg, error) {
	if order.Type != domain.OrderTypeMarket {
		return nil, fmt.Errorf("%w: realistic model fills MARKET orders only, got %s",
			domain.ErrInvalidArgument, order.Type)
	}
	if order.Qty <= 0 {
		return nil, fmt.Errorf("%w: realistic order %s non-positive qty %d",
			domain.ErrInvalidArgument, order.ClientOrderID, order.Qty)
	}
	base := bar.Open
	if m.SlippageBps == 0 {
		return []FillLeg{{Qty: order.Qty, Price: base}}, nil
	}
	// Adverse slippage: BUY pays more, SELL receives less.
	frac := m.SlippageBps / 10000.0
	adj := base.Float64() * frac
	if order.Side == domain.OrderSideSell {
		adj = -adj
	}
	p, err := domain.PriceFromFloat64(base.Float64() + adj)
	if err != nil {
		return nil, fmt.Errorf("realistic slippage price: %w", err)
	}
	return []FillLeg{{Qty: order.Qty, Price: p}}, nil
}

// Commission returns per-share commission when configured, else bps of
// notional, rounded half-to-even at 1e-4.
func (m RealisticModel) Commission(qty domain.Qty, price domain.Price) (domain.Money, error) {
	absQty := qty
	if absQty < 0 {
		absQty = -absQty
	}
	if m.CommissionPerShare != 0 {
		c, err := m.CommissionPerShare.MulInt64(int64(absQty))
		if err != nil {
			return 0, fmt.Errorf("per-share commission: %w", err)
		}
		return c, nil
	}
	if m.CommissionBps == 0 {
		return 0, nil
	}
	notional, err := price.MulQty(absQty)
	if err != nil {
		return 0, fmt.Errorf("commission notional: %w", err)
	}
	c := notional.Float64() * (m.CommissionBps / 10000.0)
	cm, err := domain.MoneyFromFloat64(c)
	if err != nil {
		return 0, fmt.Errorf("bps commission: %w", err)
	}
	return cm, nil
}
