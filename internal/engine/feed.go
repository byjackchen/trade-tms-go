package engine

// feed.go defines the bar feed: the source of domain.Bars the engine schedules.
// A feed yields bars already wrangled to domain form (2-dp prices, UTC-midnight
// timestamps, integer volume). The engine schedules them as BarEvents in the
// deterministic total order — same-timestamp bars are enqueued in INSTRUMENT
// REGISTRATION order (locked decision 2), which the assembler guarantees by
// pushing per-instrument bar lists in registration order.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// InstrumentBars is one instrument's bar series, in ascending time order. The
// engine schedules instruments in registration order, so the index of an
// InstrumentBars in the registration slice is its tie-break rank.
type InstrumentBars struct {
	Symbol string
	Bars   []domain.Bar
}

// BarFeed yields per-instrument bar series for a run. Implementations must
// return instruments in a deterministic order; the assembler treats that order
// as the instrument registration order.
type BarFeed interface {
	// Load returns the bar series for each requested ticker over [start, end],
	// in registration order (typically the ticker argument order).
	Load(ctx context.Context, tickers []string, start, end calendar.Date) ([]InstrumentBars, error)
}

// StoreFeed adapts a universe.Store (TimescaleDB-backed BarHistoryProvider) to
// the BarFeed contract, wrangling OHLCV rows into domain.Bars with the exact
// price bridge.
type StoreFeed struct {
	store *universe.Store
}

// NewStoreFeed wraps a universe store.
func NewStoreFeed(store *universe.Store) *StoreFeed { return &StoreFeed{store: store} }

// Load reads each ticker's daily bars and wrangles them to domain.Bars. Ticker
// order is preserved as registration order. Bars with NaN prices/volume (source
// NULL) are skipped — they cannot be a valid OHLC bar — mirroring the
// reference, which would drop such rows before reaching Nautilus. Each
// instrument's bars are sorted ascending by ts.
func (f *StoreFeed) Load(ctx context.Context, tickers []string, start, end calendar.Date) ([]InstrumentBars, error) {
	out := make([]InstrumentBars, 0, len(tickers))
	for _, ticker := range tickers {
		rows, err := f.store.GetBars(ctx, ticker, start, end)
		if err != nil {
			return nil, fmt.Errorf("feed: loading %s: %w", ticker, err)
		}
		bars := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			if hasNaN(r) {
				continue
			}
			bar, err := wrangle(ticker, r)
			if err != nil {
				return nil, fmt.Errorf("feed: wrangling %s bar at %s: %w", ticker, r.TS.Format(time.RFC3339), err)
			}
			bars = append(bars, bar)
		}
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		out = append(out, InstrumentBars{Symbol: ticker, Bars: bars})
	}
	return out, nil
}

func hasNaN(r universe.OHLCV) bool {
	return math.IsNaN(r.Open) || math.IsNaN(r.High) || math.IsNaN(r.Low) ||
		math.IsNaN(r.Close) || math.IsNaN(r.Volume)
}

// WrangleOHLCV converts one OHLCV row to a domain.Bar with the exact price
// bridge (float -> shortest-repr decimal -> 1e-4 fixed point), volume truncated
// toward zero. It is the single wrangling entry point shared by the StoreFeed
// (run-window bars) and the out-of-band warmup loader (pre-window bars) so both
// produce identically-bridged domain.Bars (spec §1).
func WrangleOHLCV(symbol string, r universe.OHLCV) (domain.Bar, error) {
	return wrangle(symbol, r)
}

// wrangle converts one OHLCV row to a domain.Bar with the exact price bridge
// (float -> shortest-repr decimal -> 1e-4 fixed point). Volume truncates toward
// zero (int(bar.volume), §1).
func wrangle(symbol string, r universe.OHLCV) (domain.Bar, error) {
	o, err := domain.PriceFromFloat64(r.Open)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("open: %w", err)
	}
	h, err := domain.PriceFromFloat64(r.High)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("high: %w", err)
	}
	l, err := domain.PriceFromFloat64(r.Low)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("low: %w", err)
	}
	c, err := domain.PriceFromFloat64(r.Close)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("close: %w", err)
	}
	bar := domain.Bar{
		Symbol: symbol,
		TS:     r.TS.UTC(),
		Open:   o,
		High:   h,
		Low:    l,
		Close:  c,
		Volume: int64(math.Trunc(r.Volume)),
	}
	return bar, nil
}

// SliceFeed is an in-memory feed for tests and the parity harness: it returns a
// fixed set of instrument series in the given order (= registration order).
type SliceFeed struct {
	Instruments []InstrumentBars
}

// Load returns the configured instruments whose symbol is in tickers, in the
// ticker argument order (registration order). Unknown tickers yield empty
// series. start/end are ignored (the caller pre-trims).
func (f SliceFeed) Load(_ context.Context, tickers []string, _, _ calendar.Date) ([]InstrumentBars, error) {
	bySym := make(map[string]InstrumentBars, len(f.Instruments))
	for _, ib := range f.Instruments {
		bySym[ib.Symbol] = ib
	}
	out := make([]InstrumentBars, 0, len(tickers))
	for _, t := range tickers {
		if ib, ok := bySym[t]; ok {
			out = append(out, ib)
		} else {
			out = append(out, InstrumentBars{Symbol: t})
		}
	}
	return out, nil
}
