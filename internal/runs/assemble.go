package runs

// assemble.go turns an engine.Result into the two persistence forms the result
// plane needs: a PersistInput (the DB source of truth) and an ArtifactInput
// (the legacy runs/{ts}/*.json set). It computes the portfolio + per-strategy
// metrics from the sampled equity curves (internal/metrics) and extracts
// round-trip trades from the fill stream (trades.go).
//
// Equity curve -> metrics float64 conversion: the metrics package operates on
// []float64. We convert each sampled Money point to its exact float64 (the
// Decimal(str) bridge), so the metric inputs match what the Python reference
// computes from its own float curve (hyperopt spec §1.6).

import (
	"encoding/json"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
)

// Assembled bundles the two persistence forms produced from one engine.Result.
type Assembled struct {
	Persist  PersistInput
	Artifact ArtifactInput
}

// AssembleParams supplies the run identity and config the engine.Result does
// not carry.
type AssembleParams struct {
	RunTS     string
	Kind      string
	StartDate calendar.Date
	EndDate   calendar.Date
	Config    json.RawMessage // run params (stored verbatim in tms.runs.config)
	// RegimeSamples / StrategySummary are optional artifact-only extras.
	RegimeSamples   map[string]int
	StrategySummary map[string]map[string]any
}

// Assemble computes metrics + trades from res and builds both persistence
// forms. The portfolio equity curve is the engine's TotalEquityCurve (account
// equity per sampled day); per-strategy curves are the cumulative-PnL samples.
//
// Per-strategy metric curves are mark-to-market account equity, reconstructed
// as starting_balance + strategy_cumulative_pnl so sharpe/calmar/max_drawdown
// describe a balance series (matching the reference, which marks each strategy
// to its own balance, §1.6). The portfolio metric curve is the account equity
// directly.
func Assemble(res *engine.Result, p AssembleParams) (Assembled, error) {
	startFloat := res.StartingBalance.Float64()

	// Portfolio metric curve from the total equity samples.
	portCurve := curveFloats(res.TotalEquityCurve)
	portCounts := countsFor(res, "") // portfolio: all orders/positions
	portMetrics := metrics.Compute(portCurve, startFloat, res.FinalBalance.Float64(), portCounts)

	// Per-strategy metric curves: starting balance + cumulative strategy PnL.
	stratMetrics := make(map[string]metrics.BacktestMetrics)
	stratArtifactEquity := make(map[string][]EquityPoint)
	persistStratEquity := make(map[string][]EquityPoint)
	for _, sid := range SortedKeys(res.StrategyEquity) {
		pts := res.StrategyEquity[sid]
		bal := make([]float64, len(pts))
		eqPoints := make([]EquityPoint, len(pts))
		for i, pt := range pts {
			bal[i] = startFloat + pt.Value.Float64()
			// The persisted/artifact per-strategy curve carries the cumulative
			// realized+unrealized PnL in USD (api spec §7.7), not the synthetic
			// balance — keep the engine's value.
			eqPoints[i] = EquityPoint{TS: pt.TS, BalanceUSD: pt.Value}
		}
		finalBal := startFloat
		if len(bal) > 0 {
			finalBal = bal[len(bal)-1]
		}
		counts := countsFor(res, sid)
		stratMetrics[sid] = metrics.Compute(bal, startFloat, finalBal, counts)
		stratArtifactEquity[sid] = eqPoints
		persistStratEquity[sid] = eqPoints
	}

	trades, err := ExtractTrades(res.Fills)
	if err != nil {
		return Assembled{}, err
	}

	portEquity := make([]EquityPoint, len(res.TotalEquityCurve))
	for i, pt := range res.TotalEquityCurve {
		portEquity[i] = EquityPoint{TS: pt.TS, BalanceUSD: pt.Value}
	}

	kind := p.Kind
	if kind == "" {
		kind = "multi-strategy"
	}

	persist := PersistInput{
		RunTS:            p.RunTS,
		Kind:             kind,
		Status:           "COMPLETE",
		StartDate:        p.StartDate,
		EndDate:          p.EndDate,
		StartingBalance:  res.StartingBalance,
		FinalBalance:     res.FinalBalance,
		TotalPnL:         res.TotalPnL,
		Strategies:       res.Strategies,
		Config:           p.Config,
		PortfolioMetrics: portMetrics,
		StrategyMetrics:  stratMetrics,
		PortfolioEquity:  portEquity,
		StrategyEquity:   persistStratEquity,
		Trades:           trades,
		Orders:           res.Orders,
	}

	artifact := FromEngineResult(p.RunTS, kind, p.StartDate.String(), p.EndDate.String(),
		res, p.RegimeSamples, p.StrategySummary)

	return Assembled{Persist: persist, Artifact: artifact}, nil
}

// countsFor returns the order/position counters scoped to strategyID ("" =
// portfolio: all), delegating to engine.Result.Counts — the single source of
// truth shared with the P4 hyperopt objective path so identical Results always
// produce identical counters. num_filled counts orders that produced at least
// one fill; rejected counts submitted REJECTED orders plus gate-blocked signal
// orders; num_positions counts opened positions.
func countsFor(res *engine.Result, strategyID string) metrics.Counts {
	c := res.Counts(strategyID)
	return metrics.Counts{
		NumOrders:         c.NumOrders,
		NumFilledOrders:   c.NumFilledOrders,
		NumRejectedOrders: c.NumRejectedOrders,
		NumPositions:      c.NumPositions,
	}
}

// curveFloats converts a Money equity curve to []float64 for the metrics
// package (exact Decimal(str) bridge per Money.Float64).
func curveFloats(pts []accounting.EquityPoint) []float64 {
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.Value.Float64()
	}
	return out
}
