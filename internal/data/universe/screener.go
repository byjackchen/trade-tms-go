package universe

// screener.go is the SEPA universe screener: O(1)-per-bar rolling state,
// breakout proximity, trend-template scoring and the deterministic top_k
// ranking (docs/spec/calendar-universe.md §3).

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Screener defaults.
const (
	// DefaultHistoryMaxBars caps the per-ticker rolling bar tail
	// (~1 trading year; MA200 + the 252-bar 52w window need >= 252).
	DefaultHistoryMaxBars = 260
	// BreakoutBaseLookback is the breakout-proximity high/low window.
	BreakoutBaseLookback = 60
	// Score weights: score = tt*10.0 + proximity*5.0 (sepa_screener.py:224).
	scoreWeightTrendTemplate = 10.0
	scoreWeightProximity     = 5.0
)

// MarketCapLookup returns the USD market cap for a ticker; 0.0 means
// "unknown" and fails trend-template rule 8 (spec §2.3).
type MarketCapLookup func(ticker string) float64

// ScreenerConfig mirrors SEPAScreenerConfig (sepa_screener.py:37-50).
type ScreenerConfig struct {
	// MarketCapLookup is required.
	MarketCapLookup MarketCapLookup
	// HistoryMaxBars bounds the rolling tail; <= 0 selects the default 260.
	HistoryMaxBars int
	// MarketCapMinUSD is forwarded to trend-template rule 8; <= 0 selects
	// the default 500,000,000.
	MarketCapMinUSD float64
}

// OHLCV is one daily bar in BarHistoryProvider format (spec §2.1): float64
// fields so source NaN survives to the consumer, exactly like the pandas
// frames of the reference.
type OHLCV struct {
	// TS is the trading date at UTC midnight.
	TS     time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Candidate is one screened candidate: instrument id, composite score and an
// intentionally untyped metadata bag
// with exactly the reference keys.
type Candidate struct {
	InstrumentID string
	Score        float64
	// Metadata keys: "trend_template_count" (int), "breakout_proximity"
	// (float64), "market_cap_usd" (float64), "as_of" (ISO-8601 string).
	Metadata map[string]any
}

// tickerBar is the deque element (ts, o, h, l, c, v) of _TickerState.
type tickerBar struct {
	ts                     time.Time
	open, high, low, close float64
	volume                 int64
}

// tickerState is _TickerState: a bounded ring of bars plus the cached
// 60-bar high/low and last close.
type tickerState struct {
	ring  []tickerBar // capacity == HistoryMaxBars, filled circularly
	head  int         // index of the oldest bar
	count int

	last60High float64
	last60Low  float64
	lastClose  float64
	lastTS     time.Time
}

// at returns the i-th bar (0 = oldest).
func (st *tickerState) at(i int) tickerBar {
	return st.ring[(st.head+i)%len(st.ring)]
}

// append pushes a bar, evicting the oldest when full, then recomputes the
// trailing min(count, 60)-bar high/low with strict max/min comparison
// semantics (NaN never wins a comparison; a leading NaN sticks).
func (st *tickerState) append(b tickerBar) {
	if st.count == len(st.ring) {
		st.ring[st.head] = b
		st.head = (st.head + 1) % len(st.ring)
	} else {
		st.ring[(st.head+st.count)%len(st.ring)] = b
		st.count++
	}

	n := st.count
	if n > BreakoutBaseLookback {
		n = BreakoutBaseLookback
	}
	first := st.at(st.count - n)
	hi, lo := first.high, first.low
	for i := st.count - n + 1; i < st.count; i++ {
		bar := st.at(i)
		if bar.high > hi {
			hi = bar.high
		}
		if bar.low < lo {
			lo = bar.low
		}
	}
	st.last60High = hi
	st.last60Low = lo
	st.lastClose = b.close
	st.lastTS = b.ts
}

// Screener is the concrete SEPA screener. It is NOT safe for concurrent
// use (the reference runs single-threaded inside the trading node); guard
// externally if shared.
type Screener struct {
	cfg    ScreenerConfig
	states map[string]*tickerState
}

// NewScreener validates the config and returns an empty screener.
func NewScreener(cfg ScreenerConfig) (*Screener, error) {
	if cfg.MarketCapLookup == nil {
		return nil, fmt.Errorf("universe: ScreenerConfig.MarketCapLookup is required")
	}
	if cfg.HistoryMaxBars <= 0 {
		cfg.HistoryMaxBars = DefaultHistoryMaxBars
	}
	if cfg.MarketCapMinUSD <= 0 {
		cfg.MarketCapMinUSD = DefaultMarketCapMinUSD
	}
	return &Screener{cfg: cfg, states: make(map[string]*tickerState)}, nil
}

func (s *Screener) state(ticker string) *tickerState {
	st, ok := s.states[ticker]
	if !ok {
		st = &tickerState{ring: make([]tickerBar, s.cfg.HistoryMaxBars)}
		s.states[ticker] = st
	}
	return st
}

// Update folds one live bar into the rolling state — O(1) per call. Prices go
// through the exact fixed-point -> float64 bridge (Price.Float64).
func (s *Screener) Update(bar domain.Bar) {
	s.state(bar.Symbol).append(tickerBar{
		ts:     bar.TS,
		open:   bar.Open.Float64(),
		high:   bar.High.Float64(),
		low:    bar.Low.Float64(),
		close:  bar.Close.Float64(),
		volume: bar.Volume,
	})
}

// Warmup pre-fills the rolling state from history, keeping only the latest
// HistoryMaxBars rows.
//
// A nil/empty slice is a no-op and the ticker stays untracked. A non-finite
// volume fails the whole warmup before any row is appended — the pandas
// astype(int) contract — but, like the reference, the ticker IS tracked
// (with zero bars) from that point on.
func (s *Screener) Warmup(ticker string, rows []OHLCV) error {
	if len(rows) == 0 {
		return nil
	}
	kept := rows
	if len(kept) > s.cfg.HistoryMaxBars {
		kept = kept[len(kept)-s.cfg.HistoryMaxBars:]
	}
	st := s.state(ticker) // tracked even if the volume cast below fails
	for _, r := range kept {
		if math.IsNaN(r.Volume) || math.IsInf(r.Volume, 0) {
			return fmt.Errorf("universe: warmup %s: cannot convert non-finite volume to integer (bar %s)",
				ticker, r.TS.Format(time.DateOnly))
		}
	}
	for _, r := range kept {
		st.append(tickerBar{
			ts:     r.TS,
			open:   r.Open,
			high:   r.High,
			low:    r.Low,
			close:  r.Close,
			volume: int64(r.Volume), // numpy astype(int64): truncation
		})
	}
	return nil
}

// TrackedCount returns the number of tracked tickers.
func (s *Screener) TrackedCount() int { return len(s.states) }

// BarsSeen returns the rolling-tail length for a ticker (0 if untracked).
func (s *Screener) BarsSeen(ticker string) int {
	st, ok := s.states[ticker]
	if !ok {
		return 0
	}
	return st.count
}

// BreakoutProximity is (close - low60) / (high60 - low60) clamped to
// [0, 1]; 0.0 for unknown tickers and degenerate flat ranges.
func (s *Screener) BreakoutProximity(ticker string) float64 {
	st, ok := s.states[ticker]
	if !ok || st.last60High <= st.last60Low {
		return 0.0
	}
	raw := (st.lastClose - st.last60Low) / (st.last60High - st.last60Low)
	if raw < 0.0 {
		return 0.0
	}
	if raw > 1.0 {
		return 1.0
	}
	return raw
}

// Evaluate materializes the rolling tail and runs the trend template,
// returning ok=false for untracked/empty tickers. Only call from ranking
// paths (cost contract, spec §3.5): never per Update.
func (s *Screener) Evaluate(ticker string) (TrendTemplateResult, bool) {
	st, ok := s.states[ticker]
	if !ok || st.count == 0 {
		return TrendTemplateResult{}, false
	}
	highs := make([]float64, st.count)
	lows := make([]float64, st.count)
	closes := make([]float64, st.count)
	for i := 0; i < st.count; i++ {
		b := st.at(i)
		highs[i], lows[i], closes[i] = b.high, b.low, b.close
	}
	return EvaluateTrendTemplate(highs, lows, closes,
		s.cfg.MarketCapLookup(ticker), s.cfg.MarketCapMinUSD), true
}

// TrendTemplateCount returns the number of passing trend-template rules
// (0-8); 0 for untracked tickers (sepa_screener.py:166-184).
func (s *Screener) TrendTemplateCount(ticker string) int {
	res, ok := s.Evaluate(ticker)
	if !ok {
		return 0
	}
	return res.PassingRules()
}

// TopK ranks every tracked ticker by score = tt*10 + proximity*5 with the
// deterministic sort key (score DESC, market cap DESC, ticker ASC) and
// returns the first k. asOf is informational metadata only.
//
// [IMPROVE, output-identical] the market-cap lookup runs once per ticker
// per ranking pass instead of twice.
func (s *Screener) TopK(k int, asOf calendar.Date) []Candidate {
	if k <= 0 || len(s.states) == 0 {
		return nil
	}
	type scored struct {
		ticker string
		score  float64
		cap    float64
		tt     int
		prox   float64
	}
	rows := make([]scored, 0, len(s.states))
	for ticker := range s.states {
		tt := s.TrendTemplateCount(ticker)
		prox := s.BreakoutProximity(ticker)
		// The float64 conversions force IEEE rounding of each product
		// before the add: without them Go may contract a*b+c into one FMA
		// (legal per spec, observed on arm64 but not x86), which would drift
		// the score by an ulp across platforms. Explicit rounding keeps the
		// score bit-identical on arm64 and x86 for reproducible rankings.
		score := float64(float64(tt)*scoreWeightTrendTemplate) + float64(prox*scoreWeightProximity)
		rows = append(rows, scored{
			ticker: ticker,
			score:  score,
			cap:    s.cfg.MarketCapLookup(ticker),
			tt:     tt,
			prox:   prox,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		if rows[i].cap != rows[j].cap {
			return rows[i].cap > rows[j].cap
		}
		return rows[i].ticker < rows[j].ticker
	})
	if k > len(rows) {
		k = len(rows)
	}
	out := make([]Candidate, 0, k)
	for _, r := range rows[:k] {
		out = append(out, Candidate{
			InstrumentID: r.ticker,
			Score:        r.score,
			Metadata: map[string]any{
				"trend_template_count": r.tt,
				"breakout_proximity":   r.prox,
				"market_cap_usd":       r.cap,
				"as_of":                asOf.String(),
			},
		})
	}
	return out
}
