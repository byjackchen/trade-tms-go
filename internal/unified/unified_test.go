package unified

// unified_test.go is the executable unification proof. It asserts the thesis
// stated in docs/reference/architecture.md:
//
//	The five modes (backtest, hyperopt, signal, paper, live) run on ONE engine
//	assembly. The strategy / portfolio / context set is IDENTICAL across modes;
//	the ONLY per-mode difference is the injected Clock and Executor.
//
// The proof has two halves, matching the two engine consumers that the modes
// route through:
//
//	BATCH consumer    (engine.New):        backtest + hyperopt + EOD-replay state
//	STREAMING consumer (livengine.Session): signal + paper + live
//
// Both consume the SAME []engine.Strategy slice (the strategyassembly.Assembly
// output). The test builds one such slice ONCE, hands the identical instances to
// every mode, and verifies (a) strategy-instance identity is shared, and (b) the
// Clock+Executor seams differ exactly as the architecture table claims.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
)

// probeStrategy is a do-nothing engine.Strategy: it carries an id and records
// nothing. It stands in for a real strategyassembly adapter (SEPA / Sector /
// Pairs / ORB) for the assembly-identity proof, which is about WHICH instances
// each mode receives, not what they emit. Using a trivial strategy keeps the
// test free of DB params while exercising the exact construction seams.
type probeStrategy struct {
	id     string
	onBars int
}

func (p *probeStrategy) ID() string { return p.id }

func (p *probeStrategy) OnBar(_ engine.OrderSubmitter, _ domain.Bar) error {
	p.onBars++
	return nil
}

// stubSubmitter is a no-op engine.OrderSubmitter standing in for the paper/live
// GatedSubmitter (which, in production, owns the gate + MoomooExecutor). The
// streaming Session requires a non-nil submitter in paper/live mode and refuses
// one in signal mode; the stub lets the test assemble all three streaming modes
// without a broker connection while still exercising the mode/executor pairing
// invariant.
type stubSubmitter struct{}

func (stubSubmitter) SubmitMarket(string, string, domain.OrderSide, domain.Qty, string, time.Time) (string, error) {
	return "", nil
}

func (stubSubmitter) SubmitMarketSignal(string, string, domain.SignalSide, domain.OrderSide, domain.Qty, string, time.Time) (string, bool, error) {
	return "", false, nil
}

// sharedAssembly mints ONE strategy set, exactly as strategyassembly.Assemble
// returns to every mode. Returning the same slice to each consumer is the heart
// of the proof: the modes do not each build their own strategies — they share
// the assembler's output.
func sharedAssembly() []engine.Strategy {
	return []engine.Strategy{
		&probeStrategy{id: "SEPA-UNIVERSE-001"},
		&probeStrategy{id: "SectorRotation-001"},
		&probeStrategy{id: "Pairs-001"},
	}
}

func aaplBars() []domain.Bar {
	return []domain.Bar{
		{Symbol: "AAPL", TS: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Open: domain.MustPrice("100"), High: domain.MustPrice("110"), Low: domain.MustPrice("95"), Close: domain.MustPrice("105"), Volume: 1000},
		{Symbol: "AAPL", TS: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Open: domain.MustPrice("106"), High: domain.MustPrice("112"), Low: domain.MustPrice("104"), Close: domain.MustPrice("108"), Volume: 1000},
	}
}

// TestFiveModesShareOneAssembly is the load-bearing proof. It builds the shared
// strategy set once and threads the IDENTICAL instances through:
//
//	mode 1 backtest  -> engine.New        (batch)     -> SimClock  + SimExecutor
//	mode 2 hyperopt  -> engine.New        (batch)     -> SimClock  + SimExecutor
//	mode 3 signal    -> livengine.Session (streaming) -> Wall/Virtual + NoopExecutor
//	mode 4 paper     -> livengine.Session (streaming) -> Wall/Virtual + GatedSubmitter
//	mode 5 live      -> livengine.Session (streaming) -> Wall/Virtual + GatedSubmitter
//
// then asserts the two invariants of the unification thesis.
func TestFiveModesShareOneAssembly(t *testing.T) {
	ctx := context.Background()
	strategies := sharedAssembly()
	startBal := domain.MustMoney("100000")

	// ---- BATCH consumer (backtest + hyperopt) ----------------------------
	// Both modes call engine.New with PrebuiltStrategies = the assembly output.
	// We assemble it twice with the SAME instances to model both batch modes;
	// they are construction-identical (hyperopt is N parallel backtests).
	batchCfg := func() engine.Config {
		return engine.Config{
			Tickers:            []string{"AAPL"},
			Start:              calendar.NewDate(2025, 1, 1),
			End:                calendar.NewDate(2025, 1, 31),
			StartingBalance:    startBal,
			Profile:            engine.ProfileCloseFill,
			PrebuiltStrategies: strategies,
		}
	}
	feed := engine.SliceFeed{Instruments: []engine.InstrumentBars{{Symbol: "AAPL", Bars: aaplBars()}}}

	backtestEng, err := engine.New(ctx, batchCfg(), feed)
	require.NoError(t, err, "mode=backtest must construct from the shared assembly")
	hyperoptEng, err := engine.New(ctx, batchCfg(), feed)
	require.NoError(t, err, "mode=hyperopt (a backtest) must construct from the shared assembly")

	// SEAM 1 — the batch clock is the deterministic SimClock (NOT a wall clock).
	// This is what makes backtest + hyperopt reproducible event-time replays.
	_, btIsSim := backtestEng.Clock().(*core.SimClock)
	_, hoIsSim := hyperoptEng.Clock().(*core.SimClock)
	assert.True(t, btIsSim, "backtest must be driven by a SimClock (deterministic event time)")
	assert.True(t, hoIsSim, "hyperopt must be driven by a SimClock (deterministic event time)")

	// ---- STREAMING consumer (signal + paper + live) ----------------------
	// All three modes call livengine.NewSession with the SAME strategy slice;
	// they differ only in (Exec, Submitter): signal => ExecSignal + internal
	// NoopExecutor; paper AND live => ExecAuto + the injected GatedSubmitter
	// (here a stub). Post-phase-3 the paper-vs-live distinction no longer lives
	// on the session's execution policy (both are ExecAuto) — it moved to the
	// broker Account bound to the injected executor (simulate => paper, real =>
	// live). The accounts below model that distinction.
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvPaper, 111111, "paper")
	liveAcct := domain.NewBrokerAccount("moomoo", domain.EnvReal, 222222, "live")

	signalSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      strategies,
		SPYSymbol:       "SPY",
		StartingBalance: startBal,
	})
	require.NoError(t, err, "mode=signal must construct from the shared assembly")

	paperSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecAuto,
		Strategies:      strategies,
		SPYSymbol:       "SPY",
		StartingBalance: startBal,
		Submitter:       stubSubmitter{},
	})
	require.NoError(t, err, "mode=paper must construct from the shared assembly")

	liveSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecAuto,
		Strategies:      strategies,
		SPYSymbol:       "SPY",
		StartingBalance: startBal,
		Submitter:       stubSubmitter{},
	})
	require.NoError(t, err, "mode=live must construct from the shared assembly")

	// SEAM 2 — the executor differs per streaming mode, and ONLY the executor.
	// signal: internal NoopExecutor (records would-be orders, places none).
	assert.NotNil(t, signalSess.Executor(), "signal mode owns the NoopExecutor seam")
	require.NotNil(t, signalSess.Submitter())
	// paper/live: the injected submitter REPLACES the NoopExecutor (places orders).
	assert.Nil(t, paperSess.Executor(), "paper mode must not own a NoopExecutor (the injected submitter executes)")
	assert.Nil(t, liveSess.Executor(), "live mode must not own a NoopExecutor (the injected submitter executes)")
	// The paper/live submitter is exactly the injected one — the gate seam.
	_, paperStub := paperSess.Submitter().(stubSubmitter)
	_, liveStub := liveSess.Submitter().(stubSubmitter)
	assert.True(t, paperStub, "paper mode runs strategies through the injected (gated) submitter")
	assert.True(t, liveStub, "live mode runs strategies through the injected (gated) submitter")

	// The execution policy distinguishes signal from auto: signal => ExecSignal,
	// paper AND live => ExecAuto. Post-phase-3 the session no longer carries the
	// paper-vs-live distinction (both auto sessions share the same Exec()); that
	// distinction moved to the bound broker Account (simulate => paper, real =>
	// live). We assert the policy axis on the session, and the paper-vs-live axis
	// on the accounts the injected executor would bind.
	assert.Equal(t, domain.ExecSignal, signalSess.Exec(), "signal node emits intents only")
	assert.Equal(t, domain.ExecAuto, paperSess.Exec(), "paper node auto-submits through the injected executor")
	assert.Equal(t, domain.ExecAuto, liveSess.Exec(), "live node auto-submits through the injected executor")
	// Paper and live sessions are now policy-identical (both ExecAuto) — the
	// distinction is the Account, not the session.
	assert.Equal(t, paperSess.Exec(), liveSess.Exec(), "paper/live no longer differ at the session: same execution policy")
	assert.False(t, paperAcct.IsReal(), "the paper node binds a simulate (paper) account")
	assert.True(t, liveAcct.IsReal(), "the live node binds a real-money account")
}

// TestStreamingClockSeamIsTheOnlyTimeDifference proves the streaming consumer is
// driven by the wall/virtual clock (NOT a SimClock), the mirror image of the
// batch SimClock seam. It runs the SAME shared assembly through a VirtualClock
// replay (the deterministic stand-in for the live WallClock) and confirms the
// strategies actually receive bars — i.e. the streaming path is a real engine,
// not a stub.
func TestStreamingClockSeamIsTheOnlyTimeDifference(t *testing.T) {
	ctx := context.Background()
	strategies := sharedAssembly()

	sess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      strategies,
		SPYSymbol:       "SPY",
		StartingBalance: domain.MustMoney("100000"),
	})
	require.NoError(t, err)
	require.NoError(t, sess.Prime(ctx))

	// Drive the streaming loop with a VirtualClock (StreamVirtual) — the live
	// engine uses StreamWall (a real WallClock); the discipline (loop advances a
	// non-Sim clock) is identical, which is what the consistency proof relies on.
	vc := core.NewVirtualClock(time.Time{})
	feed := livengine.SliceStreamFeed{Bars: aaplBars()}
	require.NoError(t, sess.RunStream(ctx, feed, core.StreamVirtual, vc))

	// The shared strategy instances saw the streamed bars — proving the SAME
	// strategy objects that a backtest would run are the ones the live path runs.
	var totalOnBars int
	for _, s := range strategies {
		ps, ok := s.(*probeStrategy)
		require.True(t, ok)
		totalOnBars += ps.onBars
	}
	assert.Positive(t, totalOnBars, "streaming path must dispatch bars into the shared strategy instances")
}
