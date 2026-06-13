package parity

// barsfeed.go is the JSON bar feed for parity runs: it reads the bars.json file
// the Nautilus harness dumps (the EXACT wrangled OHLCV Nautilus consumed, prices
// already quantized to price_precision=2, volume integer) and adapts it to the
// engine.BarFeed contract. Reading the same wrangled inputs both sides consumed
// removes every source of input drift (DB round-trip, re-wrangling, adjusted vs
// raw close): the only thing under test is the ENGINE, not the data pipeline.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// BarsFile is the JSON shape of the shared bars.json artifact.
type BarsFile struct {
	Tickers []string             `json:"tickers"`
	Bars    map[string][]BarJSON `json:"bars"`
}

// BarJSON is one wrangled OHLCV row as the Nautilus harness serialized it
// (prices and volume are strings to preserve exact text; ts is ISO + ns).
type BarJSON struct {
	TS     string `json:"ts"`
	TSNs   int64  `json:"ts_ns"`
	Open   string `json:"open"`
	High   string `json:"high"`
	Low    string `json:"low"`
	Close  string `json:"close"`
	Volume string `json:"volume"`
}

// JSONFeed is a BarFeed backed by a parsed bars.json. It returns instruments in
// the file's registration (ticker) order, intersected with the requested
// tickers, mirroring engine.SliceFeed semantics.
type JSONFeed struct {
	instruments map[string]engine.InstrumentBars
	order       []string
}

// LoadBarsFeed reads bars.json at path into a JSONFeed.
func LoadBarsFeed(path string) (*JSONFeed, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parity: reading bars %s: %w", path, err)
	}
	var bf BarsFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		return nil, fmt.Errorf("parity: parsing bars %s: %w", path, err)
	}
	f := &JSONFeed{
		instruments: make(map[string]engine.InstrumentBars, len(bf.Bars)),
		order:       append([]string(nil), bf.Tickers...),
	}
	for _, ticker := range bf.Tickers {
		rows := bf.Bars[ticker]
		bars := make([]domain.Bar, 0, len(rows))
		for i, r := range rows {
			bar, err := r.toBar(ticker)
			if err != nil {
				return nil, fmt.Errorf("parity: %s bar %d: %w", ticker, i, err)
			}
			bars = append(bars, bar)
		}
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		f.instruments[ticker] = engine.InstrumentBars{Symbol: ticker, Bars: bars}
	}
	return f, nil
}

func (r BarJSON) toBar(ticker string) (domain.Bar, error) {
	o, err := domain.ParsePrice(r.Open)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("open %q: %w", r.Open, err)
	}
	h, err := domain.ParsePrice(r.High)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("high %q: %w", r.High, err)
	}
	l, err := domain.ParsePrice(r.Low)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("low %q: %w", r.Low, err)
	}
	c, err := domain.ParsePrice(r.Close)
	if err != nil {
		return domain.Bar{}, fmt.Errorf("close %q: %w", r.Close, err)
	}
	var vol int64
	if _, err := fmt.Sscan(r.Volume, &vol); err != nil {
		return domain.Bar{}, fmt.Errorf("volume %q: %w", r.Volume, err)
	}
	// Prefer the explicit ns timestamp (exact); fall back to parsing ts.
	var ts time.Time
	if r.TSNs != 0 {
		ts = time.Unix(0, r.TSNs).UTC()
	} else {
		t, err := time.Parse(time.RFC3339, r.TS)
		if err != nil {
			return domain.Bar{}, fmt.Errorf("ts %q: %w", r.TS, err)
		}
		ts = t.UTC()
	}
	return domain.Bar{
		Symbol: ticker,
		TS:     ts,
		Open:   o,
		High:   h,
		Low:    l,
		Close:  c,
		Volume: vol,
	}, nil
}

// Load implements engine.BarFeed: returns the requested tickers' bar series in
// ticker (registration) order. Unknown tickers yield empty series.
func (f *JSONFeed) Load(_ context.Context, tickers []string, _, _ calendar.Date) ([]engine.InstrumentBars, error) {
	out := make([]engine.InstrumentBars, 0, len(tickers))
	for _, t := range tickers {
		if ib, ok := f.instruments[t]; ok {
			out = append(out, ib)
		} else {
			out = append(out, engine.InstrumentBars{Symbol: t})
		}
	}
	return out, nil
}
