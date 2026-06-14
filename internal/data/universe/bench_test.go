package universe

// bench_test.go is part of the permanent benchmark suite (`make bench`). It
// measures the CPU-bound core of the data-import path: wrangling raw OHLCV rows
// (float64 from the parquet cache) into domain.Bars via the exact price bridge
// (float64 -> shortest-repr decimal -> 1e-4 fixed point). The DB CopyFrom is an
// I/O concern measured separately against a live stack; this isolates the
// per-row conversion cost that bounds import CPU throughput (deliverable (d):
// data import rows/sec).
//
// engine.WrangleOHLCV is the single shared wrangling entry point, but importing
// it here would create an import cycle (engine depends on universe). The price
// bridge it calls — domain.PriceFromFloat64 — is the actual hot per-row cost, so
// the benchmark exercises the same bridge over a representative OHLCV stream.

import (
	"math"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// benchOHLCVRows builds n deterministic raw OHLCV rows with realistic float64
// prices (the shape the Sharadar parquet cache yields).
func benchOHLCVRows(n int) []OHLCV {
	rows := make([]OHLCV, n)
	ts := time.Date(2010, 1, 4, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// Prices with sub-cent fractions so the bridge does real rounding work.
		base := 50.0 + math.Mod(float64(i)*0.0137, 200.0)
		rows[i] = OHLCV{
			TS:     ts.AddDate(0, 0, i),
			Open:   base - 0.0125,
			High:   base + 0.3375,
			Low:    base - 0.4225,
			Close:  base + 0.1175,
			Volume: float64(1_000_000 + i),
		}
	}
	return rows
}

// wrangleRow reproduces the per-row work of the import wrangling path: bridge
// the four prices through the fixed-point conversion and truncate volume. It is
// intentionally a local copy of the field-by-field bridge (the canonical
// engine.WrangleOHLCV wraps the same domain.PriceFromFloat64 calls) so this
// package-local benchmark has no import cycle.
func wrangleRow(symbol string, r OHLCV) (domain.Bar, error) {
	o, err := domain.PriceFromFloat64(r.Open)
	if err != nil {
		return domain.Bar{}, err
	}
	h, err := domain.PriceFromFloat64(r.High)
	if err != nil {
		return domain.Bar{}, err
	}
	l, err := domain.PriceFromFloat64(r.Low)
	if err != nil {
		return domain.Bar{}, err
	}
	c, err := domain.PriceFromFloat64(r.Close)
	if err != nil {
		return domain.Bar{}, err
	}
	return domain.Bar{
		Symbol: symbol,
		TS:     r.TS.UTC(),
		Open:   o, High: h, Low: l, Close: c,
		Volume: int64(math.Trunc(r.Volume)),
	}, nil
}

// BenchmarkImportWrangleRowsPerSec measures the per-row wrangling throughput
// (rows/sec) of the import hot path over a 100k-row stream — representative of a
// full Sharadar daily-bar dataset slice. Deliverable (d).
func BenchmarkImportWrangleRowsPerSec(b *testing.B) {
	const n = 100_000
	rows := benchOHLCVRows(n)
	b.ReportAllocs()
	b.ResetTimer()
	var sink int64
	for i := 0; i < b.N; i++ {
		for j := range rows {
			bar, err := wrangleRow("BENCH", rows[j])
			if err != nil {
				b.Fatalf("wrangle: %v", err)
			}
			sink += int64(bar.Close)
		}
	}
	b.StopTimer()
	_ = sink
	secPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / 1e9
	if secPerOp > 0 {
		b.ReportMetric(float64(n)/secPerOp, "rows/sec")
	}
}
