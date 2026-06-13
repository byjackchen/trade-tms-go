package metrics

// stitch.go ports the walk-forward equity-curve stitching and metric
// re-computation of src/research/workers.py:83-146 (spec §4, [MUST-MATCH]).
// Each fold's backtest starts fresh from the same starting balance;
// aggregation concatenates per-period RETURNS (not balances) into one
// continuous curve and recomputes the metrics over it (never averages
// per-fold values), so sharpe and calmar describe the same return sequence.

// Stitch concatenates fold curves into one continuous equity curve seeded at
// startingBalance (spec §4.1; workers.py:83-109):
//
//	stitched = [startingBalance]
//	for curve in foldCurves:
//	    if len(curve) < 2: continue           # degenerate folds contribute nothing
//	    for (prev, cur) in pairwise(curve):
//	        if prev == 0: continue
//	        ret = (cur - prev)/prev
//	        stitched = append(stitched, stitched[-1] * (1 + ret))
//
// The result always has >= 1 point. Fold boundaries introduce no artificial
// returns (the first point of each fold is only a denominator).
func Stitch(foldCurves [][]float64, startingBalance float64) []float64 {
	stitched := []float64{startingBalance}
	for _, curve := range foldCurves {
		if len(curve) < 2 {
			continue
		}
		for i := 1; i < len(curve); i++ {
			prev, cur := curve[i-1], curve[i]
			if prev == 0 {
				continue
			}
			ret := (cur - prev) / prev
			stitched = append(stitched, stitched[len(stitched)-1]*(1+ret))
		}
	}
	return stitched
}

// AggregateFolds recomputes BacktestMetrics over the stitched curve and SUMS
// the four counters across folds (spec §4.2; workers.py:112-146). The starting
// balance is recovered from fold 0 as final_balance - total_pnl. Folds whose
// metrics carry the per-fold counters are summed; the curve-derived metrics
// (sharpe, calmar, max_drawdown_pct) come from the stitched curve, NOT averages.
//
// foldMetrics[i] must correspond to foldCurves[i] (same fold order). At least
// one fold is required.
func AggregateFolds(foldCurves [][]float64, foldMetrics []BacktestMetrics) BacktestMetrics {
	if len(foldMetrics) == 0 {
		return BacktestMetrics{}
	}
	starting := foldMetrics[0].FinalBalanceUSD - foldMetrics[0].TotalPnLUSD
	equity := Stitch(foldCurves, starting)

	var counts Counts
	for _, m := range foldMetrics {
		counts.NumOrders += m.NumOrders
		counts.NumFilledOrders += m.NumFilledOrders
		counts.NumRejectedOrders += m.NumRejectedOrders
		counts.NumPositions += m.NumPositions
	}
	final := equity[len(equity)-1]
	return Compute(equity, starting, final, counts)
}
