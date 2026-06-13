package engine

// strategy.go defines the engine-facing Strategy seam and the ScriptedStrategy
// test double — the PARITY DRIVER. ScriptedStrategy consumes a deterministic
// list of (date, ticker, side, qty) intents and submits the corresponding
// market orders on the matching bar, so the engine can be gated against
// Nautilus WITHOUT any real strategy logic (real strategies are P3).

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// OrderSubmitter is the narrow capability a strategy needs from the engine: to
// submit a market order during on_bar. The engine supplies an implementation
// that assigns a deterministic client order id and routes to the executor.
type OrderSubmitter interface {
	// SubmitMarket submits a market order for the strategy and returns the
	// assigned client order id.
	SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error)
}

// Strategy is the engine-facing strategy contract. OnBar fires once per bar in
// the deterministic event order. A strategy submits orders via the submitter.
type Strategy interface {
	// ID returns the engine strategy id (e.g. "Scripted-000", §7.7).
	ID() string
	// OnBar handles one bar. It may submit orders through sub.
	OnBar(sub OrderSubmitter, bar domain.Bar) error
}

// Intent is one scripted trading instruction: on the bar dated Date for Ticker,
// submit a market order of Side for Qty shares. Side is the strategy-level
// SignalSide; LONG -> BUY, SHORT -> SELL. FLAT closes the strategy's net
// position in the ticker (qty taken from the live position; the Qty field is
// ignored for FLAT, mirroring the reference runners).
type Intent struct {
	Date   time.Time // trading date; matched against bar.TS (UTC, day-aligned)
	Ticker string
	Side   domain.SignalSide
	Qty    domain.Qty
}

// PositionReader lets a strategy read its current net position (for FLAT close
// sizing), mirroring the reference's portfolio.net_position lookup.
type PositionReader interface {
	// NetPosition returns the strategy's signed position in symbol (0 if flat).
	NetPosition(strategyID, symbol string) domain.Qty
}

// ScriptedStrategy replays a fixed intent list. It is fully deterministic: the
// same intents and bars always submit the same orders. Intents are indexed by
// (UTC date, ticker) for O(1) lookup per bar.
type ScriptedStrategy struct {
	id      string
	byDay   map[dayKey][]Intent
	posRead PositionReader
}

type dayKey struct {
	year  int
	month time.Month
	day   int
	tick  string
}

func keyOf(ts time.Time, ticker string) dayKey {
	u := ts.UTC()
	return dayKey{year: u.Year(), month: u.Month(), day: u.Day(), tick: ticker}
}

// NewScriptedStrategy builds a strategy with the given engine id and intents.
// posRead supplies live net positions for FLAT close sizing (may be nil if no
// FLAT intents are used). Intents are validated: each must have a non-empty
// ticker, a valid side, and (for LONG/SHORT) a positive qty.
func NewScriptedStrategy(id string, intents []Intent, posRead PositionReader) (*ScriptedStrategy, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: scripted strategy needs a non-empty id", domain.ErrInvalidArgument)
	}
	byDay := make(map[dayKey][]Intent)
	for i, in := range intents {
		if in.Ticker == "" {
			return nil, fmt.Errorf("%w: intent %d has empty ticker", domain.ErrInvalidArgument, i)
		}
		if !in.Side.IsValid() {
			return nil, fmt.Errorf("%w: intent %d has invalid side %q", domain.ErrInvalidArgument, i, in.Side)
		}
		if in.Side != domain.SideFlat && in.Qty <= 0 {
			return nil, fmt.Errorf("%w: intent %d (%s %s) needs positive qty, got %d",
				domain.ErrInvalidArgument, i, in.Side, in.Ticker, in.Qty)
		}
		if in.Date.IsZero() {
			return nil, fmt.Errorf("%w: intent %d has zero date", domain.ErrInvalidArgument, i)
		}
		k := keyOf(in.Date, in.Ticker)
		byDay[k] = append(byDay[k], in)
	}
	return &ScriptedStrategy{id: id, byDay: byDay, posRead: posRead}, nil
}

// ID returns the engine strategy id.
func (s *ScriptedStrategy) ID() string { return s.id }

// OnBar submits the orders scripted for this bar's (date, ticker), in list
// order. LONG -> BUY, SHORT -> SELL, FLAT -> a close order sized from the live
// net position (no order when flat), matching the reference FLAT translation
// (§7.4).
func (s *ScriptedStrategy) OnBar(sub OrderSubmitter, bar domain.Bar) error {
	intents := s.byDay[keyOf(bar.TS, bar.Symbol)]
	for _, in := range intents {
		switch in.Side {
		case domain.SideLong, domain.SideShort:
			side, err := domain.OrderSideFor(in.Side)
			if err != nil {
				return err
			}
			reason := fmt.Sprintf("scripted %s %d %s", in.Side, in.Qty, in.Ticker)
			if _, err := sub.SubmitMarket(s.id, in.Ticker, side, in.Qty, reason, bar.TS); err != nil {
				return err
			}
		case domain.SideFlat:
			var net domain.Qty
			if s.posRead != nil {
				net = s.posRead.NetPosition(s.id, in.Ticker)
			}
			side, ok := domain.CloseSideFor(net)
			if !ok {
				continue // already flat: no order
			}
			closeQty := net
			if closeQty < 0 {
				closeQty = -closeQty
			}
			reason := fmt.Sprintf("scripted FLAT (close %d) %s", net, in.Ticker)
			if _, err := sub.SubmitMarket(s.id, in.Ticker, side, closeQty, reason, bar.TS); err != nil {
				return err
			}
		}
	}
	return nil
}
