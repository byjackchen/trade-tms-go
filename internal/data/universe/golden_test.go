package universe

// golden_test.go replays the pinned golden screener inputs (the 48-ticker P0
// import subset, as_of 2026-05-27) and requires bit-identical output: ranking
// order, scores, breakout proximities, trend-template rule flags and the
// market-cap cap.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

type goldenCandidate struct {
	InstrumentID       string  `json:"instrument_id"`
	Score              float64 `json:"score"`
	TrendTemplateCount int     `json:"trend_template_count"`
	BreakoutProximity  float64 `json:"breakout_proximity"`
	MarketCapUSD       float64 `json:"market_cap_usd"`
	AsOf               string  `json:"as_of"`
}

type goldenTT struct {
	Rules            [8]bool `json:"rules"`
	Close            float64 `json:"close"`
	MA50             float64 `json:"ma50"`
	MA150            float64 `json:"ma150"`
	MA200            float64 `json:"ma200"`
	High52w          float64 `json:"high_52w"`
	Low52w           float64 `json:"low_52w"`
	MA200UptrendDays int     `json:"ma200_uptrend_days"`
}

type goldenFile struct {
	AsOf          string              `json:"as_of"`
	WarmupStart   string              `json:"warmup_start"`
	Subset        []string            `json:"subset"`
	Universe      []string            `json:"universe_sf1_sorted"`
	MarketCaps    map[string]float64  `json:"market_caps"`
	Capped85      []string            `json:"capped_limit85"`
	Capped10      []string            `json:"capped_limit10"`
	TopK          []goldenCandidate   `json:"top_k"`
	TrendTemplate map[string]goldenTT `json:"trend_template"`
	Bars          map[string][][]any  `json:"bars"`
}

func loadGolden(t *testing.T) *goldenFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "universe_golden.json"))
	require.NoError(t, err)
	var g goldenFile
	require.NoError(t, json.Unmarshal(raw, &g))
	require.Len(t, g.Subset, 48)
	require.NotEmpty(t, g.Universe)
	require.Len(t, g.TopK, len(g.Universe))
	return &g
}

func goldenRows(t *testing.T, g *goldenFile, ticker string) []OHLCV {
	t.Helper()
	rows := g.Bars[ticker]
	require.NotEmpty(t, rows, "golden bars missing for %s", ticker)
	out := make([]OHLCV, len(rows))
	for i, r := range rows {
		require.Len(t, r, 6)
		d, err := calendar.ParseDate(r[0].(string))
		require.NoError(t, err)
		out[i] = OHLCV{
			TS:     time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC),
			Open:   r[1].(float64),
			High:   r[2].(float64),
			Low:    r[3].(float64),
			Close:  r[4].(float64),
			Volume: r[5].(float64),
		}
	}
	return out
}

// warmGoldenScreener feeds the golden warmup bars (the exact tail the pinned
// golden screener state held) into a fresh Screener.
func warmGoldenScreener(t *testing.T, g *goldenFile) *Screener {
	t.Helper()
	scr, err := NewScreener(ScreenerConfig{MarketCapLookup: staticCaps(g.MarketCaps)})
	require.NoError(t, err)
	for _, ticker := range g.Universe {
		require.NoError(t, scr.Warmup(ticker, goldenRows(t, g, ticker)))
	}
	return scr
}

func TestGoldenTopKMatchesPythonReference(t *testing.T) {
	g := loadGolden(t)
	scr := warmGoldenScreener(t, g)
	asOf, err := calendar.ParseDate(g.AsOf)
	require.NoError(t, err)

	got := scr.TopK(len(g.Universe), asOf)
	require.Len(t, got, len(g.TopK))

	for i, want := range g.TopK {
		c := got[i]
		assert.Equal(t, want.InstrumentID, c.InstrumentID, "rank %d ticker", i+1)
		assert.Equal(t, want.Score, c.Score, "rank %d (%s) score must be bit-identical", i+1, want.InstrumentID)
		assert.Equal(t, want.TrendTemplateCount, c.Metadata["trend_template_count"], "%s tt", want.InstrumentID)
		assert.Equal(t, want.BreakoutProximity, c.Metadata["breakout_proximity"], "%s proximity", want.InstrumentID)
		assert.Equal(t, want.MarketCapUSD, c.Metadata["market_cap_usd"], "%s market cap", want.InstrumentID)
		assert.Equal(t, want.AsOf, c.Metadata["as_of"], "%s as_of", want.InstrumentID)
	}
}

func TestGoldenTrendTemplateDiagnostics(t *testing.T) {
	g := loadGolden(t)
	scr := warmGoldenScreener(t, g)

	for ticker, want := range g.TrendTemplate {
		res, ok := scr.Evaluate(ticker)
		require.True(t, ok, ticker)
		assert.Equal(t, want.Rules, res.Rules, "%s rule flags", ticker)
		assert.Equal(t, want.Close, res.Close, "%s close", ticker)
		assert.Equal(t, want.MA50, res.MA50, "%s ma50 (rolling-mean bit-exact)", ticker)
		assert.Equal(t, want.MA150, res.MA150, "%s ma150", ticker)
		assert.Equal(t, want.MA200, res.MA200, "%s ma200", ticker)
		assert.Equal(t, want.High52w, res.High52w, "%s high_52w", ticker)
		assert.Equal(t, want.Low52w, res.Low52w, "%s low_52w", ticker)
		assert.Equal(t, want.MA200UptrendDays, res.MA200UptrendDays, "%s ma200_uptrend_days", ticker)
	}
}

func TestGoldenUniverseLimit(t *testing.T) {
	g := loadGolden(t)
	lookup := staticCaps(g.MarketCaps)

	assert.Equal(t, g.Capped85, ApplyUniverseLimit(g.Universe, lookup, 85),
		"36 <= 85: pass-through in sorted-ascending order")
	assert.Equal(t, g.Capped10, ApplyUniverseLimit(g.Universe, lookup, 10),
		"top-10 by market cap descending")
}

func TestGoldenExclusionsAndCapSemantics(t *testing.T) {
	g := loadGolden(t)

	// The SEPA universe never contains an excluded ticker, and the
	// exclusion set + pair legs are all inside the 48-ticker subset.
	excl := map[string]bool{}
	for _, e := range ExcludedTickers() {
		excl[e] = true
		assert.Contains(t, g.Subset, e)
	}
	for _, tk := range g.Universe {
		assert.False(t, excl[tk], "%s must be excluded from the SEPA universe", tk)
	}
	for _, leg := range PairLegTickers() {
		assert.Contains(t, g.Universe, leg,
			"pair legs are NOT excluded from the SEPA universe (spec §4.1)")
	}

	// ETF caps are 0.0 (no SF1 fundamentals).
	assert.Equal(t, 0.0, g.MarketCaps["SPY"])
	assert.Equal(t, 0.0, g.MarketCaps["XLK"])

	// Sanity: golden scores are non-NaN and already in ranking order.
	last := math.Inf(1)
	for _, c := range g.TopK {
		require.False(t, math.IsNaN(c.Score))
		assert.LessOrEqual(t, c.Score, last)
		last = c.Score
	}
}
