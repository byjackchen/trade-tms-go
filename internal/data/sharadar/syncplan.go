package sharadar

// syncplan.go is the pure (IO-free) planning layer of the API -> PG sync:
// catchup-day computation, bootstrap date chunking, ticker batching and the
// TICKERS survivorship row filter. Everything here is unit-tested against
// the Python reference oracles without a client or a database.

import (
	"fmt"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// sf1TickerBatchSize matches the Python _TICKER_BATCH_SIZE for SF1/EVENTS
// per-call ticker lists (spec §6.4/§6.5 [MUST-MATCH]).
const sf1TickerBatchSize = 500

// dateRange is one inclusive [Start, End] window.
type dateRange struct {
	Start calendar.Date
	End   calendar.Date
}

// dateChunks splits [start, end] into calendar-aligned windows of `months`
// width, inclusive on both ends — the exact _date_chunks algorithm of
// writer_sep.py (spec §6.1 [MUST-MATCH], oracle test_writer_sep.py:57-77):
// months must divide 12; with months=3 a full year yields the four
// quarters. Rationale (from the original): quarterly SEP pulls are ~220k
// rows/call, safely under the per-call ~1M row cap that half-year chunks
// hit in 2021-H2.
func dateChunks(start, end calendar.Date, months int) ([]dateRange, error) {
	switch months {
	case 1, 2, 3, 4, 6, 12:
	default:
		return nil, fmt.Errorf("sharadar: months must divide 12 evenly; got %d", months)
	}
	var out []dateRange
	for cur := start; !cur.After(end); {
		chunkEndMonth := ((int(cur.Month)-1)/months)*months + months
		if chunkEndMonth > 12 {
			chunkEndMonth = 12
		}
		var windowEnd calendar.Date
		if chunkEndMonth == 12 {
			windowEnd = calendar.NewDate(cur.Year, time.December, 31)
		} else {
			windowEnd = calendar.NewDate(cur.Year, time.Month(chunkEndMonth+1), 1).AddDays(-1)
		}
		chunkEnd := windowEnd
		if end.Before(chunkEnd) {
			chunkEnd = end
		}
		out = append(out, dateRange{Start: cur, End: chunkEnd})
		cur = chunkEnd.AddDays(1)
	}
	return out, nil
}

// tradingDays lists the days to catch up in [start, end] inclusive;
// end < start yields nil (spec §8.2 step 4).
//
// Deviation note ([IMPROVE], sanctioned by spec §8 and P1 locked decision
// 2): the original uses pandas bdate_range (Mon-Fri, holidays included as
// harmless zero-row API calls); here NYSE holidays are skipped via
// internal/data/calendar, saving ~9 wasted calls/yr × 2 datasets. Zero-row
// days remain harmless either way. Outside the calendar's covered range
// the weekday rule is used as a fallback, restoring the original behavior.
func tradingDays(cal *calendar.Calendar, start, end calendar.Date) []calendar.Date {
	if end.Before(start) {
		return nil
	}
	var out []calendar.Date
	for d := start; !d.After(end); d = d.AddDays(1) {
		trading, err := cal.IsTradingDay(d)
		if err != nil {
			// Out of calendar range: pandas bdate_range parity (weekdays).
			wd := d.Weekday()
			trading = wd != time.Saturday && wd != time.Sunday
		}
		if trading {
			out = append(out, d)
		}
	}
	return out
}

// catchupWindow derives the per-day SEP/SFP refresh range from the SEP
// watermark (spec §8.2 steps 3-4): start = the calendar date of the
// previous sync, target = today - 1 (T-1). The repull of the previous
// sync's own date is by design — idempotent merges make the overlap safe.
//
// Deviation note (P1 locked decision 2 [IMPROVE]): "today" and the
// watermark date are both taken as America/New_York trading dates via
// internal/data/calendar, where the original mixed UTC (catchup.py:109)
// and local dates (sync CLI). Documented in docs/spec/data-sharadar.md
// addendum.
func catchupWindow(lastSEPSync time.Time, today calendar.Date, loc *time.Location) (start, target calendar.Date) {
	return calendar.DateOf(lastSEPSync, loc), today.AddDays(-1)
}

// batchTickers splits tickers into batches of size (the Python
// _ticker_batches, spec §6.4). Empty input yields nil.
func batchTickers(tickers []string, size int) [][]string {
	if size <= 0 || len(tickers) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(tickers)+size-1)/size)
	for i := 0; i < len(tickers); i += size {
		j := i + size
		if j > len(tickers) {
			j = len(tickers)
		}
		out = append(out, tickers[i:j])
	}
	return out
}

// keepTickerRow is the TICKERS survivorship-bias row filter
// (writer_tickers.py:62-72; spec §2.5 [MUST-MATCH]):
//
//   - SF1 stocks: keep iff category startswith "Domestic Common Stock" —
//     both active AND delisted survive (survivor-bias-free backtests);
//     missing/NaN category is treated as "" and dropped;
//   - SFP funds: keep iff isdelisted == "N" — active only;
//   - anything else (SF2, SF3, ...) is dropped.
func keepTickerRow(table, category, isDelisted string) bool {
	switch table {
	case "SF1":
		return strings.HasPrefix(category, "Domestic Common Stock")
	case "SFP":
		return isDelisted == "N"
	default:
		return false
	}
}
