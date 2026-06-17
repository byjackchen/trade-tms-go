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
		assert.Equal(t, 1, n, "lookup for %s must run exactly once (sort-key contract)", tk)
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

func TestResolveLiveSubscriptionSet(t *testing.T) {
	// Fixed baskets are always subscribed; SEPA is market-cap-capped to the budget
	// left under the OpenD cap minus the safety margin.
	fixed := []string{"SPY", "XLK", "XLF", "KO", "PEP"}
	caps := map[string]float64{
		"AAA": 9, "BBB": 8, "CCC": 7, "DDD": 6, "EEE": 5, "FFF": 4,
	}
	lookup := staticCaps(caps)

	t.Run("small set fits without capping", func(t *testing.T) {
		sepa := []string{"AAA", "BBB", "CCC"}
		set := ResolveLiveSubscriptionSet(fixed, sepa, lookup, 100)
		assert.Equal(t, 100, set.Cap)
		assert.Equal(t, 95, set.Budget)
		assert.ElementsMatch(t, fixed, set.Fixed)
		assert.ElementsMatch(t, sepa, set.SEPA, "all SEPA names fit under the budget")
		assert.Len(t, set.All, len(fixed)+len(sepa))
		assert.LessOrEqual(t, len(set.All), set.Cap)
	})

	t.Run("SEPA capped to top-by-market-cap to fit the budget", func(t *testing.T) {
		sepa := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF"}
		// Cap 12 -> budget 7, minus 5 fixed -> 2 SEPA slots; top-2 caps are AAA(9),BBB(8).
		set := ResolveLiveSubscriptionSet(fixed, sepa, lookup, 12)
		assert.Equal(t, 7, set.Budget)
		assert.Equal(t, 2, set.SEPALimit)
		assert.ElementsMatch(t, []string{"AAA", "BBB"}, set.SEPA, "the two highest-cap SEPA names are admitted")
		assert.Len(t, set.All, 7)
		assert.LessOrEqual(t, len(set.All), set.Cap, "the distinct set fits the OpenD cap")
	})

	t.Run("safety margin keeps the set strictly under the hard cap", func(t *testing.T) {
		// A large SEPA universe with a 100 cap: budget 95, 5 fixed -> 90 SEPA slots,
		// total 95 = cap - margin, never reaching the hard 100.
		sepa := make([]string, 4682)
		big := map[string]float64{}
		for i := range sepa {
			tk := "S" + itoa(i)
			sepa[i] = tk
			big[tk] = float64(4682 - i)
		}
		set := ResolveLiveSubscriptionSet(fixed, sepa, staticCaps(big), 100)
		assert.Equal(t, 90, set.SEPALimit)
		assert.Len(t, set.All, 95, "fixed (5) + capped SEPA (90) = budget 95")
		assert.Less(t, len(set.All), 100, "stays under the hard OpenD cap by the safety margin")
	})

	t.Run("SEPA name that is also a fixed basket is counted once", func(t *testing.T) {
		// KO is a pair leg (fixed) AND appears in the SEPA screen; it must not consume
		// a SEPA slot or appear twice.
		sepa := []string{"KO", "AAA", "BBB"}
		set := ResolveLiveSubscriptionSet(fixed, sepa, lookup, 100)
		assert.NotContains(t, set.SEPA, "KO", "a fixed-basket name is folded into Fixed")
		assert.ElementsMatch(t, []string{"AAA", "BBB"}, set.SEPA)
		// KO appears exactly once in the final distinct set.
		count := 0
		for _, s := range set.All {
			if s == "KO" {
				count++
			}
		}
		assert.Equal(t, 1, count, "KO appears exactly once in the subscription set")
	})

	t.Run("fixed baskets alone over budget admit no SEPA", func(t *testing.T) {
		sepa := []string{"AAA", "BBB"}
		// Cap 4 -> budget 0 (4-5 margin clamped), so no SEPA budget at all.
		set := ResolveLiveSubscriptionSet(fixed, sepa, lookup, 4)
		assert.Equal(t, 0, set.Budget)
		assert.Equal(t, 0, set.SEPALimit)
		assert.Empty(t, set.SEPA)
		assert.ElementsMatch(t, fixed, set.All, "only the (un-cappable) fixed baskets remain")
	})
}

// itoa is a tiny strconv.Itoa to avoid an import just for the test loop above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
