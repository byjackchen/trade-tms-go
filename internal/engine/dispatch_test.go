package engine

// dispatch_test.go pins fix-4: the symbol-indexed strategy dispatch
// (buildDispatch + handleBar) hits EXACTLY the same strategies, in the SAME
// registration order, that the old per-bar full scan would have hit — only
// cheaper. The guard compares the indexed dispatch against an independently
// computed full-scan reference (every strategy offered every bar, self-filtered
// by its declared symbol scope), so a regression that drops or reorders a bar
// is caught.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// dispatchObservation is one OnBar delivery: which strategy saw which bar.
type dispatchObservation struct {
	strategyID string
	symbol     string
	ts         time.Time
}

// recordingStrategy is a behaviour-transparent engine.Strategy double. OnBar
// records the (strategy, symbol, ts) it receives into a SHARED ordered log, so
// the log preserves the exact cross-strategy dispatch ORDER the engine used at
// each bar. It submits NO orders (the dispatch order is independent of trading).
//
// When scope is non-nil it implements SymbolScoped (declaring exactly those
// symbols); when scope is nil it does NOT, so the engine must broadcast every
// bar to it (the unscoped fallback). It SELF-FILTERS in OnBar exactly as a real
// generator does (a bar outside its scope is a no-op), so the recorded log is
// the set of bars it actually reacted to — the quantity the optimization must
// preserve.
type recordingStrategy struct {
	id    string
	scope map[string]struct{} // nil => unscoped (no SymbolScoped, reacts to all)
	log   *[]dispatchObservation
}

func (r *recordingStrategy) ID() string { return r.id }

func (r *recordingStrategy) OnBar(_ OrderSubmitter, bar domain.Bar) error {
	if r.scope != nil {
		if _, ok := r.scope[bar.Symbol]; !ok {
			return nil // out of scope: real generators no-op here
		}
	}
	*r.log = append(*r.log, dispatchObservation{strategyID: r.id, symbol: bar.Symbol, ts: bar.TS})
	return nil
}

// SymbolsScoped is provided ONLY when scope is non-nil. A nil-scope strategy
// must NOT satisfy SymbolScoped, so we keep this method but a nil-scope instance
// is wrapped in unscopedStrategy (below) which hides it.
func (r *recordingStrategy) SymbolsScoped() []string {
	out := make([]string, 0, len(r.scope))
	for s := range r.scope {
		out = append(out, s)
	}
	return out
}

// unscopedStrategy wraps a recordingStrategy WITHOUT promoting SymbolScoped, so
// the engine treats it as a broadcast (full-scan) strategy. It forwards ID/OnBar
// verbatim.
type unscopedStrategy struct{ inner *recordingStrategy }

func (u unscopedStrategy) ID() string                                 { return u.inner.ID() }
func (u unscopedStrategy) OnBar(s OrderSubmitter, b domain.Bar) error { return u.inner.OnBar(s, b) }

func scoped(id string, log *[]dispatchObservation, syms ...string) *recordingStrategy {
	set := make(map[string]struct{}, len(syms))
	for _, s := range syms {
		set[s] = struct{}{}
	}
	return &recordingStrategy{id: id, scope: set, log: log}
}

// TestIndexedDispatchEqualsFullScan asserts the engine's symbol-indexed dispatch
// produces the SAME (strategy, symbol, ts) delivery log — in the same order — as
// an explicit full scan (every strategy offered every bar in registration order,
// self-filtering). It mixes single-symbol scoped, multi-symbol scoped, and
// unscoped (broadcast) strategies, with overlapping symbols, to exercise the
// registration-order merge.
func TestIndexedDispatchEqualsFullScan(t *testing.T) {
	syms := []string{"AAA", "BBB", "CCC", "DDD"}
	instruments := make([]InstrumentBars, 0, len(syms))
	rows := []barRow{
		{2025, 1, 2, "100", "110", "95", "105", 1000},
		{2025, 1, 3, "105", "112", "104", "108", 1000},
		{2025, 1, 6, "108", "109", "100", "101", 1000},
	}
	for _, s := range syms {
		instruments = append(instruments, mkBars(s, rows))
	}

	// --- engine (indexed dispatch) delivery log -----------------------------
	var got []dispatchObservation
	// Registration order of strategies is the slice order below. Mix of scopes:
	//   0: scoped single AAA
	//   1: scoped multi  BBB+CCC
	//   2: UNSCOPED (broadcast — reacts to every symbol)
	//   3: scoped single CCC (overlaps strategy 1 on CCC)
	//   4: scoped multi  AAA+DDD (overlaps strategy 0 on AAA)
	prebuilt := []Strategy{
		scoped("S0-AAA", &got, "AAA"),
		scoped("S1-BBB-CCC", &got, "BBB", "CCC"),
		unscopedStrategy{inner: scoped("S2-ALL", &got)}, // scope map unused (hidden)
		scoped("S3-CCC", &got, "CCC"),
		scoped("S4-AAA-DDD", &got, "AAA", "DDD"),
	}
	// Strategy 2 is unscoped: make its self-filter accept everything (nil scope).
	prebuilt[2].(unscopedStrategy).inner.scope = nil

	cfg := Config{
		Tickers:            syms,
		Start:              calendar.NewDate(2025, 1, 1),
		End:                calendar.NewDate(2025, 1, 31),
		StartingBalance:    domain.MustMoney("100000"),
		Profile:            ProfileCloseFill,
		PrebuiltStrategies: prebuilt,
	}
	eng, err := New(context.Background(), cfg, SliceFeed{Instruments: instruments})
	require.NoError(t, err)
	_, err = eng.Run(context.Background())
	require.NoError(t, err)

	// --- reference full-scan delivery log -----------------------------------
	// Reproduce the OLD per-bar full scan: for each bar (registration-order
	// instruments, ascending ts — the engine's queue order) offer it to EVERY
	// strategy in registration order; the strategy self-filters. This is exactly
	// the loop the optimization replaced, so equality proves behaviour preserved.
	want := fullScanLog(instruments, prebuilt)

	require.Equal(t, len(want), len(got),
		"indexed dispatch must deliver the same NUMBER of (strategy,bar) pairs as a full scan")
	assert.Equal(t, want, got,
		"indexed dispatch must hit the same strategies in the same order as a full scan")

	// Sanity: the unscoped strategy saw every bar; a scoped one saw only its
	// symbols — so the optimization is actually narrowing dispatch, not a no-op.
	assert.Equal(t, len(syms)*len(rows), countFor(got, "S2-ALL"),
		"unscoped strategy must receive every bar")
	assert.Equal(t, len(rows), countFor(got, "S0-AAA"),
		"single-symbol scoped strategy must receive only its symbol's bars")
}

// fullScanLog computes the delivery log the pre-optimization full scan would
// produce, in the engine's ACTUAL event-queue order: the loop orders bars by
// (ts, registration-seq), so at each timestamp it dispatches the instruments in
// registration order, and for each bar offers it to EVERY strategy in
// registration order (the strategy self-filters). This mirrors the deterministic
// queue (seed schedules instrument-by-instrument in registration order, so
// same-ts bars carry ascending seq), so equality with the indexed dispatch proves
// the optimization is order-preserving. Assumes each instrument carries the same
// dense set of timestamps (true for this test's shared rows).
func fullScanLog(instruments []InstrumentBars, strategies []Strategy) []dispatchObservation {
	var log []dispatchObservation
	nbars := 0
	if len(instruments) > 0 {
		nbars = len(instruments[0].Bars)
	}
	for k := 0; k < nbars; k++ { // timestamp index (ascending)
		for _, ib := range instruments { // registration order
			bar := ib.Bars[k]
			for _, st := range strategies { // registration order
				if !fullScanReacts(st, bar.Symbol) {
					continue
				}
				log = append(log, dispatchObservation{strategyID: st.ID(), symbol: bar.Symbol, ts: bar.TS})
			}
		}
	}
	return log
}

// fullScanReacts reports whether a full scan would have this strategy react to a
// bar of sym: an unscoped strategy (no SymbolScoped) reacts to ALL symbols; a
// scoped one reacts iff sym is in its declared set.
func fullScanReacts(st Strategy, sym string) bool {
	ss, ok := st.(SymbolScoped)
	if !ok {
		return true
	}
	for _, s := range ss.SymbolsScoped() {
		if s == sym {
			return true
		}
	}
	return false
}

func countFor(log []dispatchObservation, id string) int {
	n := 0
	for _, o := range log {
		if o.strategyID == id {
			n++
		}
	}
	return n
}

// TestBuildDispatchOrderIsRegistration unit-tests buildDispatch directly: each
// dispatch[sym] slice is in ascending registration-index order, regardless of
// the scope-declaration order, and broadcast strategies precede/interleave by
// index — never by map iteration.
func TestBuildDispatchOrderIsRegistration(t *testing.T) {
	var log []dispatchObservation
	e := &Engine{
		registrationIx: map[string]int{},
		strategies: []Strategy{
			scoped("0", &log, "X"), // idx 0 scoped X
			unscopedStrategy{inner: &recordingStrategy{id: "1-bc", log: &log}}, // idx 1 broadcast
			scoped("2", &log, "X", "Y"),                                        // idx 2 scoped X,Y
			scoped("3", &log, "Y"),                                             // idx 3 scoped Y
		},
		registration: []string{"X", "Y", "Z"},
	}
	e.buildDispatch()

	// X: broadcast{1} + scoped{0,2} merged by index => [0,1,2].
	assert.Equal(t, []int{0, 1, 2}, e.dispatch["X"], "X dispatch must be registration-ordered")
	// Y: broadcast{1} + scoped{2,3} => [1,2,3].
	assert.Equal(t, []int{1, 2, 3}, e.dispatch["Y"], "Y dispatch must be registration-ordered")
	// Z: only broadcast{1}.
	assert.Equal(t, []int{1}, e.dispatch["Z"], "Z dispatch must be broadcast-only")
	// broadcast set is the single unscoped strategy at idx 1.
	assert.Equal(t, []int{1}, e.broadcast, "broadcast must hold the unscoped strategy index")

	// An unregistered symbol falls back to broadcast (a full scan would too).
	assert.Equal(t, []int{1}, e.strategiesFor("UNREGISTERED"))
	assert.Equal(t, []int{0, 1, 2}, e.strategiesFor("X"))
}
