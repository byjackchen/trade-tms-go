package accounting

// position.go is the mutable NETTING position book entry. One Position exists
// per (strategy_id, symbol). It applies fills, maintaining a signed quantity,
// an average entry price, and cumulative realized PnL per this library's
// NETTING arithmetic (see doc.go).
//
// All money math is exact 1e-4 fixed point (domain.Money / domain.Price);
// average price is held as an exact rational (notional / qty) to avoid the
// rounding drift naive cost-basis tracking would otherwise accumulate over
// long fill chains, then materialized to a Price only for reporting.

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Position is one netted position. The zero Position (signedQty 0) is flat.
// Not safe for concurrent use; mutated only on the engine loop goroutine.
type Position struct {
	strategyID string
	symbol     string

	signedQty domain.Qty // + long, - short, 0 flat

	// Average entry cost is tracked as an exact rational entryNotional /
	// |signedQty| in 1e-4 money units. entryNotional is the absolute cost
	// basis of the currently-open quantity (always >= 0). When flat it is 0.
	entryNotional domain.Money

	realized  domain.Money // cumulative realized PnL over this position's life
	updatedAt time.Time    // ts of the last applied fill (UTC)
}

// NewPosition returns a flat position for the (strategyID, symbol) key.
func NewPosition(strategyID, symbol string) *Position {
	return &Position{strategyID: strategyID, symbol: symbol}
}

// StrategyID returns the engine strategy id.
func (p *Position) StrategyID() string { return p.strategyID }

// Symbol returns the instrument symbol.
func (p *Position) Symbol() string { return p.symbol }

// SignedQty returns the signed share count (+ long, - short, 0 flat).
func (p *Position) SignedQty() domain.Qty { return p.signedQty }

// IsFlat reports whether the position is flat.
func (p *Position) IsFlat() bool { return p.signedQty == 0 }

// RealizedPnL returns cumulative realized PnL.
func (p *Position) RealizedPnL() domain.Money { return p.realized }

// UpdatedAt returns the timestamp of the last applied fill.
func (p *Position) UpdatedAt() time.Time { return p.updatedAt }

// AvgEntryPrice returns the current average entry price as a Price (0 when
// flat). It is computed exactly as entryNotional / |signedQty|, rounded
// half-to-even at 1e-4 — used only for reporting/snapshots; internal math uses
// the exact notional.
func (p *Position) AvgEntryPrice() domain.Price {
	if p.signedQty == 0 {
		return 0
	}
	abs := absQty(p.signedQty)
	return domain.Price(roundHalfEvenDiv(int64(p.entryNotional), abs))
}

// Snapshot returns an immutable domain.Position view.
func (p *Position) Snapshot() domain.Position {
	return domain.Position{
		StrategyID:  p.strategyID,
		Symbol:      p.symbol,
		SignedQty:   p.signedQty,
		AvgPx:       p.AvgEntryPrice(),
		RealizedPnL: p.realized,
		UpdatedAt:   p.updatedAt,
	}
}

// UnrealizedPnL returns the mark-to-market PnL of the open quantity at last:
// signedQty*last - entryNotional (long), and the analogous signed value for a
// short. Returns 0 when flat. Overflow-checked.
func (p *Position) UnrealizedPnL(last domain.Price) (domain.Money, error) {
	if p.signedQty == 0 {
		return 0, nil
	}
	mktValue, err := last.MulQty(p.signedQty) // signed: negative for shorts
	if err != nil {
		return 0, fmt.Errorf("unrealized for %s/%s: %w", p.strategyID, p.symbol, err)
	}
	// cost basis is signed the same way as the position: +entryNotional for a
	// long, -entryNotional for a short.
	cost := p.entryNotional
	if p.signedQty < 0 {
		neg, nerr := cost.Neg()
		if nerr != nil {
			return 0, fmt.Errorf("unrealized for %s/%s: %w", p.strategyID, p.symbol, nerr)
		}
		cost = neg
	}
	u, err := mktValue.Sub(cost)
	if err != nil {
		return 0, fmt.Errorf("unrealized for %s/%s: %w", p.strategyID, p.symbol, err)
	}
	return u, nil
}

// FillOutcome reports what a fill did to the position, for account settlement
// and event recording.
type FillOutcome struct {
	// RealizedDelta is the realized PnL produced by this fill (0 for pure
	// opens/increases). Account balance moves by exactly this amount.
	RealizedDelta domain.Money
	// Opened/Closed/Changed/Flipped classify the transition (for recording
	// PositionOpened/Closed/Changed events).
	Opened, Closed, Changed, Flipped bool
}

// ApplyFill mutates the position with a delta fill (positive qty, BUY or SELL)
// and returns the realized-PnL delta and transition classification.
//
// The arithmetic is this library's NETTING rule:
//   - same-direction (or opening from flat): re-weight entryNotional, realized
//     unchanged;
//   - opposite-direction within the open qty: realize closedQty *
//     (fill_px - avg) [long] / (avg - fill_px) [short]; reduce entryNotional
//     proportionally; if it brings qty to 0 the position closes;
//   - opposite-direction exceeding the open qty (flip): close the whole open
//     qty (realize it), then open the residual at fill_px with realized
//     accumulating (cumulative over the position's life).
func (p *Position) ApplyFill(f domain.Fill) (FillOutcome, error) {
	if f.Qty <= 0 {
		return FillOutcome{}, fmt.Errorf("%w: fill %s qty must be positive, got %d",
			domain.ErrInvalidArgument, f.TradeID, f.Qty)
	}
	// Signed delta in shares: BUY adds, SELL subtracts.
	var delta domain.Qty
	switch f.Side {
	case domain.OrderSideBuy:
		delta = f.Qty
	case domain.OrderSideSell:
		neg, err := f.Qty.Neg()
		if err != nil {
			return FillOutcome{}, fmt.Errorf("fill %s: %w", f.TradeID, err)
		}
		delta = neg
	default:
		return FillOutcome{}, fmt.Errorf("%w: fill %s has invalid side %q",
			domain.ErrInvalidArgument, f.TradeID, f.Side)
	}

	prevQty := p.signedQty
	newQty, err := prevQty.Add(delta)
	if err != nil {
		return FillOutcome{}, fmt.Errorf("fill %s: position qty overflow: %w", f.TradeID, err)
	}

	var out FillOutcome
	switch {
	case prevQty == 0:
		// Opening from flat.
		if err := p.openLeg(f.Qty, f.Price); err != nil {
			return FillOutcome{}, err
		}
		out.Opened = true

	case sameSign(prevQty, newQty) && absQty(newQty) > absQty(prevQty):
		// Increasing an existing position (same direction). Re-weight cost.
		if err := p.openLeg(f.Qty, f.Price); err != nil {
			return FillOutcome{}, err
		}
		out.Changed = true

	case sameSign(prevQty, newQty) && newQty != 0:
		// Reducing (partial close), still same direction. Realize on closedQty.
		realized, err := p.closeLeg(f.Qty, f.Price, prevQty)
		if err != nil {
			return FillOutcome{}, err
		}
		out.RealizedDelta = realized
		out.Changed = true

	case newQty == 0:
		// Exact close to flat. Realize on the whole previous qty.
		closedQty := absQtyVal(prevQty)
		realized, err := p.closeLeg(closedQty, f.Price, prevQty)
		if err != nil {
			return FillOutcome{}, err
		}
		out.RealizedDelta = realized
		out.Closed = true

	default:
		// Flip: opposite direction exceeding the open qty. Close all of prevQty,
		// then open |newQty| at fill price.
		closedQty := absQtyVal(prevQty)
		realized, err := p.closeLeg(closedQty, f.Price, prevQty)
		if err != nil {
			return FillOutcome{}, err
		}
		out.RealizedDelta = realized
		// p is now flat (closeLeg zeroed it). Open the residual.
		residual := absQtyVal(newQty)
		if err := p.openLeg(residual, f.Price); err != nil {
			return FillOutcome{}, err
		}
		out.Flipped = true
		out.Closed = true
		out.Opened = true
	}

	p.signedQty = newQty
	p.updatedAt = f.TS
	return out, nil
}

// openLeg adds qty shares at price to the cost basis (used for opens and
// same-direction increases). It does not change signedQty (the caller sets the
// final qty); it only grows entryNotional by qty*price.
func (p *Position) openLeg(qty domain.Qty, price domain.Price) error {
	add, err := price.MulQty(qty)
	if err != nil {
		return fmt.Errorf("position %s/%s open leg: %w", p.strategyID, p.symbol, err)
	}
	n, err := p.entryNotional.Add(add)
	if err != nil {
		return fmt.Errorf("position %s/%s open leg: %w", p.strategyID, p.symbol, err)
	}
	p.entryNotional = n
	return nil
}

// closeLeg realizes PnL for closing closeQty shares at price against the
// prevailing direction (prevQty's sign), reduces entryNotional by the closed
// portion's cost basis, and returns the realized delta. The position quantity
// is set by the caller; here we only adjust cost basis and realized.
func (p *Position) closeLeg(closeQty domain.Qty, price domain.Price, prevQty domain.Qty) (domain.Money, error) {
	prevAbs := absQty(prevQty)
	closeAbs := absQty(closeQty)
	if closeAbs == 0 {
		return 0, nil
	}
	// avg entry of the open qty = entryNotional / prevAbs (exact rational).
	// Cost basis released for the closed portion = entryNotional * closeAbs /
	// prevAbs, computed exactly with a single rounding at the end.
	releasedCost := mulDivRoundHalfEven(int64(p.entryNotional), closeAbs, prevAbs)

	// Proceeds at fill price for the closed shares.
	proceeds, err := price.MulQty(closeQty) // closeQty positive
	if err != nil {
		return 0, fmt.Errorf("position %s/%s close leg: %w", p.strategyID, p.symbol, err)
	}

	var realizedFull domain.Money
	if prevQty > 0 {
		// Long: realized = proceeds - releasedCost.
		realizedFull, err = proceeds.Sub(domain.Money(releasedCost))
	} else {
		// Short: realized = releasedCost - proceeds.
		realizedFull, err = domain.Money(releasedCost).Sub(proceeds)
	}
	if err != nil {
		return 0, fmt.Errorf("position %s/%s close leg: %w", p.strategyID, p.symbol, err)
	}
	// Quantize realized PnL to cents (2 dp, half-even) PER closing fill: USD
	// realized_pnl has cent precision and is quantized on every fill (e.g. a
	// depth-walk average of 105.001666... yields per-fill realized 299.83 /
	// -400.17 / -1300.17, each rounded to the cent). Settling at 4 dp instead
	// would drift by up to a cent per fill.
	realized := roundMoneyToCents(realizedFull)

	// Reduce the remaining cost basis.
	remaining, err := p.entryNotional.Sub(domain.Money(releasedCost))
	if err != nil {
		return 0, fmt.Errorf("position %s/%s close leg: %w", p.strategyID, p.symbol, err)
	}
	// Guard against tiny negative residue from rounding on a full close.
	if closeAbs == prevAbs {
		remaining = 0
	}
	p.entryNotional = remaining

	acc, err := p.realized.Add(realized)
	if err != nil {
		return 0, fmt.Errorf("position %s/%s close leg: %w", p.strategyID, p.symbol, err)
	}
	p.realized = acc
	return realized, nil
}
