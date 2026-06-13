package livengine

// feed.go defines the live engine's bar feed seams: a streaming DataFeed (bars
// arriving over time, fed by the moomoo Qot_UpdateKL push OR the mock OpenD) and
// the warmup-history provider (pre-window klines, fed by Qot_RequestHistoryKL OR
// Postgres bars_daily/intraday). Both produce domain.Bars already wrangled to
// the exact price bridge (the moomoo client / mock and the PG loader wrangle at
// their boundary; here they are domain-ready).
//
// The streaming feed exposes a core.StreamSource (a channel of StreamEvents) the
// live loop drains. A live source pushes bars as the market produces them and
// closes the channel on graceful shutdown; the deterministic test source (a
// SliceStreamFeed) pushes a scripted day of bars and closes — driving a
// VirtualClock for the consistency proof.

import (
	"context"
	"sort"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// WarmupProvider supplies the out-of-band pre-window history used to prime
// WarmupConsumer strategies (SEPA) before the live loop starts — the live
// counterpart of engine.WarmupConfig. In production this is fed by the moomoo
// client's Qot_RequestHistoryKL (or the PG bars store); in tests by an
// in-memory map. It is queried ONCE per symbol at session start.
type WarmupProvider interface {
	// WarmupBars returns sym's pre-window history (ascending by ts, all strictly
	// before the run window). An unknown symbol returns an empty slice, nil error.
	WarmupBars(ctx context.Context, sym string) ([]domain.Bar, error)
}

// MapWarmupProvider is an in-memory WarmupProvider for tests and the PG-prefetch
// path (load once into a map, then serve). Symbols absent from the map warm up
// from nothing (the faithful asymmetry: non-SEPA strategies receive no warmup).
type MapWarmupProvider struct {
	Bars map[string][]domain.Bar
}

// WarmupBars returns the history for sym (empty if absent).
func (p MapWarmupProvider) WarmupBars(_ context.Context, sym string) ([]domain.Bar, error) {
	return p.Bars[sym], nil
}

// StreamFeed is the live engine's run-window bar source: it produces a
// core.StreamSource the loop drains. The producer (moomoo push / mock OpenD /
// test script) delivers bars in non-decreasing timestamp order and closes the
// channel at end-of-stream. Open is called once by the session, after warmup,
// with the context that bounds the run.
type StreamFeed interface {
	// Open starts delivery and returns the source the loop drains. The returned
	// source's channel must be closed by the producer on clean end-of-stream;
	// ctx cancellation must also stop the producer (no goroutine leak).
	Open(ctx context.Context) (core.StreamSource, error)
}

// SliceStreamFeed is a deterministic in-memory StreamFeed: it delivers a fixed,
// pre-ordered slice of bars (each wrapped as a core.BarEvent) and closes the
// channel. It is the consistency-proof + test driver (paired with a
// VirtualClock the loop advances to each bar's ts). The bars MUST already be in
// the intended dispatch order — typically interleaved by timestamp with the SPY
// heartbeat first within each timestamp (look-ahead-safe context), which the
// session's BatchBars helper produces from per-instrument series.
type SliceStreamFeed struct {
	// Bars is the flat, dispatch-ordered bar stream.
	Bars []domain.Bar
	// Buffer sizes the delivery channel (0 = unbuffered, fully lock-stepped with
	// the loop). A test may set a buffer to decouple producer/consumer timing;
	// determinism is unaffected because the loop drains in receive order.
	Buffer int
}

// Open spins a producer goroutine that pushes every bar as a BarEvent then
// closes the channel; it stops early if ctx is cancelled (no leak).
func (f SliceStreamFeed) Open(ctx context.Context) (core.StreamSource, error) {
	ch := make(chan core.StreamEvent, f.Buffer)
	go func() {
		defer close(ch)
		for _, b := range f.Bars {
			select {
			case <-ctx.Done():
				return
			case ch <- core.StreamEvent{Event: core.BarEvent{Bar: b}}:
			}
		}
	}()
	return core.NewChannelSource(ch), nil
}

// BatchBars flattens per-instrument bar series into a single dispatch-ordered
// stream: ascending by timestamp, and within a timestamp in REGISTRATION ORDER
// (the order instruments appear in the slice). This is the exact ordering the
// backtest engine's seed() produces (engine.go seed: per-instrument bars pushed
// in registration order, so same-ts bars carry ascending seq), so a stream/
// replay built from BatchBars dispatches identically to a backtest seeded from
// the same instruments — the foundation of the live==batch consistency proof.
//
// Callers MUST pass instruments with the SPY heartbeat FIRST so its bar
// dispatches before same-date stock bars (look-ahead-safe context), exactly as
// the assembler unions ExtraTickers with SPY first.
func BatchBars(instruments []engine.InstrumentBars) []domain.Bar {
	// Stable merge: build a flat list tagged with (ts, registrationIndex) and
	// sort by (ts asc, registrationIndex asc). A stable sort on a per-instrument-
	// ascending input keeps each instrument's bars in time order.
	type tagged struct {
		bar domain.Bar
		reg int
	}
	flat := make([]tagged, 0)
	for reg, ib := range instruments {
		for _, b := range ib.Bars {
			flat = append(flat, tagged{bar: b, reg: reg})
		}
	}
	sort.SliceStable(flat, func(i, j int) bool {
		if !flat[i].bar.TS.Equal(flat[j].bar.TS) {
			return flat[i].bar.TS.Before(flat[j].bar.TS)
		}
		return flat[i].reg < flat[j].reg
	})
	out := make([]domain.Bar, len(flat))
	for i, t := range flat {
		out[i] = t.bar
	}
	return out
}

// compile-time checks.
var (
	_ WarmupProvider = MapWarmupProvider{}
	_ StreamFeed     = SliceStreamFeed{}
)
