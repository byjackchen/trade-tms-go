package riskgate

// context_refresher.go (spec §7.2-§7.4): the three pure context-computation
// functions consumed by the backtest context providers and (in live mode) the
// context publishers.
//
//   - ComputeRegime:        SPY daily history -> {bull,bear,neutral,warning}.
//   - LoadSF1MarketCaps:    latest market cap per ticker as of a date.
//   - LoadEarningsCalendar: per-ticker earnings-blackout flag as of a date.
//
// The loaders take typed slices and preserve input order so the "stable sort by
// datekey, last per ticker" tie-break (§7.3) is deterministic. Float math is
// float64; the rolling MA uses min_periods=200 (first 199 entries NaN) and NaN
// propagates through the mean.

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// SPYBar is one SPY daily history row for regime classification (a `close`
// column plus a date; spec §7.2). Date is the bar's calendar date (UTC).
type SPYBar struct {
	Date  time.Time
	Close float64 // may be NaN; NaN poisons the rolling MA exactly as pandas
}

// SF1Row is one SHARADAR/SF1 fundamentals row (spec §7.3). HasMarketCap is false
// when marketcap is null (pandas NaN) so the loader can drop it; HasDimension
// distinguishes "no dimension column" (no filter) from a present-but-empty one.
type SF1Row struct {
	Ticker       string
	DateKey      time.Time // filing date
	MarketCap    float64
	HasMarketCap bool   // false == null marketcap (dropped)
	Dimension    string // e.g. "MRT"
	HasDimension bool   // false == no dimension column in the source frame
}

// EarningsRow is one (ticker, report_date) earnings row (spec §7.4). The caller
// pre-filters SHARADAR/EVENTS to earnings rows (eventcode 22).
type EarningsRow struct {
	Ticker     string
	ReportDate time.Time
}

// ComputeRegime classifies the market regime from SPY daily history (spec §7.2).
//
// asOf (when non-zero) filters to bars with date <= asOf BEFORE any computation
// (look-ahead prevention). Classification order:
//  1. nil/empty or < 200 rows after filter -> neutral.
//  2. ma200 = rolling mean window 200, min_periods 200 (first 199 NaN).
//  3. last_ma NaN -> neutral.
//  4. last_close < last_ma (strict) -> bear. (== falls through.)
//  5. len(ma200) < 31 -> warning.
//  6. ma_then = ma200[-31]; NaN or 0 -> warning.
//  7. slope_pct = (last_ma - ma_then)/ma_then; > 0 -> bull, else warning.
func ComputeRegime(history []SPYBar, asOf time.Time) string {
	bars := history
	if !asOf.IsZero() {
		filtered := make([]SPYBar, 0, len(bars))
		cutoff := dateOnly(asOf)
		for _, b := range bars {
			if !dateOnly(b.Date).After(cutoff) {
				filtered = append(filtered, b)
			}
		}
		bars = filtered
	}
	if len(bars) < regimeMinBars {
		return RegimeNeutral
	}

	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	ma200 := rollingMean(closes, regimeMinBars)

	lastClose := closes[len(closes)-1]
	lastMA := ma200[len(ma200)-1]
	if math.IsNaN(lastMA) {
		return RegimeNeutral
	}

	if lastClose < lastMA { // strict; equality falls through (close==MA == "above")
		return RegimeBear
	}

	if len(ma200) < regimeSlopeWindow+1 {
		return RegimeWarning
	}
	maThen := ma200[len(ma200)-1-regimeSlopeWindow]
	if math.IsNaN(maThen) || maThen == 0 {
		return RegimeWarning
	}
	slopePct := (lastMA - maThen) / maThen
	if slopePct > regimeSlopeFlatPct {
		return RegimeBull
	}
	return RegimeWarning
}

// rollingMean reproduces pandas Series.rolling(window, min_periods=window).mean():
// element i is NaN for i < window-1, else the mean of the prior `window` values.
// A NaN inside the window poisons the mean to NaN (pandas sum propagates NaN).
func rollingMean(xs []float64, window int) []float64 {
	out := make([]float64, len(xs))
	for i := range xs {
		if i+1 < window {
			out[i] = math.NaN()
			continue
		}
		var sum float64
		for j := i + 1 - window; j <= i; j++ {
			sum += xs[j]
		}
		out[i] = sum / float64(window)
	}
	return out
}

// LoadSF1MarketCaps returns the latest known market cap per ticker as of asOf
// (spec §7.3).
//
// Steps:
//  1. nil/empty -> empty map.
//  2. If the source frame had a dimension column (HasDimension on any row),
//     keep only rows with Dimension == dimension; empty after filter -> {}.
//  3. Keep rows with DateKey <= asOf (inclusive).
//  4. Drop rows with null marketcap (!HasMarketCap) or marketcap <= 0.
//  5. Stable-sort by DateKey ascending and take the LAST row per ticker (ties on
//     DateKey: last in original input order wins — stable sort + tail).
//  6. Value = exact decimal of the float's shortest-repr string.
//
// Value strings carry a trailing ".0" for integral floats (e.g. 2.7e12 ->
// "2700000000000.0"); MarketCapDecString below renders that canonical form.
func LoadSF1MarketCaps(rows []SF1Row, asOf time.Time, dimension string) map[string]dec {
	out := map[string]dec{}
	if len(rows) == 0 {
		return out
	}
	if dimension == "" {
		dimension = sf1DimensionDefault
	}

	// Does the source frame have a dimension column at all? (Open question 10:
	// the Go boundary models the column's presence per-row; any row carrying
	// HasDimension means the column exists.)
	hasDimCol := false
	for _, r := range rows {
		if r.HasDimension {
			hasDimCol = true
			break
		}
	}

	cutoff := dateOnly(asOf)
	// Filter, preserving input order (stability requirement).
	type kept struct {
		row   SF1Row
		order int
	}
	var ks []kept
	for i, r := range rows {
		if hasDimCol && r.Dimension != dimension {
			continue
		}
		if dateOnly(r.DateKey).After(cutoff) {
			continue
		}
		if !r.HasMarketCap {
			continue
		}
		if r.MarketCap <= 0 {
			continue
		}
		ks = append(ks, kept{row: r, order: i})
	}
	if len(ks) == 0 {
		return out
	}

	// Stable sort by DateKey ascending; original order breaks ties.
	sort.SliceStable(ks, func(a, b int) bool {
		da, db := dateOnly(ks[a].row.DateKey), dateOnly(ks[b].row.DateKey)
		if da.Equal(db) {
			return ks[a].order < ks[b].order
		}
		return da.Before(db)
	})

	// tail(1) per ticker: the LAST occurrence in the sorted order wins.
	for _, k := range ks {
		out[k.row.Ticker] = decFromFloatStr(k.row.MarketCap)
	}
	return out
}

// LoadEarningsCalendar returns the per-ticker earnings-blackout flag as of asOf
// (spec §7.4).
//
// A ticker is in blackout iff ANY of its earnings dates d satisfies
// asOf-N <= d <= asOf+N calendar days, inclusive both ends (N = blackoutDays).
// Output contains ONLY true entries — tickers not in blackout are ABSENT.
func LoadEarningsCalendar(rows []EarningsRow, asOf time.Time, blackoutDays int) map[string]bool {
	out := map[string]bool{}
	if len(rows) == 0 {
		return out
	}
	cutoff := dateOnly(asOf)
	lo := cutoff.AddDate(0, 0, -blackoutDays)
	hi := cutoff.AddDate(0, 0, blackoutDays)
	for _, r := range rows {
		d := dateOnly(r.ReportDate)
		if !d.Before(lo) && !d.After(hi) { // lo <= d <= hi, inclusive
			out[r.Ticker] = true
		}
	}
	return out
}

// decFromFloatStr builds the exact decimal of the float's shortest decimal repr,
// then parses it exactly. strconv 'g'/-1 yields that shortest repr.
func decFromFloatStr(f float64) dec {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	d, ok := ParseDec(s)
	if !ok {
		return decZero()
	}
	return d
}

// dateOnly truncates ts to its UTC calendar date (midnight UTC). All as_of /
// datekey / report_date comparisons are calendar-date comparisons (spec §9: day
// boundaries by UTC calendar date).
func dateOnly(ts time.Time) time.Time {
	u := ts.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
