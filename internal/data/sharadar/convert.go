package sharadar

// convert.go holds the pure conversion layer: parquet cell values -> SQL
// cell values ([]any rows ready for pgx CopyFrom). All numeric semantics are
// pinned to the Python reference:
//
//   - Prices: float64 -> int64 1e-4 fixed point via domain.PriceFromFloat64,
//     i.e. Decimal(str(x)).quantize(Decimal("0.0001"), ROUND_HALF_EVEN) —
//     the documented project rounding (docs/spec/domain-types-money.md §1.2).
//     The narrowing from the reference cache's raw float64 is sanctioned by
//     the [IMPROVE] note in docs/spec/data-sharadar.md §2.1: out-of-range
//     values (±Inf, |x| > ~9.22e14 USD) are stored NULL and surfaced in
//     ImportStats.FieldsNulled; the parquet layer keeps raw float64 and
//     remains the round-trip-parity surface (spec §12).
//     NaN/NULL -> SQL NULL (NaN tickers are dropped by consumers, never
//     cleaned in the store, spec §2.1).
//   - Volume: float64 (NaN-able upstream) -> int64 by truncation toward
//     zero, the Python int(row["volume"]) consumer cast (spec §2.1);
//     NaN/NULL -> SQL NULL. Negative or out-of-int64-range values are
//     unrepresentable in the schema (CHECK volume >= 0) and become NULL,
//     counted as field warnings.
//   - Dates: tz-naive cache timestamps -> UTC midnight (same instant as the
//     engine's tz_localize("UTC"), spec §2.6).

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// errSkipRow tags row-level conversion failures: the row is skipped and
// counted, the file import continues.
var errSkipRow = errors.New("sharadar: row skipped")

// sf1Dimensions is the closed set of SF1 reporting dimensions (spec §2.3);
// all six coexist per (ticker, datekey).
var sf1Dimensions = map[string]struct{}{
	"ARQ": {}, "ART": {}, "ARY": {}, "MRQ": {}, "MRT": {}, "MRY": {},
}

// sf1MetricColumns is the full SF1 metric column set, alphabetical, exactly
// as observed in the production cache parquet and pinned by migration
// 000002 (105 DOUBLE PRECISION columns; spec §2.3 — cached verbatim, no
// pruning).
var sf1MetricColumns = []string{
	"accoci", "assets", "assetsavg", "assetsc", "assetsnc", "assetturnover",
	"bvps", "capex", "cashneq", "cashnequsd", "cor", "consolinc",
	"currentratio", "de", "debt", "debtc", "debtnc", "debtusd",
	"deferredrev", "depamor", "deposits", "divyield", "dps", "ebit",
	"ebitda", "ebitdamargin", "ebitdausd", "ebitusd", "ebt", "eps",
	"epsdil", "epsusd", "equity", "equityavg", "equityusd", "ev",
	"evebit", "evebitda", "fcf", "fcfps", "fxusd", "gp",
	"grossmargin", "intangibles", "intexp", "invcap", "invcapavg", "inventory",
	"investments", "investmentsc", "investmentsnc", "liabilities", "liabilitiesc", "liabilitiesnc",
	"marketcap", "ncf", "ncfbus", "ncfcommon", "ncfdebt", "ncfdiv",
	"ncff", "ncfi", "ncfinv", "ncfo", "ncfx", "netinc",
	"netinccmn", "netinccmnusd", "netincdis", "netincnci", "netmargin", "opex",
	"opinc", "payables", "payoutratio", "pb", "pe", "pe1",
	"ppnenet", "prefdivis", "price", "ps", "ps1", "receivables",
	"retearn", "revenue", "revenueusd", "rnd", "roa", "roe",
	"roic", "ros", "sbcomp", "sgna", "sharefactor", "sharesbas",
	"shareswa", "shareswadil", "sps", "tangibles", "taxassets", "taxexp",
	"taxliabilities", "tbvps", "workingcapital",
}

// ---------------------------------------------------------------------------
// Scalar cell conversions
// ---------------------------------------------------------------------------

// priceCell converts a float price to the SQL cell for a BIGINT 1e-4
// fixed-point column. Returns (cell, nulledUnrepresentable):
//
//	NULL / NaN          -> (nil, false)  — faithful NULL, not a warning
//	finite representable -> (int64, false) via the Decimal(str(x)) half-even bridge
//	±Inf / out of range  -> (nil, true)   — unrepresentable, stored NULL + counted
func priceCell(v float64, valid bool) (any, bool) {
	if !valid || math.IsNaN(v) {
		return nil, false
	}
	p, err := domain.PriceFromFloat64(v)
	if err != nil {
		return nil, true
	}
	return p.Raw(), false
}

// volumeCell converts the float volume column (NaN-able upstream, spec §2.1)
// to a BIGINT cell by truncation toward zero — Python's int(float). Negative
// or out-of-range values are unrepresentable under the schema CHECK and
// become (nil, true).
func volumeCell(v float64, valid bool) (any, bool) {
	if !valid || math.IsNaN(v) {
		return nil, false
	}
	t := math.Trunc(v)
	if t < 0 || t >= math.MaxInt64 || math.IsInf(t, 0) {
		return nil, true
	}
	return int64(t), false
}

// dateCell converts an optional temporal value to a DATE cell (UTC
// midnight) or NULL.
func dateCell(t time.Time, valid bool) any {
	if !valid {
		return nil
	}
	return utcMidnight(t)
}

// ---------------------------------------------------------------------------
// Per-dataset row converters. Each returns the CopyFrom row (without the
// staging seq column, which the loader prepends), the count of fields
// nulled because they were unrepresentable, and an error wrapping errSkipRow
// for rows that cannot be stored at all.
// ---------------------------------------------------------------------------

// barColumns is the staging/upsert column order for SEP/SFP bars
// (tms.bars_daily).
var barColumns = []string{
	"ticker", "ts", "source",
	"open", "high", "low", "close", "volume",
	"close_adj", "close_unadj", "dividends", "last_updated",
}

// convertBarRow converts one SEP/SFP row. source must be "SEP" or "SFP".
func convertBarRow(rec arrow.Record, c colmap, row int, source string) ([]any, int, error) {
	ticker, ok := stringCell(rec, c.idx("ticker"), row)
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	date, ok := timeCell(rec, c.idx("date"), row)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing date", errSkipRow, ticker)
	}

	nulled := 0
	price := func(name string) any {
		v, valid := floatCell(rec, c.idx(name), row)
		cell, warn := priceCell(v, valid)
		if warn {
			nulled++
		}
		return cell
	}
	volV, volValid := floatCell(rec, c.idx("volume"), row)
	vol, volWarn := volumeCell(volV, volValid)
	if volWarn {
		nulled++
	}
	lastUpdated, luOK := timeCell(rec, c.idx("lastupdated"), row)

	return []any{
		ticker, utcMidnight(date), source,
		price("open"), price("high"), price("low"), price("close"), vol,
		price("closeadj"), price("closeunadj"), price("dividends"), dateCell(lastUpdated, luOK),
	}, nulled, nil
}

// tickerColumns is the staging/upsert column order for tms.tickers.
var tickerColumns = []string{
	"ticker", "name", "exchange", "is_delisted", "category", "sector",
	"industry", "table_name", "first_price_date", "last_price_date", "delist_date",
}

// convertTickerRow converts one TICKERS row. The on-disk file is already
// filtered/pruned by the Python writer (spec §2.5); this converter only maps
// representations: isdelisted "Y"/"N" -> bool, NaT/"" last price date ->
// NULL ("still active"), the never-present delistedate keep-column -> NULL.
func convertTickerRow(rec arrow.Record, c colmap, row int) ([]any, int, error) {
	ticker, ok := stringCell(rec, c.idx("ticker"), row)
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	tableName, ok := stringCell(rec, c.idx("table"), row)
	if !ok || (tableName != "SF1" && tableName != "SFP") {
		return nil, 0, fmt.Errorf("%w: %s has table %q (want SF1|SFP)", errSkipRow, ticker, tableName)
	}
	isDelistedStr, ok := stringCell(rec, c.idx("isdelisted"), row)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing isdelisted", errSkipRow, ticker)
	}
	var isDelisted bool
	switch isDelistedStr {
	case "Y":
		isDelisted = true
	case "N":
		isDelisted = false
	default:
		return nil, 0, fmt.Errorf("%w: %s has isdelisted %q (want Y|N)", errSkipRow, ticker, isDelistedStr)
	}

	optStr := func(name string) any {
		if s, ok := stringCell(rec, c.idx(name), row); ok {
			return s
		}
		return nil
	}
	first, firstOK := dateFlexCell(rec, c.idx("firstpricedate"), row)
	last, lastOK := dateFlexCell(rec, c.idx("lastpricedate"), row)
	delist, delistOK := dateFlexCell(rec, c.idx("delistedate"), row)

	return []any{
		ticker, optStr("name"), optStr("exchange"), isDelisted, optStr("category"),
		optStr("sector"), optStr("industry"), tableName,
		dateCell(first, firstOK), dateCell(last, lastOK), dateCell(delist, delistOK),
	}, 0, nil
}

// sf1Columns is the staging/upsert column order for tms.fundamentals_sf1:
// the 7 key columns followed by the 105 metric columns.
var sf1Columns = append([]string{
	"ticker", "dimension", "calendardate", "datekey", "reportperiod",
	"fiscalperiod", "lastupdated",
}, sf1MetricColumns...)

// convertSF1Row converts one SF1 fundamentals row. Metric doubles pass
// through unrounded (they are analytics inputs, consumed as
// Decimal(str(float)) by the reference, spec §2.3); NaN/NULL -> SQL NULL.
func convertSF1Row(rec arrow.Record, c colmap, row int) ([]any, int, error) {
	ticker, ok := stringCell(rec, c.idx("ticker"), row)
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	dimension, ok := stringCell(rec, c.idx("dimension"), row)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing dimension", errSkipRow, ticker)
	}
	if _, valid := sf1Dimensions[dimension]; !valid {
		return nil, 0, fmt.Errorf("%w: %s has dimension %q (want ARQ|ART|ARY|MRQ|MRT|MRY)", errSkipRow, ticker, dimension)
	}
	datekey, ok := timeCell(rec, c.idx("datekey"), row)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s/%s missing datekey", errSkipRow, ticker, dimension)
	}

	calendarDate, calOK := timeCell(rec, c.idx("calendardate"), row)
	reportPeriod, repOK := timeCell(rec, c.idx("reportperiod"), row)
	lastUpdated, luOK := timeCell(rec, c.idx("lastupdated"), row)
	var fiscalPeriod any
	if s, ok := stringCell(rec, c.idx("fiscalperiod"), row); ok {
		fiscalPeriod = s
	}

	vals := make([]any, 0, len(sf1Columns))
	vals = append(vals,
		ticker, dimension, dateCell(calendarDate, calOK), utcMidnight(datekey),
		dateCell(reportPeriod, repOK), fiscalPeriod, dateCell(lastUpdated, luOK),
	)
	for _, name := range sf1MetricColumns {
		if v, valid := floatCell(rec, c.idx(name), row); valid && !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, v)
		} else {
			vals = append(vals, nil)
		}
	}
	return vals, 0, nil
}

// eventColumns is the staging/upsert column order for tms.events.
var eventColumns = []string{"ticker", "event_date", "eventcodes"}

// convertEventRow converts one EVENTS row. eventcodes is stored verbatim as
// the pipe-separated code string — the earnings "22" exact-member filter is
// a consumer concern (spec §2.4).
func convertEventRow(rec arrow.Record, c colmap, row int) ([]any, int, error) {
	ticker, ok := stringCell(rec, c.idx("ticker"), row)
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	date, ok := timeCell(rec, c.idx("date"), row)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing date", errSkipRow, ticker)
	}
	codes, ok := stringCell(rec, c.idx("eventcodes"), row)
	if !ok || codes == "" {
		return nil, 0, fmt.Errorf("%w: %s@%s missing eventcodes", errSkipRow, ticker, utcMidnight(date).Format("2006-01-02"))
	}
	return []any{ticker, utcMidnight(date), codes}, 0, nil
}
