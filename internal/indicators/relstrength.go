package indicators

// relstrength.go provides a cross-sectional Relative-Strength rank used to make
// every SEPA forming signal rankable on the trader's watchlist. The baseline
// SEPA state machine does not compute an RS rank; this layer adds one. We
// compute the Minervini-style weighted return blend per symbol, then
// percentile-rank it across the universe.
//
// The blend follows Minervini's RS line construction (quarter-weighted trailing
// returns favouring the most recent quarter):
//
//	rs_raw = 0.4*r63 + 0.2*r126 + 0.2*r189 + 0.2*r252
//
// where rN = close[t]/close[t-N] - 1 over N TRADING-day lookbacks (63 ≈ 1q,
// 126 ≈ 2q, 189 ≈ 3q, 252 ≈ 1y). A symbol lacking the full 252-bar history is
// skipped (no partial blend — partial-history names would rank unfairly). The
// surviving raw scores are percentile-ranked into [1,99].

import "sort"

// RS lookback windows (trading days) and their blend weights.
const (
	RSLookback63  = 63
	RSLookback126 = 126
	RSLookback189 = 189
	RSLookback252 = 252

	rsWeight63  = 0.4
	rsWeight126 = 0.2
	rsWeight189 = 0.2
	rsWeight252 = 0.2

	// RSRankMin / RSRankMax bound the percentile rank (Minervini's 1..99 RS
	// scale). 1 = weakest in the universe, 99 = strongest.
	RSRankMin = 1
	RSRankMax = 99
)

// RSRawScore computes the Minervini-weighted return blend for one symbol's
// adjusted-close series (oldest first). ok=false when the series lacks the full
// 252-bar history or any required base price is non-positive (split/NaN gap),
// signalling the caller to skip the symbol from the ranking universe.
func RSRawScore(closeAdj []float64) (score float64, ok bool) {
	n := len(closeAdj)
	if n <= RSLookback252 {
		return 0, false
	}
	cur := closeAdj[n-1]
	if cur <= 0 {
		return 0, false
	}
	r := func(lookback int) (float64, bool) {
		base := closeAdj[n-1-lookback]
		if base <= 0 {
			return 0, false
		}
		return cur/base - 1.0, true
	}
	r63, ok1 := r(RSLookback63)
	r126, ok2 := r(RSLookback126)
	r189, ok3 := r(RSLookback189)
	r252, ok4 := r(RSLookback252)
	if !(ok1 && ok2 && ok3 && ok4) {
		return 0, false
	}
	return rsWeight63*r63 + rsWeight126*r126 + rsWeight189*r189 + rsWeight252*r252, true
}

// RSRankUniverse percentile-ranks a universe of raw RS scores into [1,99].
//
// Input: symbol -> raw blended score (only symbols with full history; produce it
// via RSRawScore). Output: symbol -> integer rank in [1,99], where the highest
// raw score maps toward 99 and the lowest toward 1. Ties share the same rank
// (rank = percentile of the count of strictly-weaker symbols). A single-symbol
// universe ranks at RSRankMax (it is, trivially, the strongest). Symbols absent
// from the input map are absent from the output (the caller leaves their rank
// nil).
//
// Percentile formula (TMS enhancement): for a symbol with `below` strictly-weaker
// peers out of N total,
//
//	pct  = below / (N-1)          # fraction in [0,1]; the strongest -> 1.0
//	rank = round(1 + pct*(99-1))  # mapped into [1,99]
//
// This is a standard percentile rank (the strongest name in the universe always
// lands on 99, the weakest on 1), which is exactly how the watchlist wants to
// rank "leadership" cross-sectionally.
func RSRankUniverse(raw map[string]float64) map[string]int {
	out := make(map[string]int, len(raw))
	n := len(raw)
	if n == 0 {
		return out
	}
	if n == 1 {
		for sym := range raw {
			out[sym] = RSRankMax
		}
		return out
	}

	// Sort the scores ascending so we can count strictly-weaker peers per value.
	scores := make([]float64, 0, n)
	for _, v := range raw {
		scores = append(scores, v)
	}
	sort.Float64s(scores)

	// belowCount(v) = number of scores strictly less than v (lower-bound index in
	// the ascending sorted slice). Ties therefore share the same `below` and rank.
	belowCount := func(v float64) int {
		return sort.Search(len(scores), func(i int) bool { return scores[i] >= v })
	}

	denom := float64(n - 1)
	span := float64(RSRankMax - RSRankMin)
	for sym, v := range raw {
		pct := float64(belowCount(v)) / denom
		rank := int(float64(RSRankMin) + pct*span + 0.5) // round half up
		if rank < RSRankMin {
			rank = RSRankMin
		}
		if rank > RSRankMax {
			rank = RSRankMax
		}
		out[sym] = rank
	}
	return out
}
