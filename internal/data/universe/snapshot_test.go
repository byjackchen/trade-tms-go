package universe

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

func TestSnapshotFromResult(t *testing.T) {
	asOf := calendar.NewDate(2026, time.May, 27)
	res := &Result{
		AsOf:        asOf,
		WarmupStart: asOf.AddDays(-WarmupCalendarDays),
		Kind:        KindEOD,
		Limit:       85,
		Raw:         []string{"AAPL", "MSFT", "ZZZ"},
		Excluded:    []string{"SPY"},
		Tickers:     []string{"AAPL", "MSFT"},
		Candidates: []Candidate{
			{
				InstrumentID: "AAPL",
				Score:        84.5,
				Metadata: map[string]any{
					"trend_template_count": 8,
					"breakout_proximity":   0.9,
					"market_cap_usd":       4.1e12,
					"as_of":                asOf.String(),
				},
			},
			{
				InstrumentID: "MSFT",
				Score:        math.NaN(), // adversarial: NaN must not reach JSONB
				Metadata: map[string]any{
					"trend_template_count": 0,
					"breakout_proximity":   math.NaN(),
					"market_cap_usd":       3.1e12,
					"as_of":                asOf.String(),
				},
			},
		},
		Rules: map[string]TrendTemplateResult{
			"AAPL": {Rules: [8]bool{true, true, true, true, true, true, true, true}},
		},
		Warmed: 2,
	}

	snap := SnapshotFromResult(res)
	assert.Equal(t, asOf, snap.AsOf)
	assert.Equal(t, KindEOD, snap.Kind)
	assert.Equal(t, TableSF1, snap.TableFilter)
	assert.Equal(t, res.WarmupStart, snap.WindowStart)
	assert.Equal(t, asOf, snap.WindowEnd)
	assert.Equal(t, 85, snap.LimitN)
	assert.Equal(t, []string{"AAPL", "MSFT"}, snap.Tickers)
	assert.Equal(t, []string{"SPY"}, snap.Excluded)

	require.Len(t, snap.Members, 2)
	assert.Equal(t, Member{
		Ticker: "AAPL", Rank: 1, Score: 84.5, TrendTemplateCount: 8,
		BreakoutProximity: 0.9, MarketCapUSD: 4.1e12,
		Reasons: []string{
			"rule_1_close_gt_ma50", "rule_2_close_gt_ma150", "rule_3_close_gt_ma200",
			"rule_4_ma50_gt_ma150", "rule_5_ma150_gt_ma200",
			"rule_6_within_25pct_of_52w_high", "rule_7_above_30pct_above_52w_low",
			"rule_8_market_cap_above_min",
		},
	}, snap.Members[0])

	msft := snap.Members[1]
	assert.Equal(t, 2, msft.Rank)
	assert.Equal(t, 0.0, msft.Score, "NaN sanitized for JSON")
	assert.Equal(t, 0.0, msft.BreakoutProximity)
	assert.Equal(t, []string{}, msft.Reasons, "no rules diagnostics -> empty, not null")

	// The whole members array must marshal cleanly (JSON has no NaN).
	raw, err := json.Marshal(snap.Members)
	require.NoError(t, err)
	var back []Member
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, snap.Members, back)

	assert.Equal(t, 3, snap.Params["raw_count"])
	assert.Equal(t, 2, snap.Params["warmed"])
}

func TestSnapshotFromResultDefaults(t *testing.T) {
	snap := SnapshotFromResult(&Result{AsOf: calendar.NewDate(2026, time.January, 5)})
	assert.Equal(t, KindManual, snap.Kind, "empty kind defaults to manual")
	assert.Equal(t, []string{}, snap.Excluded)
	assert.Empty(t, snap.Members)
	assert.Equal(t, 0, snap.LimitN, "uncapped -> 0 -> NULL at insert")
}
