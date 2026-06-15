package study

// dataset.go loads the SHARED, READ-ONLY bar dataset a study evaluates over —
// once, up front (locked decision 5). Every trial and every fold reads from this
// immutable in-memory dataset; no worker ever touches the database during the
// optimization loop, and no mutable state is shared between concurrent engine
// instances. The dataset spans the full study window PLUS the strategy warmup
// buffer (400 calendar days before start; SPY 500 — §1.6) so every fold has its
// warmup history available without re-querying.
//
// The per-fold feed (foldFeed) trims the shared series to a fold's window plus
// the same warmup buffer at fold-evaluation time. Trimming is a pure read over
// the shared slices (no copy of bar structs), so it is safe for concurrent use.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// warmupDaysDefault is the calendar-day warmup loaded before a window start for
// ticker history (§1.6 / multi_strategy_backtest.py:404-411).
const warmupDaysDefault = 400

// spyWarmupDays is the SPY regime warmup (~500 days; §1.6).
const spyWarmupDays = 500

// Dataset is the immutable, shared bar dataset for one study. instruments are in
// registration order (SPY first when present, then the rest), each series sorted
// ascending by ts. It is built once and read by every trial/fold concurrently.
type Dataset struct {
	instruments []engine.InstrumentBars
	bySym       map[string]engine.InstrumentBars
	// marketCaps is the latest-known (look-ahead-safe as of the study window)
	// SF1 market cap per SEPA stock, loaded ONCE up front (locked decision 5: no
	// DB during the optimization loop). 0 == unknown (fails the SEPA rule-8 cap
	// gate, sorts last) — same contract as universe.Store.MarketCaps. nil/empty
	// means no caps were threaded in (cold-start: every name reads 0 and the SEPA
	// objective degenerates to 0, the pre-fix behaviour); the hyperopt handler
	// populates this from tms.fundamentals_sf1 so the objective is non-degenerate.
	marketCaps map[string]float64
}

// SetMarketCaps attaches the look-ahead-safe per-ticker market caps to the
// dataset (loaded once from tms.fundamentals_sf1 by the caller, mirroring the
// production backtest context provider). Keys are tickers; 0 means unknown.
// Called exactly once before the dataset is shared with concurrent evaluators,
// so no synchronization is needed.
func (d *Dataset) SetMarketCaps(caps map[string]float64) {
	if len(caps) == 0 {
		return
	}
	d.marketCaps = make(map[string]float64, len(caps))
	for k, v := range caps {
		d.marketCaps[k] = v
	}
}

// MarketCap returns the look-ahead-safe market cap for ticker (0 when unknown or
// no caps were loaded). 0 fails the SEPA rule-8 cap gate, exactly as the
// universe.Store.MarketCaps contract specifies.
func (d *Dataset) MarketCap(ticker string) float64 {
	if d.marketCaps == nil {
		return 0
	}
	return d.marketCaps[ticker]
}

// LoadDataset reads every instrument's bars over [start - max(warmup), end] from
// the feed and returns an immutable Dataset. tickers must already be in the
// desired registration order (SPY first for look-ahead-safe context). The window
// is widened by the SPY warmup so both the ticker (400d) and SPY (500d) warmups
// are covered by one load.
func LoadDataset(ctx context.Context, feed engine.BarFeed, tickers []string, start, end calendar.Date) (*Dataset, error) {
	if len(tickers) == 0 {
		return nil, fmt.Errorf("hyperopt: dataset needs at least one ticker")
	}
	loadStart := start.AddDays(-spyWarmupDays)
	ibs, err := feed.Load(ctx, tickers, loadStart, end)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: loading shared dataset: %w", err)
	}
	bySym := make(map[string]engine.InstrumentBars, len(ibs))
	for _, ib := range ibs {
		// Defensive: ensure ascending order (StoreFeed already sorts).
		bars := ib.Bars
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		bySym[ib.Symbol] = engine.InstrumentBars{Symbol: ib.Symbol, Bars: bars}
	}
	// Preserve the requested registration order.
	ordered := make([]engine.InstrumentBars, 0, len(tickers))
	for _, t := range tickers {
		if ib, ok := bySym[t]; ok {
			ordered = append(ordered, ib)
		} else {
			ordered = append(ordered, engine.InstrumentBars{Symbol: t})
		}
	}
	return &Dataset{instruments: ordered, bySym: bySym}, nil
}

// NewDatasetFromInstruments builds a Dataset from already-loaded instrument bar
// series (in registration order). Each series is sorted ascending by ts. This is
// the in-memory constructor used by tests and any caller that pre-loads bars
// without a feed; the resulting Dataset is immutable and safe for concurrent use.
func NewDatasetFromInstruments(ibs []engine.InstrumentBars) *Dataset {
	bySym := make(map[string]engine.InstrumentBars, len(ibs))
	ordered := make([]engine.InstrumentBars, 0, len(ibs))
	for _, ib := range ibs {
		bars := append([]domain.Bar(nil), ib.Bars...)
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		cp := engine.InstrumentBars{Symbol: ib.Symbol, Bars: bars}
		bySym[ib.Symbol] = cp
		ordered = append(ordered, cp)
	}
	return &Dataset{instruments: ordered, bySym: bySym}
}

// Tickers returns the registration-ordered instrument symbols.
func (d *Dataset) Tickers() []string {
	out := make([]string, len(d.instruments))
	for i, ib := range d.instruments {
		out[i] = ib.Symbol
	}
	return out
}

// foldFeed is an engine.BarFeed over the shared Dataset, trimming each
// instrument's series to EXACTLY [start, end] (UTC-midnight calendar bounds) —
// the run window only, NO preceding warmup tail (warmup is injected out-of-band
// via WarmupSlices, never replayed through the engine loop; spec §3 / Python's
// warmup_by_ticker). It is read-only and safe for concurrent use across trials:
// every Load returns a fresh slice header over the shared, never-mutated backing
// array.
type foldFeed struct {
	ds *Dataset
}

// WindowFeed returns a BarFeed that trims each instrument to EXACTLY the
// requested [start, end] window — NO preceding warmup tail. This is the
// engine feed for the parity-correct path: the event loop replays only the
// test/run window, and SEPA warmup is supplied separately via WarmupSlices
// (mirroring Python, where the 400d warmup goes to warmup_by_ticker and is
// never replayed through the Nautilus engine).
func (d *Dataset) WindowFeed() engine.BarFeed {
	return &foldFeed{ds: d}
}

// WarmupSlices returns, for each requested ticker, the pre-window history in
// [start.AddDays(-warmupDays), start) — i.e. strictly BEFORE the run window —
// for out-of-band warmup priming (engine.WarmupConfig.Bars). Bars are shared
// (immutable backing array), not copied. Tickers with no pre-window history are
// omitted. This is the Go equivalent of multi_strategy_backtest.py's
// warmup_by_ticker (bars < run_start_ts).
func (d *Dataset) WarmupSlices(tickers []string, start calendar.Date, warmupDays int) map[string][]domain.Bar {
	if warmupDays <= 0 {
		warmupDays = warmupDaysDefault
	}
	lo := midnight(start.AddDays(-warmupDays))
	hi := midnight(start) // exclusive of the run-window start bar
	out := make(map[string][]domain.Bar, len(tickers))
	for _, t := range tickers {
		ib, ok := d.bySym[t]
		if !ok {
			continue
		}
		// [lo, hi): warmup tail strictly before the run window's first bar.
		w := trimHalfOpen(ib.Bars, lo, hi)
		if len(w) > 0 {
			out[t] = w
		}
	}
	return out
}

// Load implements engine.BarFeed. It returns, for each requested ticker (in
// argument order = registration order), the shared series trimmed to
// [start midnight, end midnight] inclusive — the run window only. Bars are not
// copied (the backing array is immutable), only the slice bounds change.
func (f *foldFeed) Load(_ context.Context, tickers []string, start, end calendar.Date) ([]engine.InstrumentBars, error) {
	lo := midnight(start)
	hi := midnight(end)
	out := make([]engine.InstrumentBars, 0, len(tickers))
	for _, t := range tickers {
		ib, ok := f.ds.bySym[t]
		if !ok {
			out = append(out, engine.InstrumentBars{Symbol: t})
			continue
		}
		out = append(out, engine.InstrumentBars{Symbol: t, Bars: trim(ib.Bars, lo, hi)})
	}
	return out, nil
}

// trim returns the sub-slice of bars whose ts is within [lo, hi] inclusive.
// bars are ascending by ts; binary search bounds the window.
func trim(bars []domain.Bar, lo, hi time.Time) []domain.Bar {
	// first index with ts >= lo
	i := sort.Search(len(bars), func(i int) bool { return !bars[i].TS.Before(lo) })
	// first index with ts > hi
	j := sort.Search(len(bars), func(j int) bool { return bars[j].TS.After(hi) })
	if i >= j {
		return nil
	}
	return bars[i:j]
}

// trimHalfOpen returns the sub-slice of bars whose ts is within [lo, hi) —
// inclusive of lo, EXCLUSIVE of hi. bars are ascending by ts. Used for the
// warmup tail (strictly before the run-window start bar).
func trimHalfOpen(bars []domain.Bar, lo, hi time.Time) []domain.Bar {
	i := sort.Search(len(bars), func(i int) bool { return !bars[i].TS.Before(lo) })
	j := sort.Search(len(bars), func(j int) bool { return !bars[j].TS.Before(hi) })
	if i >= j {
		return nil
	}
	return bars[i:j]
}

func midnight(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}
