//go:build parity_folds

package study

// parity_folds_test.go drives the Go objective Evaluator over MULTIPLE real
// adjacent folds (the walk-forward objective path with return-stitching) and
// prints per-fold + stitched sharpe/calmar/maxDD for comparison against the
// Python research.workers pipeline (tmp/parity_folds_harness.py). Gate step 5.
//
// Run (compose stack up):
//   TMS_PG_HOST=localhost TMS_PG_PORT=55432 TMS_PG_USER=tms TMS_PG_PASSWORD=tms \
//     TMS_PG_DATABASE=tms go test -tags parity_folds ./internal/hyperopt/study/ \
//     -run TestParityFoldsObjective -v

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	x, err := time.Parse("2006-01-02", s)
	require.NoError(t, err)
	return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC)
}

func TestParityFoldsObjective(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("TMS_PG_HOST") == "" {
		t.Skip("needs TMS_PG_HOST")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	store := universe.NewStore(pool)
	feed := engine.NewStoreFeed(store)

	legs := []string{"CVX", "KO", "MA", "PEP", "V", "XOM"}
	ds, err := LoadDataset(ctx, feed, legs,
		calendar.NewDate(2020, 1, 1), calendar.NewDate(2023, 1, 1))
	require.NoError(t, err)

	pairsDefaults := map[string]any{
		"pairs":                []any{[]any{"KO", "PEP"}, []any{"MA", "V"}, []any{"XOM", "CVX"}},
		"lookback":             float64(60),
		"entry_z":              2.0,
		"exit_z":               0.5,
		"capital_per_pair_pct": 0.30,
		"timezone":             "America/New_York",
	}

	folds := []hyperopt.EvalSegment{
		{TestStart: mustTime(t, "2021-01-04"), TestEnd: mustTime(t, "2021-06-30")},
		{TestStart: mustTime(t, "2021-07-01"), TestEnd: mustTime(t, "2021-12-31")},
		{TestStart: mustTime(t, "2022-01-03"), TestEnd: mustTime(t, "2022-06-30")},
	}

	ev, err := NewEvaluator(EvaluatorConfig{
		Strategy:        "pairs",
		Dataset:         ds,
		Start:           calendar.NewDate(2021, 1, 4),
		End:             calendar.NewDate(2022, 6, 30),
		Folds:           folds,
		Defaults:        map[string]map[string]any{"pairs": pairsDefaults},
		StartingBalance: 100000.0,
	})
	require.NoError(t, err)

	res, err := ev.Evaluate(ctx, emptyPairsDecoded())
	require.NoError(t, err)

	out := map[string]any{}
	perFold := make([]map[string]any, 0, len(res.FoldMetrics))
	for i, m := range res.FoldMetrics {
		perFold = append(perFold, map[string]any{
			"start":                folds[i].TestStart.Format("2006-01-02"),
			"end":                  folds[i].TestEnd.Format("2006-01-02"),
			"nav_sharpe":           m.Sharpe,
			"nav_calmar":           m.Calmar,
			"nav_max_drawdown_pct": m.MaxDrawdownPct,
			"nav_final":            m.FinalBalanceUSD,
			"num_orders":           m.NumOrders,
			"num_filled_orders":    m.NumFilledOrders,
			"num_positions":        m.NumPositions,
		})
	}
	out["per_fold"] = perFold
	a := res.Aggregated
	out["aggregated"] = map[string]any{
		"stitched_final":            a.FinalBalanceUSD,
		"stitched_sharpe":           a.Sharpe,
		"stitched_calmar":           a.Calmar,
		"stitched_max_drawdown_pct": a.MaxDrawdownPct,
		"num_orders":                a.NumOrders,
		"num_filled_orders":         a.NumFilledOrders,
		"num_positions":             a.NumPositions,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println("GO_FOLDS_OUT_BEGIN")
	fmt.Println(string(b))
	fmt.Println("GO_FOLDS_OUT_END")
}
