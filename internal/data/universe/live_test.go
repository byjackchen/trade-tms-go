package universe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveUniverseLimit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", DefaultLiveUniverseLimit},
		{"whitespace", "   \t", DefaultLiveUniverseLimit},
		{"integer", "85", 85},
		{"padded", " 42 ", 42},
		{"plus-sign", "+7", 7},
		{"zero", "0", 0},
		{"negative", "-3", -3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveUniverseLimit(fakeEnv(map[string]string{EnvLiveUniverseLimit: tc.raw}))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := ResolveUniverseLimit(fakeEnv(map[string]string{EnvLiveUniverseLimit: "12x"}))
	require.Error(t, err)
	assert.EqualError(t, err, `TMS_LIVE_UNIVERSE_LIMIT must be an integer, got "12x"`,
		"fail fast with the reference's message (live_runner.py:55-60)")
}

func TestApplyUniverseLimitEdgeCases(t *testing.T) {
	lookup := staticCaps(nil)
	assert.Empty(t, ApplyUniverseLimit(nil, lookup, 5), "empty universe")
	assert.Empty(t, ApplyUniverseLimit([]string{"A", "B"}, lookup, 0), "limit 0")
	assert.Empty(t, ApplyUniverseLimit([]string{"A", "B"}, lookup, -1), "negative limit")
}

func TestApplyUniverseLimitPassThroughPreservesOrder(t *testing.T) {
	in := []string{"ZULU", "ALPHA", "MIKE"}
	got := ApplyUniverseLimit(in, staticCaps(map[string]float64{"ALPHA": 9e12}), 3)
	assert.Equal(t, in, got, "len <= limit: input unchanged, no reshuffling")
	got[0] = "MUTATED"
	assert.Equal(t, "ZULU", in[0], "returned slice is a copy")
}

func TestApplyUniverseLimitTopNByCapDescending(t *testing.T) {
	// Mirrors tests/runner/test_live_universe_limit.py:52-76.
	caps := map[string]float64{
		"AAPL": 2.8e12,
		"MSFT": 3.0e12,
		"NVDA": 2.0e12,
		"KO":   2.6e11,
		"GME":  1.0e10,
	}
	in := []string{"AAPL", "GME", "KO", "MSFT", "NVDA", "ZZZZ"} // sorted ascending, ZZZZ unknown
	got := ApplyUniverseLimit(in, staticCaps(caps), 3)
	assert.Equal(t, []string{"MSFT", "AAPL", "NVDA"}, got,
		"cap-descending order, not re-sorted alphabetically")

	// Unknown (0.0) tickers sort last and are cut when enough caps exist.
	got = ApplyUniverseLimit(in, staticCaps(caps), 5)
	assert.NotContains(t, got, "ZZZZ")
}

func TestApplyUniverseLimitStableOnTies(t *testing.T) {
	// All caps 0.0 (unknown): the stable sort must keep input order.
	in := []string{"DELTA", "ALPHA", "CHARLIE", "BRAVO"}
	got := ApplyUniverseLimit(in, staticCaps(nil), 2)
	assert.Equal(t, []string{"DELTA", "ALPHA"}, got)
}

func TestApplyUniverseLimitCallsLookupOncePerTicker(t *testing.T) {
	calls := map[string]int{}
	lookup := func(tk string) float64 {
		calls[tk]++
		return 0
	}
	ApplyUniverseLimit([]string{"A", "B", "C", "D"}, lookup, 2)
	for tk, n := range calls {
		assert.Equal(t, 1, n, "lookup for %s must run exactly once (Python sorted key contract)", tk)
	}
}

func TestExclusionAndSubscriptionSets(t *testing.T) {
	assert.Equal(t, []string{
		"SPY", "XLK", "XLF", "XLE", "XLV", "XLY", "XLP", "XLU", "XLB", "XLI", "XLRE", "XLC",
	}, ExcludedTickers(), "SPY + the 11 sector ETFs, source order — pair legs NOT excluded")

	assert.Equal(t, []string{"CVX", "KO", "MA", "PEP", "V", "XOM"}, PairLegTickers(),
		"deduped + sorted pair legs")

	pairs := DefaultPairs()
	assert.Equal(t, [][2]string{{"KO", "PEP"}, {"MA", "V"}, {"XOM", "CVX"}}, pairs)
	for _, leg := range PairLegTickers() {
		assert.NotContains(t, ExcludedTickers(), leg)
	}

	etfs := SectorETFTickers()
	assert.Len(t, etfs, 11)
	etfs[0] = "MUTATED"
	assert.Equal(t, "XLK", SectorETFTickers()[0], "accessors return copies")
}
