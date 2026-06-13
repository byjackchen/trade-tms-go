package api

// handlers_strategies_test.go drives GET /api/v1/strategies and
// GET /api/v1/strategies/{id} against the embedded-baseline StrategyReader
// (no DB, no env dir) wired into newTestServer. These are contract tests:
// they assert the wire shape, the registry membership/order, the ORB id
// remap (loader stem intraday_breakout -> backtest token orb), and the
// resolved param schema (defaults + search bounds from the baseline JSON).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrategyReaderListBaseline(t *testing.T) {
	r := NewStrategyReader(nil, "")
	list, err := r.ListStrategies(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 4, "the four production strategies")

	// Fixed display order mirrors the engine (sepa, sector_rotation, pairs, orb).
	wantIDs := []string{"sepa", "sector_rotation", "pairs", "intraday_breakout"}
	wantBT := []string{"sepa", "sector_rotation", "pairs", "orb"}
	for i, m := range list {
		require.Equal(t, wantIDs[i], m.ID)
		require.Equal(t, wantBT[i], m.BacktestID)
		require.Equal(t, "baseline", m.ParamsSource)
		require.Empty(t, m.Error, "baseline must resolve cleanly")
		require.Greater(t, m.ParametersCount, 0)
		require.Len(t, m.Parameters, m.ParametersCount)
	}

	// SEPA baseline carries an allocation block (capital_pct 0.40, active).
	sepa := list[0]
	require.NotNil(t, sepa.CapitalPct)
	require.InDelta(t, 0.40, *sepa.CapitalPct, 1e-9)
	require.True(t, sepa.Active)
	require.Equal(t, "SEPA", sepa.Label)
	require.NotEmpty(t, sepa.Description)

	// ORB baseline (intraday_breakout) has no allocation block -> nil capital,
	// active defaults true.
	orb := list[3]
	require.Nil(t, orb.CapitalPct)
	require.True(t, orb.Active)
	require.Equal(t, "ORB", orb.Label)
}

func TestStrategyReaderGetUnknown(t *testing.T) {
	r := NewStrategyReader(nil, "")
	_, err := r.GetStrategy(context.Background(), "nope")
	require.ErrorIs(t, err, ErrStrategyNotFound)
}

func TestStrategyReaderSchemaBounds(t *testing.T) {
	r := NewStrategyReader(nil, "")
	m, err := r.GetStrategy(context.Background(), "sepa")
	require.NoError(t, err)

	// risk_pct is the first SEPA param: default 1.0, search [1,4].
	var risk *ParamSchema
	for i := range m.Parameters {
		if m.Parameters[i].Name == "risk_pct" {
			risk = &m.Parameters[i]
			break
		}
	}
	require.NotNil(t, risk, "risk_pct param present")
	require.Equal(t, "float", risk.Type)
	require.InDelta(t, 1.0, risk.Default.(float64), 1e-9)
	require.NotNil(t, risk.SearchLow)
	require.NotNil(t, risk.SearchHigh)
	require.InDelta(t, 1.0, *risk.SearchLow, 1e-9)
	require.InDelta(t, 4.0, *risk.SearchHigh, 1e-9)
	require.NotEmpty(t, risk.Description)

	// The active value mirrors the default in baseline mode.
	require.InDelta(t, 1.0, m.ActiveValues["risk_pct"].(float64), 1e-9)
}

func TestHandleStrategyListHTTP(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/strategies", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Strategies []StrategyMeta `json:"strategies"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Strategies, 4)
	require.Equal(t, "sepa", body.Strategies[0].ID)
	require.Equal(t, "orb", body.Strategies[3].BacktestID)
}

func TestHandleStrategyGetHTTP(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/strategies/pairs", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Strategy StrategyMeta `json:"strategy"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "pairs", body.Strategy.ID)
	require.Equal(t, "pairs", body.Strategy.BacktestID)
	require.Greater(t, body.Strategy.ParametersCount, 0)
}

func TestHandleStrategyGetNotFoundHTTP(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/strategies/bogus", nil, true)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, CodeNotFound, errCode(t, rec))
}

func TestHandleStrategyListRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/strategies", nil, false)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
