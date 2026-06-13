// Package universe implements universe construction and the SEPA screener
// (docs/spec/calendar-universe.md §2–§4) on top of TimescaleDB instead of
// the Python reference's parquet cache.
//
// # What is [MUST-MATCH] here
//
//   - Ticker-window filtering (survivor-bias-free, spec §2.2): a ticker is
//     tradable in [start, end] iff firstpricedate is NULL or <= end, AND
//     lastpricedate is NULL or >= start; result sorted ascending.
//   - Market-cap lookup (spec §2.3): latest SF1 row by datekey across ALL
//     dimensions; NULL/absent -> 0.0 ("unknown, fails rule 8, sorts last").
//   - SEPA screener (spec §3): 260-bar rolling tail, 60-bar breakout
//     proximity with the exact clamp/degenerate-range semantics, the 8
//     Minervini trend-template rules with pandas-equivalent float64
//     arithmetic (see rollmean emulation in trendtemplate.go), and the
//     top_k sort key (score DESC, market cap DESC, ticker ASC).
//   - Live assembly (spec §4): 730-calendar-day warmup window, SF1 table
//     filter, exclusion of SPY + the 11 Select Sector SPDR ETFs (pair legs
//     are deliberately NOT excluded), TMS_LIVE_UNIVERSE_LIMIT resolution
//     (default 85, fail-fast on non-integer), and the stable
//     top-N-by-market-cap cap with pass-through when len <= limit.
//
// # Documented deviations (P1 locked decisions)
//
//   - "Today" is the America/New_York calendar date of the injected clock
//     (calendar.DateOf(now, NY)), not the machine-local date the Python
//     reference uses (live_runner.py:257) and not the UTC date catch-up
//     uses. This resolves spec Open Question 8 by normalizing all
//     trading-date logic to the exchange time zone [IMPROVE].
//   - Market-cap datekey ties are broken by dimension DESC, which is
//     byte-equivalent to the reference's stable sort over the parquet
//     row order (files are sorted by (ticker, datekey, dimension); a
//     stable sort_values("datekey") keeps dimension ascending within a
//     tie and iloc[-1] takes the greatest dimension). This pins spec Open
//     Question 2 deterministically.
//   - Every computed universe is persisted to tms.universe_snapshots with
//     a ranked members JSONB array (rank + score diagnostics + passing-rule
//     reasons); the reference keeps no such record [IMPROVE].
//   - Infrastructure errors (DB down, query failure) are returned to the
//     caller instead of being swallowed; the reference's warn-and-continue
//     policy (spec §4.1) is applied only to per-ticker warmup failures,
//     which are logged and reported in the build result.
//
// Golden parity: internal/data/universe/testdata/universe_golden.json is
// produced by tmp/gen_universe_ref.py running the real Python screener
// (trade-multi-strategies venv) over the 48-ticker P0 import subset;
// golden_test.go replays the same inputs through this package and requires
// identical ranking, rule flags and (exact) float scores.
package universe
