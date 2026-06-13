//go:build parity_backtest

package study

// parity_backtest_test.go is the PERMANENT regression test that closes the gap
// the P3 suite missed: it proves the INTEGRATED real-strategy backtest path
// (engine event loop + strategy adapters + multi-strategy portfolio gate +
// equity sampler) — NOT just the pure signal.py layer — matches Python's
// scripts/multi_strategy_backtest integrated path over the SAME real fold
// window and the SAME real bars (tms.bars_daily).
//
// It runs the pairs strategy single-window under the canonical MULTI-strategy
// gate via the parity-correct path: the engine replays ONLY [start, end] (no
// warmup tail; pairs is intentionally NOT warmed), exactly mirroring Python's
// run-window-only Pairs loader. The objective Evaluator IS that path, so this
// guards the engine-level warmup-semantics fix permanently.
//
// Expected numbers are EMBEDDED from a fresh Python run of
// tmp/parity_pairs_harness.py (PairsRunner through a Nautilus BacktestEngine +
// _build_portfolio gate + EquityCurveSamplerActor), reading the SAME bars
// exported from tms.bars_daily. Re-derive with:
//
//	cd <PY repo>; .venv/bin/python <go repo>/tmp/parity_pairs_harness.py
//
// Run (compose stack up, postgres on TMS_PG_HOST/PORT):
//
//	TMS_PG_HOST=localhost TMS_PG_PORT=55432 TMS_PG_USER=tms TMS_PG_PASSWORD=tms \
//	  TMS_PG_DATABASE=tms go test -tags parity_backtest ./internal/hyperopt/study/ -run TestParityIntegratedBacktest -v
//
// Or: make parity-backtest

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// pyFold is one fold's embedded Python expectation (from parity_pairs_harness).
type pyFold struct {
	start, end                  string
	finalBalance, totalPnL      float64
	sharpe, calmar, maxDrawdown float64
	numOrders, numFilled        int
	numPositions                int
	equityLen                   int // expected NAV-curve length (sample count)
}

// Embedded from a fresh run of tmp/parity_pairs_harness.py over tms.bars_daily
// (pairs KO/PEP, MA/V, XOM/CVX; defaults lookback=60 entry_z=2 exit_z=0.5
// cap=0.30; canonical multi-strategy gate). gate_fold is the diagnosis case
// (window too short to warm lookback=60 -> 0 trades, flat curve); real_fold is
// the long fold where the pairs actually trade.
//
// The curve-derived metrics (sharpe/calmar/max_drawdown) are taken from the
// harness's NAV curve (account balance_total + open-position unrealized per
// heartbeat) — the mark-to-market equity Go's accounting samples (acct.Equity()
// = cash + unrealized), which reconciles to balance_total when flat. This is the
// apples-to-apples basis: Python's run_backtest *also* exposes a per-strategy
// EquityCurveSampler curve, but that sums Nautilus's NETTING-position
// realized_pnl, which (by avg-cost NETTING accounting) does NOT reconcile to
// balance_total — a Nautilus internal artifact Go's FIFO/cash accounting does
// not reproduce. The NAV curve isolates the ENGINE FLOW (fills + marks), which
// is what this regression pins; both bases agree on the gate-fold (no trades).
var (
	pyGateFold = pyFold{
		start: "2022-02-01", end: "2022-03-15",
		finalBalance: 100000.0, totalPnL: 0.0,
		sharpe: 0.0, calmar: 0.0, maxDrawdown: 0.0,
		numOrders: 0, numFilled: 0, numPositions: 0,
		equityLen: 30,
	}
	pyRealFold = pyFold{
		start: "2021-01-04", end: "2022-06-30",
		finalBalance: 97464.16, totalPnL: -2535.8399999999965,
		// NAV-curve metrics (cash + unrealized), reconciling to balance_total.
		sharpe: -1.0019757241974792, calmar: -0.5340069067044804,
		maxDrawdown: -3.2045505665854965,
		numOrders:   48, numFilled: 48, numPositions: 6,
		equityLen: 376,
	}
	// liq_fold pins the END-OF-RUN LIQUIDATION (Nautilus on_stop
	// close_all_positions; FIXER round-1 finding 1): a window ending 2021-06-30
	// whose terminal bar leaves XOM/CVX OPEN. Python's on_stop flattens both on
	// the last bar 2021-06-30 (CVX SELL 141 @104.74, XOM BUY 248 @63.08 — the two
	// extra orders O-7/O-8), yielding 8 orders / 4 positions / final 99136.37. A Go
	// engine WITHOUT the end-of-run flatten would leave the position open and
	// report 6 orders / final 99996.61 (the open position never realized) — so
	// this fold fails today and guards the liquidation permanently. Embedded from a
	// fresh run of tmp/parity_pairs_harness_liq.py over the same bars.
	pyLiqFold = pyFold{
		start: "2021-01-04", end: "2021-06-30",
		finalBalance: 99136.37, totalPnL: -863.6299999999901,
		// NAV-curve metrics (cash + unrealized); the terminal NAV sample marks the
		// open position to the same 2021-06-30 close the liquidation realizes at,
		// so it equals the liquidated final 99136.37 (curve length unchanged).
		sharpe: -1.7256069469003308, calmar: -1.520186862942825,
		maxDrawdown: -1.1586577101910764,
		numOrders:   8, numFilled: 8, numPositions: 4,
		equityLen: 124,
	}
)

func parityPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TMS_PG_HOST") == "" || os.Getenv("TMS_PG_PORT") == "" {
		t.Skip("parity_backtest: TMS_PG_HOST/TMS_PG_PORT not set (compose stack up?)")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestParityIntegratedBacktest proves the Go integrated pairs backtest matches
// the Python integrated pairs backtest over the same real bars and fold windows.
func TestParityIntegratedBacktest(t *testing.T) {
	ctx := context.Background()
	pool := parityPool(t, ctx)
	store := universe.NewStore(pool)
	feed := engine.NewStoreFeed(store)

	legs := []string{"CVX", "KO", "MA", "PEP", "V", "XOM"}
	// Load the shared dataset (the LoadDataset widens by the SPY warmup buffer,
	// but the parity-correct WindowFeed/objective path replays only the run
	// window, and pairs receives NO out-of-band warmup).
	ds, err := LoadDataset(ctx, feed, legs,
		calendar.NewDate(2020, 1, 1), calendar.NewDate(2023, 1, 1))
	require.NoError(t, err)
	// Skip if the DB lacks these bars (stack not seeded for this era).
	for _, l := range legs {
		if len(ds.bySym[l].Bars) == 0 {
			t.Skipf("parity_backtest: tms.bars_daily has no bars for %s — seed the stack first", l)
		}
	}

	pairsDefaults := map[string]any{
		"pairs":                []any{[]any{"KO", "PEP"}, []any{"MA", "V"}, []any{"XOM", "CVX"}},
		"lookback":             float64(60),
		"entry_z":              2.0,
		"exit_z":               0.5,
		"capital_per_pair_pct": 0.30,
		"timezone":             "America/New_York",
	}

	for _, want := range []pyFold{pyGateFold, pyRealFold, pyLiqFold} {
		want := want
		t.Run(want.start+".."+want.end, func(t *testing.T) {
			start := mustDate(t, want.start)
			end := mustDate(t, want.end)
			ev := newSingleWindowPairsEvaluator(t, ds, pairsDefaults, start, end, 100000.0)
			res, err := ev.Evaluate(ctx, emptyPairsDecoded())
			require.NoError(t, err)
			got := res.Aggregated

			// Final balance / PnL: EXACT (penny) — the realized cash flow the
			// identical fill sequence produces.
			requireClose(t, "final_balance", got.FinalBalanceUSD, want.finalBalance, 0.005)
			requireClose(t, "total_pnl", got.TotalPnLUSD, want.totalPnL, 0.005)
			// Curve-derived metrics on the NAV (cash+unrealized) curve: the curve
			// is built from the IDENTICAL fill sequence and identical 2-dp bar
			// closes, so these agree to within float summation/rounding noise
			// (~1e-6 on a ~1.0-magnitude Sharpe; the only slack is a sub-cent
			// per-bar unrealized-mark rounding between Go's domain.Money decimal
			// and Nautilus's Decimal). 1e-4 is comfortably tight yet float-robust.
			requireClose(t, "sharpe", got.Sharpe, want.sharpe, 1e-4)
			requireClose(t, "calmar", got.Calmar, want.calmar, 1e-4)
			requireClose(t, "max_drawdown_pct", got.MaxDrawdownPct, want.maxDrawdown, 1e-4)

			// Integer counters that ARE parity targets (num_rejected is NOT — see
			// docs/spec/hyperopt-metrics.md: Go counts allocator-gate drops,
			// Python venue-rejections; different denominator, informational only).
			require.Equal(t, want.numOrders, got.NumOrders, "num_orders")
			require.Equal(t, want.numFilled, got.NumFilledOrders, "num_filled_orders")
			require.Equal(t, want.numPositions, got.NumPositions, "num_positions")

			// Equity-curve length: the engine must sample EXACTLY one point per
			// trading day in [start, end] (the test window) — NOT the warmup tail.
			// This pins the equity-sampling-window fix (no warmup-period samples).
			// (Re-run the engine to inspect the raw curve, which Evaluate doesn't
			// surface in single-window mode.)
			require.Equal(t, want.equityLen, parityCurveLen(t, ctx, ds, pairsDefaults, start, end),
				"equity_curve length (test window only)")
		})
	}
}

func mustDate(t *testing.T, s string) calendar.Date {
	t.Helper()
	d, err := calendar.ParseDate(s)
	require.NoError(t, err)
	return d
}

func requireClose(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.10f, want %.10f (|Δ|=%.3g > tol %.3g)", name, got, want, math.Abs(got-want), tol)
	}
}

// parityCurveLen assembles + runs the same pairs backtest the Evaluator runs and
// returns the engine's raw equity-curve length, so the test can assert the
// engine samples one point per test-window trading day (no warmup-period points).
func parityCurveLen(t *testing.T, ctx context.Context, ds *Dataset, defaults map[string]any, start, end calendar.Date) int {
	t.Helper()
	pp, err := params.PairsFromMap(defaults)
	require.NoError(t, err)
	in := strategyassembly.Input{Strategy: "pairs", StartingBalance: 100000.0, MultiStrategyGate: true}
	in.Params.Pairs = pp
	asm, err := strategyassembly.Assemble(in)
	require.NoError(t, err)
	sb, err := domain.MoneyFromFloat64(100000.0)
	require.NoError(t, err)
	cfg := engine.Config{
		Start: start, End: end, StartingBalance: sb, Profile: engine.ProfileNautilusCompat,
		Portfolio: asm.Portfolio, PrebuiltStrategies: asm.Strategies, Tickers: asm.ExtraTickers,
	}
	eng, err := engine.New(ctx, cfg, ds.WindowFeed())
	require.NoError(t, err)
	asm.BindEquity(eng)
	res, err := eng.Run(ctx)
	require.NoError(t, err)
	return len(res.TotalEquityCurve)
}
