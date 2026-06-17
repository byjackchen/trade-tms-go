package engine

// strategy.go defines the engine-facing Strategy seam and the ScriptedStrategy
// test double — the SCRIPTED DRIVER. ScriptedStrategy consumes a deterministic
// list of (date, ticker, side, qty) intents and submits the corresponding
// market orders on the matching bar, so the engine can be exercised WITHOUT any
// real strategy logic (real strategies are P3).

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// OrderSubmitter is the narrow capability a strategy needs from the engine: to
// submit a market order during on_bar. The engine supplies an implementation
// that assigns a deterministic client order id, runs the pre-trade portfolio
// gate (when configured), and routes the approved order to the executor.
//
// SubmitMarket is the ungated primitive: it always submits (the caller has
// already decided side/qty). The real strategy adapters instead use
// SubmitMarketSignal, which carries the originating strategy-level SignalSide
// so the engine can run the portfolio gate (FLAT and qty<=0 bypass the gate;
// LONG/SHORT are gated against the allocator budget and the aggregate risk
// rules).
type OrderSubmitter interface {
	// SubmitMarket submits a market order for the strategy and returns the
	// assigned client order id. It does NOT run the portfolio gate — use it for
	// already-translated close/flatten orders, or for ungated scripted replays.
	SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error)

	// SubmitMarketSignal submits a market order for a strategy SIGNAL, running
	// the pre-trade portfolio gate first when one is configured. signalSide is
	// the strategy-level side (LONG/SHORT/FLAT); orderSide is its translated
	// broker side (BUY/SELL). qty is the absolute share magnitude. It returns
	// the assigned client order id and whether the order was actually submitted
	// (false == rejected by the gate, no order placed — the runner skips the
	// submit). A gate rejection is NOT an error.
	SubmitMarketSignal(strategyID, symbol string, signalSide domain.SignalSide, orderSide domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (coid string, submitted bool, err error)
}

// Strategy is the engine-facing strategy contract. OnBar fires once per bar in
// the deterministic event order. A strategy submits orders via the submitter.
type Strategy interface {
	// ID returns the engine strategy id (e.g. "Scripted-000", §7.7).
	ID() string
	// OnBar handles one bar. It may submit orders through sub.
	OnBar(sub OrderSubmitter, bar domain.Bar) error
}

// ---------------------------------------------------------------------------
// Capability interfaces (P3, locked decision 3): EXTEND the Strategy seam
// additively. Real strategies (SEPA / Pairs / Sector-ORB) implement these so
// the engine and runners can inject context, pull per-leg intents, summarize
// state, and persist/restore — WITHOUT changing the base Strategy interface,
// so ScriptedStrategy keeps compiling untouched. The engine probes each
// capability via a type assertion; a strategy that does not implement one is
// simply not asked for it.
// ---------------------------------------------------------------------------

// IntentEvaluator is a strategy that can emit per-leg/per-name SignalIntents
// for observability after a bar (read-side only; never affects trading).
type IntentEvaluator interface {
	// EvaluateIntentJSON returns the intents for the as-of timestamp as a
	// JSON-serializable value (the concrete intent slice). It must be a pure
	// read of strategy telemetry/state.
	EvaluateIntentJSON(asOf time.Time) any
}

// StateSummarizer is a strategy that can publish a JSON-serializable summary
// of its current state for the UI after every bar.
type StateSummarizer interface {
	StateSummaryJSON() any
}

// StatePersister is a strategy that can snapshot and restore its full internal
// state for warm restarts.
type StatePersister interface {
	StateDictJSON() any
	LoadStateJSON(b []byte) error
}

// ContextConsumer is a strategy that consumes per-bar portfolio context
// (regime / market-cap / earnings) injected before OnBar. Pairs does not need
// context; SEPA does. Defined here so the seam is uniform across strategies.
type ContextConsumer interface {
	// InjectContext supplies the context snapshot effective for ts.
	InjectContext(ctx StrategyContext)
}

// SymbolScoped is an OPTIONAL capability a strategy implements to DECLARE the
// exact set of bar symbols it reacts to — the symbols whose bars its OnBar
// mutates state for or emits signals from. A strategy's OnBar is a NO-OP (returns
// no signals, mutates no state) for any bar whose symbol is NOT in this set
// (every real generator self-filters: bar.Symbol != cfg.Symbol / not in the pair
// universe / not in the rotation universe returns early). Declaring the set lets
// the engine dispatch each bar ONLY to the matching adapter(s) instead of the
// full ~N-strategy scan, an O(strategies)->O(matches) per-bar win for the
// full-universe SEPA path (one single-symbol adapter per name).
//
// SymbolsScoped MUST return EVERY symbol the strategy reacts to:
//   - SEPA / ORB (single-symbol): the one trading symbol.
//   - Pairs (multi-symbol): every leg symbol across all pairs.
//   - SectorRotation (multi-symbol): every universe ETF.
//
// Omitting a reacted-to symbol would DROP that bar from the strategy and change
// behaviour, so the contract is "exhaustive or do not implement". A strategy that
// CANNOT enumerate its symbols (or reacts to all of them, e.g. a broadcast
// observer) simply DOES NOT implement SymbolScoped: the engine then falls back to
// dispatching EVERY bar to it (the pre-optimization behaviour), so the indexed
// dispatch is always a safe superset-free subset — it never skips a bar a full
// scan would have delivered. The returned slice is read-only (the engine does not
// mutate it) and its order is irrelevant (used only to build a membership set).
type SymbolScoped interface {
	SymbolsScoped() []string
}

// WarmupConsumer is a strategy that can PRIME its internal indicator/history
// state from pre-window historical bars OUT OF BAND — i.e. before the engine's
// event loop runs, WITHOUT submitting any orders and WITHOUT emitting any
// equity samples. The 400-calendar-day warmup tail is injected directly into
// the SignalGenerator's history, NOT replayed through the engine/venue. The
// engine then replays ONLY the [Start, End] run window.
//
// ONLY strategies that receive this out-of-band warmup implement it — SEPA
// (warmup_ticker) does; SectorRotation and Pairs do NOT (they pull
// run-window-only bars and build rolling state from in-window on_bar calls).
// Preserving that asymmetry is REQUIRED for a correct objective: a Pairs
// lookback=60 SG is intentionally NOT warm at test_start.
//
// WarmupBars receives the historical bars for ONE symbol the strategy trades,
// ascending by ts, all strictly before the run window. A strategy that trades
// no instrument matching sym (or has no use for the history) is a no-op.
type WarmupConsumer interface {
	// WarmupBars primes the strategy's state for sym from history (ascending,
	// pre-window). It MUST NOT submit orders or mutate the account.
	WarmupBars(sym string, history []domain.Bar)
}

// BatchWarmupConsumer is a MULTI-SYMBOL strategy that primes its internal state
// from a pre-window bar stream that must be delivered INTERLEAVED across symbols
// (dispatch-ordered: ascending by ts, registration order within a ts), not the
// per-symbol fan-out of WarmupConsumer.
//
// SectorRotation and Pairs build their state from CROSS-SYMBOL on_bar calls (a
// month-rollover rebalance needs every ETF's latest close; a pair's z-score
// needs both legs in-sync at the same date). Feeding their warmup per-symbol —
// all of E1's bars, then all of E2's — would break the month-rollover / pair-sync
// detection. They therefore consume the whole interleaved pre-window stream at
// once and replay it through their pure generator's OnBar (discarding the emitted
// signals), reaching EXACTLY the state a backtest that processed those same
// pre-window bars in-loop would have — WITHOUT submitting any orders or mutating
// any account. This is what makes a warmed live sector/pairs session consistent
// with a backtest over [start-lookback, end].
//
// SEPA implements the per-symbol WarmupConsumer instead (its generator is
// single-symbol and warms per-ticker). A strategy implements at most ONE of the
// two warmup seams.
type BatchWarmupConsumer interface {
	// WarmupBatch primes the strategy from the interleaved pre-window bar stream
	// (ascending by ts, all strictly before the run window). It MUST NOT submit
	// orders or mutate the account — pure state priming, identical to what an
	// in-loop replay of the same bars would build minus the order side effects.
	WarmupBatch(bars []domain.Bar)
}

// StrategyContext is the per-bar context a ContextConsumer may read. Fields are
// optional; a zero value means "not provided".
type StrategyContext struct {
	Regime           string
	AsOf             time.Time
	MarketCapUSD     map[string]float64
	EarningsBlackout map[string]bool
}

// Intent is one scripted trading instruction: on the bar dated Date for Ticker,
// submit a market order of Side for Qty shares. Side is the strategy-level
// SignalSide; LONG -> BUY, SHORT -> SELL. FLAT closes the strategy's net
// position in the ticker (qty taken from the live position; the Qty field is
// ignored for FLAT).
type Intent struct {
	Date   time.Time // trading date; matched against bar.TS (UTC, day-aligned)
	Ticker string
	Side   domain.SignalSide
	Qty    domain.Qty
}

// PositionReader lets a strategy read its current net position (for FLAT close
// sizing).
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
// net position (no order when flat), per the FLAT translation (§7.4).
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
