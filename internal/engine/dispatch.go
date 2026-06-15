package engine

// dispatch.go holds the genuinely-shared per-bar dispatch glue that BOTH the
// batch driver (engine.Engine) and the streaming/replay driver
// (livengine.Session) run, so the two paths cannot silently diverge across the
// hand-copied "// identical to engine.X" seam (modularization-review.md §F3 /
// E2). It deliberately extracts ONLY the shared surface — the context-injection
// seam and the warmup priming loop — NOT the loop drivers themselves (the batch
// handleBar carries exec/fill-timing, the streaming onBar carries timestamp-
// rollover guards; those stay mode-specific, per §E6). The cross-path
// equivalence test (livengine/crosspath_test.go) pins that this extraction is
// behaviour-preserving.

import (
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// StrategyContextFrom builds the per-bar StrategyContext from the shared context
// state (the look-ahead-safe store the ContextProvider just wrote on the SPY
// heartbeat). asOf is the heartbeat bar's timestamp (telemetry only; the field
// values come from the state). It is the single source of the StrategyContext
// shape: both engine.Engine.injectContext and livengine.Session.injectContext
// call it, so a field added to StrategyContext is wired in exactly one place
// (modularization-review.md §E2). The snapshot maps carry ONLY symbols with a
// published value, so a consumer for a symbol without context keeps its prior
// value (matching the Actors only calling set_* on transitions).
func StrategyContextFrom(asOf time.Time, st *portfolio.SharedContextState) StrategyContext {
	return StrategyContext{
		Regime:           st.Regime(),
		AsOf:             asOf,
		MarketCapUSD:     st.MarketCapFloats(),
		EarningsBlackout: st.EarningsBlackouts(),
	}
}

// InjectContextInto snapshots the shared context state (via StrategyContextFrom)
// and pushes it into every ContextConsumer. It is the shared context-injection
// seam both dispatch drivers call on the SPY heartbeat; a nil state or empty
// consumer set is a no-op. asOf is the heartbeat bar's timestamp.
func InjectContextInto(cons []ContextConsumer, asOf time.Time, st *portfolio.SharedContextState) {
	if len(cons) == 0 || st == nil {
		return
	}
	ctx := StrategyContextFrom(asOf, st)
	for _, cc := range cons {
		cc.InjectContext(ctx)
	}
}

// PrimeWarmup feeds out-of-band pre-window history into every WarmupConsumer
// strategy, once per (symbol, strategy), BEFORE the loop runs — no executor, no
// account mutation, no sampling, pure indicator/history priming (the faithful Go
// port of SEPAUniverseRunner.warmup_ticker). It is the shared warmup loop both
// dispatch drivers call; they differ ONLY in where the per-symbol bars come from
// (the batch engine reads a preloaded map, the live session queries a provider),
// so the caller supplies a barsFor closure that yields the history for one
// symbol (returning an error aborts priming, e.g. a provider failure).
//
// Symbols are primed in sorted order (priming is per-symbol independent, but a
// stable order keeps any future logging reproducible). Each consumer self-filters
// by symbol (SEPA primes only its own symbol; Pairs/Sector are not
// WarmupConsumers and never reach here), so offering every warmup symbol to every
// consumer is correct and order-independent. An empty symbol set is a no-op.
func PrimeWarmup(strategies []Strategy, symbols []string, barsFor func(sym string) ([]domain.Bar, error)) error {
	return PrimeWarmupExcept(strategies, symbols, nil, barsFor)
}

// PrimeWarmupExcept is PrimeWarmup with a skip set: strategies whose ID is in
// skip are NOT primed. The skip set carries the IDs of strategies whose state was
// already RESTORED from a crash-recovery snapshot — re-warming a recovered
// strategy over its restored state would corrupt it (recovery supersedes warmup;
// see livetrade/session.go Prime). A nil/empty skip set is identical to
// PrimeWarmup (the signal-mode / backtest path, which never recovers).
func PrimeWarmupExcept(strategies []Strategy, symbols []string, skip map[string]bool, barsFor func(sym string) ([]domain.Bar, error)) error {
	if len(symbols) == 0 {
		return nil
	}
	syms := append([]string(nil), symbols...)
	sort.Strings(syms)
	for _, st := range strategies {
		if skip[st.ID()] {
			continue
		}
		wc, ok := st.(WarmupConsumer)
		if !ok {
			continue
		}
		for _, sym := range syms {
			hist, err := barsFor(sym)
			if err != nil {
				return err
			}
			if len(hist) > 0 {
				wc.WarmupBars(sym, hist)
			}
		}
	}
	return nil
}

// PrimeWarmupBatch feeds an INTERLEAVED (dispatch-ordered) pre-window bar stream
// into every BatchWarmupConsumer strategy (SectorRotation / Pairs), BEFORE the
// loop runs — no executor, no account mutation, no sampling, pure state priming.
// It is the multi-symbol counterpart of PrimeWarmup: where PrimeWarmup fans out
// per-symbol (SEPA), this delivers the WHOLE cross-symbol stream at once so the
// generator's month-rollover / pair-sync logic sees the same bar ordering an
// in-loop backtest replay would, reaching the same state at the run-window start
// WITHOUT submitting orders.
//
// bars must be ascending by ts (dispatch order), all strictly before the run
// window; the caller (the assembler / engine) guarantees look-ahead safety. Each
// BatchWarmupConsumer receives its OWN copy-free view of the SAME stream and
// self-filters out-of-universe symbols (its pure generator ignores them), so
// offering the full stream to every consumer is correct. An empty stream or no
// batch consumers is a no-op.
func PrimeWarmupBatch(strategies []Strategy, bars []domain.Bar) {
	PrimeWarmupBatchExcept(strategies, bars, nil)
}

// PrimeWarmupBatchExcept is PrimeWarmupBatch with a skip set: BatchWarmupConsumer
// strategies whose ID is in skip are NOT primed. The skip set carries the IDs of
// strategies whose cross-symbol state (sector month-rollover ring / pairs spread
// window) was already RESTORED from a crash-recovery snapshot. The batch seam
// APPENDS the pre-window bars by replaying them through the generator's OnBar
// (ring push, bounded deque), so re-warming a recovered strategy would push the
// pre-window bars onto the already-restored ring — resetting lastUniverseDate to
// an OLDER pre-window month (scrambling the next-bar month rollover) and
// corrupting the momentum window. Skipping restored strategies enforces the
// "recovery supersedes warmup" invariant (livetrade/session.go Prime). A
// nil/empty skip set is identical to PrimeWarmupBatch (signal/backtest, no
// recovery).
func PrimeWarmupBatchExcept(strategies []Strategy, bars []domain.Bar, skip map[string]bool) {
	if len(bars) == 0 {
		return
	}
	for _, st := range strategies {
		if skip[st.ID()] {
			continue
		}
		if bw, ok := st.(BatchWarmupConsumer); ok {
			bw.WarmupBatch(bars)
		}
	}
}
