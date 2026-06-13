package sharadar

// syncconvert.go converts Nasdaq Data Link API rows (wire.go Row) into the
// same SQL cell rows the parquet importer produces (convert.go), so both
// ingestion paths share the staging plans, merge SQL and numeric semantics:
// prices through the Decimal(str(x)) half-even 1e-4 fixed-point bridge,
// NaN/null -> SQL NULL, dates -> UTC midnight (spec §2.6).
//
// P1 locked decision 3: the Python keep-column 'delistedate'
// (writer_tickers.py:47) is a typo never present in real API output (spec
// Q2). The Go sync reads 'delistdate' instead and drops the dead spelling;
// see the addendum in docs/spec/data-sharadar.md.

import (
	"fmt"
	"math"
)

// convertBarAPIRow maps one SEP/SFP API row onto barColumns order.
// Returns (row, fieldsNulled, error wrapping errSkipRow).
func convertBarAPIRow(r Row, source string) ([]any, int, error) {
	ticker, ok := r.Str("ticker")
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	date, ok := r.Date("date")
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing date", errSkipRow, ticker)
	}

	nulled := 0
	price := func(name string) any {
		v, valid := r.Float(name)
		cell, warn := priceCell(v, valid)
		if warn {
			nulled++
		}
		return cell
	}
	volV, volValid := r.Float("volume")
	vol, volWarn := volumeCell(volV, volValid)
	if volWarn {
		nulled++
	}
	lastUpdated, luOK := r.Date("lastupdated")

	return []any{
		ticker, utcMidnight(date), source,
		price("open"), price("high"), price("low"), price("close"), vol,
		price("closeadj"), price("closeunadj"), price("dividends"), dateCell(lastUpdated, luOK),
	}, nulled, nil
}

// convertTickerAPIRow maps one TICKERS API row onto tickerColumns order.
// The caller applies keepTickerRow first; this converter only maps
// representations: isdelisted "Y"/"N" -> bool, empty-string/invalid price
// dates -> NULL (pandas to_datetime(errors="coerce") parity, spec §2.5),
// and delistdate (not the dead 'delistedate', P1 locked decision 3).
func convertTickerAPIRow(r Row) ([]any, int, error) {
	ticker, ok := r.Str("ticker")
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	tableName, ok := r.Str("table")
	if !ok || (tableName != "SF1" && tableName != "SFP") {
		return nil, 0, fmt.Errorf("%w: %s has table %q (want SF1|SFP)", errSkipRow, ticker, tableName)
	}
	isDelistedStr, ok := r.Str("isdelisted")
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
		if s, ok := r.Str(name); ok {
			return s
		}
		return nil
	}
	first, firstOK := r.Date("firstpricedate")
	last, lastOK := r.Date("lastpricedate")
	delist, delistOK := r.Date("delistdate")

	return []any{
		ticker, optStr("name"), optStr("exchange"), isDelisted, optStr("category"),
		optStr("sector"), optStr("industry"), tableName,
		dateCell(first, firstOK), dateCell(last, lastOK), dateCell(delist, delistOK),
	}, 0, nil
}

// convertSF1APIRow maps one SF1 API row onto sf1Columns order. Metric
// doubles pass through unrounded (analytics inputs, spec §2.3); NaN/Inf/
// null -> SQL NULL.
func convertSF1APIRow(r Row) ([]any, int, error) {
	ticker, ok := r.Str("ticker")
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	dimension, ok := r.Str("dimension")
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing dimension", errSkipRow, ticker)
	}
	if _, valid := sf1Dimensions[dimension]; !valid {
		return nil, 0, fmt.Errorf("%w: %s has dimension %q (want ARQ|ART|ARY|MRQ|MRT|MRY)", errSkipRow, ticker, dimension)
	}
	datekey, ok := r.Date("datekey")
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s/%s missing datekey", errSkipRow, ticker, dimension)
	}

	calendarDate, calOK := r.Date("calendardate")
	reportPeriod, repOK := r.Date("reportperiod")
	lastUpdated, luOK := r.Date("lastupdated")
	var fiscalPeriod any
	if s, ok := r.Str("fiscalperiod"); ok {
		fiscalPeriod = s
	}

	vals := make([]any, 0, len(sf1Columns))
	vals = append(vals,
		ticker, dimension, dateCell(calendarDate, calOK), utcMidnight(datekey),
		dateCell(reportPeriod, repOK), fiscalPeriod, dateCell(lastUpdated, luOK),
	)
	for _, name := range sf1MetricColumns {
		if v, valid := r.Float(name); valid && !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, v)
		} else {
			vals = append(vals, nil)
		}
	}
	return vals, 0, nil
}

// convertEventAPIRow maps one EVENTS API row onto eventColumns order.
// eventcodes is stored verbatim as the pipe-separated code string (the
// "22" exact-member earnings filter is a consumer concern, spec §2.4); a
// numerically encoded single code (e.g. 22) round-trips as its integer
// string via Row.Str.
func convertEventAPIRow(r Row) ([]any, int, error) {
	ticker, ok := r.Str("ticker")
	if !ok || ticker == "" {
		return nil, 0, fmt.Errorf("%w: missing ticker", errSkipRow)
	}
	date, ok := r.Date("date")
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s missing date", errSkipRow, ticker)
	}
	codes, ok := r.Str("eventcodes")
	if !ok || codes == "" {
		return nil, 0, fmt.Errorf("%w: %s@%s missing eventcodes", errSkipRow, ticker, utcMidnight(date).Format("2006-01-02"))
	}
	return []any{ticker, utcMidnight(date), codes}, 0, nil
}
