package livengine_test

// crosspath_test.go is the F3 SAFETY NET (modularization-review.md §F3 / SEAM-2):
// the permanent CROSS-PATH EQUIVALENCE TEST that runs ONE identical
// strategyassembly.Assemble output through BOTH dispatch drivers —
//
//	(A) the BATCH driver: engine.New + Engine.Run  (the backtest/hyperopt loop)
//	(B) the STREAMING driver: livengine.Session.RunStream over a VirtualClock
//	    AND the EOD batch-replay driver: livengine.Session.Replay
//
// — and asserts the emitted per-timestamp signals are IDENTICAL across all
// three (canonical compare).
//
// WHY THIS EXISTS. The pre-existing consistency_test.go only compares two
// livengine.Session paths (stream vs Session.Replay); it NEVER drives
// engine.New. The F3 finding is exactly that gap: the batch per-bar dispatch
// (engine.handleBar: context-injection on the SPY heartbeat, warmup priming,
// strategy OnBar in registration order) is HAND-COPIED into livengine.onBar with
// no test tying the two together. A change to context timing, warmup ordering or
// the SPY-heartbeat rule on one side can silently diverge the live path from
// backtest — the exact P4 warmup-divergence regression — with no test failing.
// This test makes the "five modes share the engine 100%" thesis structurally
// guarded instead of comment-asserted, and MUST be added BEFORE any seam refactor
// (Guard phase): it pins the current behaviour so the later F3/E2 extraction is
// provably behaviour-preserving.
//
// HOW THE BATCH INTENTS ARE OBSERVED. The batch engine.Engine does not itself
// emit signals (it emits orders/fills). But the strategies it runs ARE
// SignalEvaluators, and an intent is a PURE READ of strategy state that is
// fill-independent (noop.go: "generators evolve purely from OnBar(bar) inputs and
// never read fills/positions"). So we wrap each assembled strategy in a thin
// intentRecordingProxy that delegates OnBar to the real strategy and, on a
// timestamp rollover (detected from the bars it sees, exactly as
// livengine.Session.flushTimestamp detects it), records the completed
// timestamp's EvaluateSignalJSON. The proxy changes NO behaviour: it forwards
// OnBar / ID untouched and only observes. Running the SAME wrapped instances as
// PrebuiltStrategies through engine.New therefore yields the per-timestamp intent
// stream the batch driver would produce — which must equal the streaming driver's.
//
// The two drivers use SEPARATE assemblies (generators mutate state + a generation
// counter), built identically, so the comparison is meaningful (not aliasing).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
)

// intentRecordingProxy wraps an engine.Strategy that is also an
// engine.SignalEvaluator. It is BEHAVIOR-TRANSPARENT for the engine: ID and OnBar
// delegate verbatim to the inner strategy, so the batch engine drives the real
// strategy exactly as it would unwrapped. It additionally OBSERVES the bar stream
// to detect timestamp rollovers and snapshot the inner strategy's
// EvaluateSignalJSON for each COMPLETED timestamp — the same trigger
// livengine.Session uses (evaluate after every bar at a timestamp has run OnBar).
// Flush() captures the final, still-open timestamp at end-of-run.
type intentRecordingProxy struct {
	inner  engine.Strategy
	eval   engine.SignalEvaluator
	recs   *[]livengine.SignalRecord
	curTS  time.Time
	haveTS bool
}

func newSignalRecordingProxy(inner engine.Strategy, recs *[]livengine.SignalRecord) *intentRecordingProxy {
	ie, ok := inner.(engine.SignalEvaluator)
	if !ok {
		panic("crosspath_test: inner strategy must implement engine.SignalEvaluator")
	}
	return &intentRecordingProxy{inner: inner, eval: ie, recs: recs}
}

func (p *intentRecordingProxy) ID() string { return p.inner.ID() }

// OnBar records the prior timestamp's intent on a rollover, then delegates the
// bar to the inner strategy UNCHANGED (no behaviour added on the trading path).
func (p *intentRecordingProxy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	if p.haveTS && bar.TS.After(p.curTS) {
		p.snapshot(p.curTS)
	}
	p.curTS = bar.TS
	p.haveTS = true
	return p.inner.OnBar(sub, bar)
}

// Flush records the final (still-open) timestamp's intent after the run drains —
// the analogue of livengine.Session flushing the last timestamp at end-of-stream.
func (p *intentRecordingProxy) Flush() {
	if p.haveTS {
		p.snapshot(p.curTS)
		p.haveTS = false
	}
}

func (p *intentRecordingProxy) snapshot(asOf time.Time) {
	*p.recs = append(*p.recs, livengine.SignalRecord{
		StrategyID: p.inner.ID(),
		AsOf:       asOf,
		Payload:    p.eval.EvaluateSignalJSON(asOf),
	})
}

// canonicalIntentStream JSON-canonicalizes intent records into comparable
// (AsOf, StrategyID, payloadJSON) tuples — the SAME equality the live
// consistency proof uses (the JSON payload is the persistence/transport shape, so
// JSON equality is the right equality). Defined separately from
// consistency_test.go's helper so this file is self-contained.
func canonicalIntentStream(t *testing.T, recs []livengine.SignalRecord) []string {
	t.Helper()
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r.Payload)
		require.NoError(t, err)
		out = append(out, r.AsOf.UTC().Format(time.RFC3339Nano)+"|"+r.StrategyID+"|"+string(b))
	}
	return out
}

// assembleSector builds a fresh signal-capable SectorRotation assembly (one ETF
// per momentum name, rising so a real rebalance fires). A fresh assembly per
// call gives each dispatch driver its OWN generator state.
func assembleSector(t *testing.T) *strategyassembly.Assembly {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Composition:     mustSeed(t, "sector-only"),
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: wideSectorParams()},
	})
	require.NoError(t, err)
	return asm
}

// TestBatchEqualsStreamingIntents is the F3 cross-path equivalence guard: the
// per-timestamp signals produced when the SAME assembly is driven by the
// BATCH engine (engine.New + Run) equal those produced when it is driven by the
// STREAMING live session (Session.RunStream over a VirtualClock) AND by the
// EOD-replay live session (Session.Replay) over the same bars.
func TestBatchEqualsStreamingIntents(t *testing.T) {
	instruments := sectorInstruments()
	flat := livengine.BatchBars(instruments)
	require.NotEmpty(t, flat)

	syms := make([]string, 0, len(instruments))
	for _, ib := range instruments {
		syms = append(syms, ib.Symbol)
	}

	// ----- (A) BATCH driver: engine.New + Engine.Run -----------------------
	batchAsm := assembleSector(t)
	var batchRecs []livengine.SignalRecord
	proxies := make([]engine.Strategy, 0, len(batchAsm.Strategies))
	rawProxies := make([]*intentRecordingProxy, 0, len(batchAsm.Strategies))
	for _, st := range batchAsm.Strategies {
		p := newSignalRecordingProxy(st, &batchRecs)
		proxies = append(proxies, p)
		rawProxies = append(rawProxies, p)
	}
	eng, err := engine.New(context.Background(), engine.Config{
		Tickers:            syms,
		Start:              calendar.Date{Year: 2024, Month: time.January, Day: 1},
		End:                calendar.Date{Year: 2024, Month: time.February, Day: 2},
		StartingBalance:    domain.MustMoney("100000"),
		Profile:            engine.ProfileCloseFill,
		PrebuiltStrategies: proxies,
		Gate:               batchAsm.Gate,
		Context:            batchAsm.Context,
		SPYSymbol:          batchAsm.SPYSymbol,
	}, engine.SliceFeed{Instruments: instruments})
	require.NoError(t, err)
	_, err = eng.Run(context.Background())
	require.NoError(t, err)
	for _, p := range rawProxies {
		p.Flush() // capture the final still-open timestamp's intent
	}
	require.NotEmpty(t, batchRecs, "batch driver must emit intents (8 ETFs * 4 timestamps)")

	// ----- (B) STREAMING driver: Session.RunStream (VirtualClock) ----------
	streamSink := livengine.NewMemSink()
	streamSess := buildSectorSession(t, streamSink)
	require.NoError(t, streamSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, streamSess.RunStream(context.Background(),
		livengine.SliceStreamFeed{Bars: flat, Buffer: 4}, core.StreamVirtual, vc))

	// ----- (C) EOD-replay driver: Session.Replay ---------------------------
	replaySink := livengine.NewMemSink()
	replaySess := buildSectorSession(t, replaySink)
	require.NoError(t, replaySess.Prime(context.Background()))
	require.NoError(t, replaySess.Replay(context.Background(), flat))

	// ----- canonical compare: all three intent streams must be IDENTICAL ----
	batchCanon := canonicalIntentStream(t, sortIntentRecs(batchRecs))
	streamCanon := canonicalIntentStream(t, streamSink.SortedIntents())
	replayCanon := canonicalIntentStream(t, replaySink.SortedIntents())

	require.Equal(t, len(streamCanon), len(batchCanon),
		"batch and streaming must emit the same NUMBER of per-timestamp intents")
	assert.Equal(t, streamCanon, batchCanon,
		"F3: batch (engine.New) intents must equal streaming (Session.RunStream) intents")
	assert.Equal(t, streamCanon, replayCanon,
		"sanity: streaming and EOD-replay intents must match (pre-existing invariant)")
}

// sortIntentRecs orders intent records canonically (AsOf, then StrategyID) — the
// same canonicalization MemSink.SortedIntents applies — so the batch stream
// compares apples-to-apples with the live sinks regardless of emission order.
func sortIntentRecs(recs []livengine.SignalRecord) []livengine.SignalRecord {
	out := make([]livengine.SignalRecord, len(recs))
	copy(out, recs)
	// stable insertion to mirror sort.SliceStable in MemSink.SortedIntents.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			less := false
			if !a.AsOf.Equal(b.AsOf) {
				less = a.AsOf.Before(b.AsOf)
			} else {
				less = a.StrategyID <= b.StrategyID
			}
			if less {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
