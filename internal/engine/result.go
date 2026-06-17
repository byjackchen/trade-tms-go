package engine

// result.go is the structured backtest result the assembler returns, plus the
// run configuration. The result carries the database-source-of-truth fields
// (final balance, PnL, equity curves, trades, positions) and enough detail to
// emit the legacy runs/{ts}/*.json artifacts (locked decision 4).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

// FillProfile selects the executor fill model.
type FillProfile string

const (
	// ProfileCloseFill is the zero-cost deterministic profile: same-bar
	// close fill, no slippage, no commission.
	ProfileCloseFill FillProfile = "close-fill"
	// ProfileRealistic is the production default: next-bar-open fill with
	// configurable slippage and commission.
	ProfileRealistic FillProfile = "realistic"
)

// IsValid reports whether p is a known profile.
func (p FillProfile) IsValid() bool {
	return p == ProfileCloseFill || p == ProfileRealistic
}

// StrategySpec describes one scripted strategy to run: its engine id and its
// intent list.
type StrategySpec struct {
	ID      string
	Intents []Intent
}

// Config is the backtest specification.
type Config struct {
	// Tickers in registration order. Same-timestamp bars dispatch in this order
	// (locked decision 2).
	Tickers []string
	// Start, End bound the bar window (inclusive).
	Start, End calendar.Date
	// StartingBalance seeds the account (USD).
	StartingBalance domain.Money
	// Profile selects the fill model. Defaults to realistic when empty.
	Profile FillProfile
	// Realistic configures the realistic model (ignored for close-fill).
	Realistic RealisticParams
	// Strategies are the scripted strategies (the scripted drivers). Mutually
	// exclusive with PrebuiltStrategies; supply exactly one of the two.
	Strategies []StrategySpec
	// PrebuiltStrategies are already-constructed engine.Strategy instances (the
	// real strategy adapters: SEPA / SectorRotation / Pairs / ORB), used by the
	// multi-strategy assembler instead of Strategies. When set, the engine wires
	// these directly (no ScriptedStrategy construction) and probes each for the
	// P3 capability seams (ContextConsumer / StateSummarizer / ...). Exactly one
	// of Strategies / PrebuiltStrategies must be non-empty.
	PrebuiltStrategies []Strategy
	// Gate is the optional pre-trade gating pipeline (allocator budget +
	// aggregate risk constraints). When non-nil, every LONG/SHORT signal order
	// is gated before submission; FLAT/close orders and qty<=0 bypass the gate.
	// Rejections are counted and
	// reported in RejectedOrders (never an error).
	Gate *riskgate.Gate
	// Context, when non-nil, is the look-ahead-safe per-bar context source
	// (regime / market-cap / earnings). The engine advances it on every
	// SPYSymbol heartbeat bar and injects the resulting context snapshot into
	// each ContextConsumer strategy before OnBar (the context is published onto
	// the bus that the SignalGenerators read).
	Context *riskgate.ContextProvider
	// SPYSymbol is the heartbeat instrument whose bars drive Context refresh
	// (default "SPY"). Ignored when Context is nil.
	SPYSymbol string
	// Warmup, when non-nil, supplies OUT-OF-BAND pre-window history per symbol to
	// prime WarmupConsumer strategies (SEPA) BEFORE the event loop runs. These
	// bars are NEVER scheduled through the loop (no orders, no fills, no equity
	// samples); the engine replays ONLY [Start, End]. SEPA SGs are pre-warmed
	// from the 400d tail while Pairs/Sector are not (their generators do not
	// implement WarmupConsumer). SPY regime warmup is handled separately by the
	// ContextProvider's own full SPY history, NOT here.
	// Default (nil) => no warmup priming.
	Warmup *WarmupConfig
	// Progress, when non-nil, is called after each bar is dispatched with the
	// number of bars processed so far and the total scheduled (for "% complete"
	// reporting). It runs on the loop goroutine; keep it cheap and non-blocking.
	// It MUST NOT mutate engine state. Optional.
	Progress func(processed, total int)
}

// WarmupConfig carries the out-of-band pre-window history used to prime
// WarmupConsumer strategies. Bars maps a symbol to its warmup history (ascending
// by ts, all strictly before Config.Start). Only symbols a WarmupConsumer
// strategy trades are consumed; extra entries are ignored. These bars are not
// scheduled through the event loop.
type WarmupConfig struct {
	Bars map[string][]domain.Bar
}

// RealisticParams configures the realistic fill model.
type RealisticParams struct {
	SlippageBps        float64
	CommissionPerShare domain.Money
	CommissionBps      float64
}

// RejectedOrder records one signal order the portfolio gate rejected, in
// rejection (= signal emission) order. It is a pre-trade REJECTED order, and is
// what makes the metrics num_rejected_orders count meaningful (the gate is the
// only producer of rejections in this engine). Reason carries the rule name +
// explanation.
type RejectedOrder struct {
	StrategyID string
	Symbol     string
	SignalSide domain.SignalSide
	Qty        domain.Qty
	RuleName   string
	Reason     string
	TS         time.Time
}

// Result is the deterministic backtest output.
type Result struct {
	// StartingBalance and FinalBalance in USD; PnL = Final - Starting.
	StartingBalance domain.Money
	FinalBalance    domain.Money
	TotalPnL        domain.Money

	// Profile is the fill model used.
	Profile FillProfile

	// Strategies are the engine strategy ids run, sorted.
	Strategies []string

	// Orders submitted (submission order), Fills (settlement order).
	Orders []domain.Order
	Fills  []domain.Fill

	// RejectedOrders are the signal orders the portfolio gate rejected, in
	// rejection order (empty when no gate is configured or nothing was
	// rejected). len(RejectedOrders) == num_rejected_orders.
	RejectedOrders []RejectedOrder

	// Positions are the final position snapshots (all keys, sorted).
	Positions []domain.Position

	// AccountStates is the account-state curve (one point per settlement plus
	// the initial state), the basis for account.json (§7.4).
	AccountStates []AccountStatePoint

	// TotalEquityCurve is the per-bar sampled account equity.
	TotalEquityCurve []accounting.EquityPoint

	// StrategyEquity maps engine strategy id -> per-bar cumulative-PnL curve.
	StrategyEquity map[string][]accounting.EquityPoint

	// BarsProcessed counts dispatched bars; SampledDays counts equity samples.
	BarsProcessed int
	SampledDays   int

	// FirstTS/LastTS bound the processed data (UTC).
	FirstTS, LastTS time.Time
}

// OrderCounts are the order/position counters for a Result, scoped to a single
// engine strategy id (or the whole portfolio when strategyID == ""). It is the
// single source of truth for num_orders / num_filled_orders /
// num_rejected_orders / num_positions across every metrics path (the P2/P3
// backtest assembly AND the P4 hyperopt objective), so the same Result always
// yields the same counters regardless of caller.
type OrderCounts struct {
	NumOrders         int
	NumFilledOrders   int
	NumRejectedOrders int
	NumPositions      int
}

// Counts returns the order/position counters scoped to strategyID ("" =
// portfolio: all strategies):
//
//   - num_orders        — orders SUBMITTED to the executor.
//   - num_filled_orders — orders that produced at least one fill (the engine
//     settles fills asynchronously and never mutates the submitted order's
//     Status to FILLED, so we derive "filled" from res.Fills, NOT from
//     Order.Status which stays at submit-time).
//   - num_rejected_orders — submitted orders left in a REJECTED status PLUS the
//     signal orders the portfolio gate blocked pre-submit (RejectedOrders); the
//     gate is the engine's pre-trade REJECTED order.
//   - num_positions     — final position snapshots (one per (strategy, symbol)
//     that ever left flat).
func (r *Result) Counts(strategyID string) OrderCounts {
	var c OrderCounts
	filledByOrder := make(map[string]bool, len(r.Fills))
	for _, f := range r.Fills {
		if strategyID != "" && f.StrategyID != strategyID {
			continue
		}
		filledByOrder[f.ClientOrderID] = true
	}
	for _, o := range r.Orders {
		if strategyID != "" && o.StrategyID != strategyID {
			continue
		}
		c.NumOrders++
		if filledByOrder[o.ClientOrderID] {
			c.NumFilledOrders++
		}
		if o.Status == domain.OrderStatusRejected {
			c.NumRejectedOrders++
		}
	}
	for _, rej := range r.RejectedOrders {
		if strategyID != "" && rej.StrategyID != strategyID {
			continue
		}
		c.NumRejectedOrders++
	}
	for _, pos := range r.Positions {
		if strategyID != "" && pos.StrategyID != strategyID {
			continue
		}
		c.NumPositions++
	}
	return c
}
