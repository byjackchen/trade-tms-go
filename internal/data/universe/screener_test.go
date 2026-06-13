package universe

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func staticCaps(caps map[string]float64) MarketCapLookup {
	return func(t string) float64 { return caps[t] }
}

func newTestScreener(t *testing.T, caps map[string]float64) *Screener {
	t.Helper()
	s, err := NewScreener(ScreenerConfig{MarketCapLookup: staticCaps(caps)})
	require.NoError(t, err)
	return s
}

func dayTS(i int) time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
}

func mkRows(n int, f func(i int) (o, h, l, c, v float64)) []OHLCV {
	out := make([]OHLCV, n)
	for i := range out {
		o, h, l, c, v := f(i)
		out[i] = OHLCV{TS: dayTS(i), Open: o, High: h, Low: l, Close: c, Volume: v}
	}
	return out
}

func TestNewScreenerRequiresLookup(t *testing.T) {
	_, err := NewScreener(ScreenerConfig{})
	require.Error(t, err)
}

func TestBreakoutProximity(t *testing.T) {
	s := newTestScreener(t, nil)

	assert.Equal(t, 0.0, s.BreakoutProximity("NOPE"), "unknown ticker")

	// One bar low=10 high=20 close=15 -> (15-10)/(20-10) = 0.5.
	require.NoError(t, s.Warmup("MID", mkRows(1, func(int) (float64, float64, float64, float64, float64) {
		return 15, 20, 10, 15, 100
	})))
	assert.Equal(t, 0.5, s.BreakoutProximity("MID"))

	// Degenerate flat range -> 0.0.
	require.NoError(t, s.Warmup("FLAT", mkRows(5, func(int) (float64, float64, float64, float64, float64) {
		return 10, 10, 10, 10, 100
	})))
	assert.Equal(t, 0.0, s.BreakoutProximity("FLAT"))

	// Close at the window high (same bar updates the window) -> exactly 1.0.
	require.NoError(t, s.Warmup("GAP", []OHLCV{
		{TS: dayTS(0), Open: 10, High: 12, Low: 9, Close: 11, Volume: 1},
		{TS: dayTS(1), Open: 14, High: 16, Low: 13, Close: 16, Volume: 1},
	}))
	assert.Equal(t, 1.0, s.BreakoutProximity("GAP"))

	// Malformed close above the bar high clamps to 1.0; below low to 0.0.
	require.NoError(t, s.Warmup("HI", []OHLCV{{TS: dayTS(0), Open: 1, High: 10, Low: 5, Close: 99, Volume: 1}}))
	assert.Equal(t, 1.0, s.BreakoutProximity("HI"))
	require.NoError(t, s.Warmup("LO", []OHLCV{{TS: dayTS(0), Open: 1, High: 10, Low: 5, Close: 1, Volume: 1}}))
	assert.Equal(t, 0.0, s.BreakoutProximity("LO"))
}

func TestRolling60WindowDropsOldBars(t *testing.T) {
	s := newTestScreener(t, nil)
	// 61 bars: first has the global high (1000); the trailing 60-bar window
	// must exclude it after the 61st append.
	rows := mkRows(61, func(i int) (float64, float64, float64, float64, float64) {
		if i == 0 {
			return 1000, 1000, 999, 1000, 1
		}
		return 10, 20, 10, 15, 1
	})
	require.NoError(t, s.Warmup("T", rows))
	assert.Equal(t, 0.5, s.BreakoutProximity("T"), "high 1000 left the 60-bar window")
}

func TestWarmupKeepsLatestTail(t *testing.T) {
	s := newTestScreener(t, nil)
	require.NoError(t, s.Warmup("T", mkRows(300, func(i int) (float64, float64, float64, float64, float64) {
		f := float64(i)
		return f, f + 1, f - 1, f, 10
	})))
	assert.Equal(t, DefaultHistoryMaxBars, s.BarsSeen("T"))
	assert.Equal(t, 1, s.TrackedCount())

	// Empty warmup is a no-op and does not track.
	require.NoError(t, s.Warmup("EMPTY", nil))
	assert.Equal(t, 0, s.BarsSeen("EMPTY"))
	assert.Equal(t, 1, s.TrackedCount())
}

func TestWarmupNonFiniteVolume(t *testing.T) {
	s := newTestScreener(t, nil)
	rows := mkRows(3, func(i int) (float64, float64, float64, float64, float64) {
		v := 10.0
		if i == 1 {
			v = math.NaN()
		}
		return 10, 20, 10, 15, v
	})
	err := s.Warmup("BAD", rows)
	require.Error(t, err)
	// Like pandas astype(int) raising after the state dict insert: tracked,
	// zero bars appended.
	assert.Equal(t, 1, s.TrackedCount())
	assert.Equal(t, 0, s.BarsSeen("BAD"))
	assert.Equal(t, 0, s.TrendTemplateCount("BAD"))
	assert.Equal(t, 0.0, s.BreakoutProximity("BAD"))
}

func TestUpdateFromDomainBar(t *testing.T) {
	s := newTestScreener(t, nil)
	p := func(f float64) domain.Price {
		pr, err := domain.PriceFromFloat64(f)
		require.NoError(t, err)
		return pr
	}
	s.Update(domain.Bar{Symbol: "T", TS: dayTS(0).UTC(), Open: p(15), High: p(20), Low: p(10), Close: p(15), Volume: 100})
	assert.Equal(t, 1, s.BarsSeen("T"))
	assert.Equal(t, 0.5, s.BreakoutProximity("T"))
}

func TestTrendTemplateCountUntracked(t *testing.T) {
	s := newTestScreener(t, nil)
	assert.Equal(t, 0, s.TrendTemplateCount("NOPE"))
	_, ok := s.Evaluate("NOPE")
	assert.False(t, ok)
}

func TestTopKOrderingAndMetadata(t *testing.T) {
	caps := map[string]float64{
		"BIGB": 2e9, // rule 8 passes -> tt = 1 with short history
		"BIGA": 2e9, // identical score and cap as BIGB -> ticker ASC tiebreak
		"HUGE": 9e9, // same score, larger cap -> ahead of BIGA/BIGB
		"TINY": 1e6, // rule 8 fails -> tt = 0
	}
	s := newTestScreener(t, caps)
	flat := func(int) (float64, float64, float64, float64, float64) { return 10, 10, 10, 10, 1 }
	for _, tk := range []string{"BIGB", "BIGA", "HUGE", "TINY"} {
		require.NoError(t, s.Warmup(tk, mkRows(5, flat)))
	}

	asOf := calendar.NewDate(2026, time.May, 27)
	assert.Nil(t, s.TopK(0, asOf), "k <= 0 -> empty")
	assert.Nil(t, s.TopK(-1, asOf))

	got := s.TopK(10, asOf)
	require.Len(t, got, 4)
	order := []string{got[0].InstrumentID, got[1].InstrumentID, got[2].InstrumentID, got[3].InstrumentID}
	assert.Equal(t, []string{"HUGE", "BIGA", "BIGB", "TINY"}, order,
		"score DESC, then market cap DESC, then ticker ASC")

	c := got[0]
	assert.Equal(t, 10.0, c.Score, "tt=1 (rule 8), proximity 0 on flat range")
	require.Len(t, c.Metadata, 4)
	assert.Equal(t, 1, c.Metadata["trend_template_count"])
	assert.Equal(t, 0.0, c.Metadata["breakout_proximity"])
	assert.Equal(t, 9e9, c.Metadata["market_cap_usd"])
	assert.Equal(t, "2026-05-27", c.Metadata["as_of"])

	// Truncation takes the first k of the same total order.
	top2 := s.TopK(2, asOf)
	require.Len(t, top2, 2)
	assert.Equal(t, "HUGE", top2[0].InstrumentID)
	assert.Equal(t, "BIGA", top2[1].InstrumentID)
}

func TestTopKEmptyScreener(t *testing.T) {
	s := newTestScreener(t, nil)
	assert.Empty(t, s.TopK(5, calendar.NewDate(2026, time.January, 2)))
}
