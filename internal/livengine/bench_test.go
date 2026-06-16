package livengine

// bench_test.go is part of the permanent benchmark suite (`make bench`). It
// measures the LIVE engine's per-bar processing latency: the cost of one bar
// flowing through Session.onBar (context injection skipped here; strategy OnBar;
// per-timestamp intent evaluation + emission to the sink). This is the latency
// that gates intent emission in signal/paper/live modes (deliverable (c):
// live engine per-bar latency / intent emission).
//
// The benchmark drives the deterministic Replay path (identical onBar to the
// streaming path, but without wall-clock waits), with N symbols per timestamp
// each carrying a stub strategy that emits a SignalIntent every bar — so the
// emission path is exercised on every timestamp. The reported ns/op is the
// per-(timestamp x symbol) bar latency.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// benchStrategy is a minimal engine.Strategy that submits one market order per
// bar (so the NoopExecutor records a would-be order) and exposes an intent via
// the IntentEvaluator seam (so the session's per-timestamp emission path runs).
type benchStrategy struct {
	id  string
	sym string
	n   int
}

func (s *benchStrategy) ID() string { return s.id }

func (s *benchStrategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	s.n++
	side := domain.OrderSideBuy
	signal := domain.SideLong
	if s.n%2 == 0 {
		side = domain.OrderSideSell
		signal = domain.SideShort
	}
	_, _, err := sub.SubmitMarketSignal(s.id, bar.Symbol, signal, side, 10, "bench", bar.TS)
	return err
}

// EvaluateIntentJSON satisfies engine.IntentEvaluator so the session emits an
// intent for this strategy each timestamp.
func (s *benchStrategy) EvaluateIntentJSON(asOf time.Time) any {
	return map[string]any{"ticker": s.sym, "side": "LONG", "as_of": asOf}
}

// benchSession builds a signal-mode session with nStrats stub strategies, each
// trading its own symbol, emitting to a DiscardSink (the emission machinery
// runs; the sink write is O(1)).
func benchSession(b *testing.B, nStrats int) (*Session, []string) {
	b.Helper()
	strats := make([]engine.Strategy, 0, nStrats)
	syms := make([]string, 0, nStrats)
	for i := 0; i < nStrats; i++ {
		sym := fmt.Sprintf("SYM%02d", i)
		syms = append(syms, sym)
		strats = append(strats, &benchStrategy{id: fmt.Sprintf("strat-%02d", i), sym: sym})
	}
	sess, err := NewSession(Config{
		Exec:            domain.ExecSignal,
		Strategies:      strats,
		StartingBalance: domain.MustMoney("1000000"),
		Sink:            DiscardSink{},
	})
	if err != nil {
		b.Fatalf("NewSession: %v", err)
	}
	return sess, syms
}

// benchBars builds nBars timestamps, each with one bar per symbol (so a
// timestamp rollover triggers per-timestamp emission), ascending in time.
func benchBars(syms []string, nBars int) []domain.Bar {
	bars := make([]domain.Bar, 0, nBars*len(syms))
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for t := 0; t < nBars; t++ {
		ts := start.AddDate(0, 0, t)
		for _, sym := range syms {
			c := domain.Price(int64(5_000_000 + t*10))
			bars = append(bars, domain.Bar{
				Symbol: sym, TS: ts,
				Open: c - 50, High: c + 100, Low: c - 100, Close: c,
				Volume: 1_000_000,
			})
		}
	}
	return bars
}

// BenchmarkLiveBarLatency_1strat measures the single-strategy per-bar latency
// (the tightest live loop: one symbol, one strategy, emit every bar). ns/op is
// the per-bar intent-emission latency deliverable.
func BenchmarkLiveBarLatency_1strat(b *testing.B) {
	benchLiveBars(b, 1)
}

// BenchmarkLiveBarLatency_10strat measures the per-bar latency under a 10-symbol
// universe (the realistic multi-strategy live node). ns/op divided by 10 is the
// per-(symbol) bar latency.
func BenchmarkLiveBarLatency_10strat(b *testing.B) {
	benchLiveBars(b, 10)
}

func benchLiveBars(b *testing.B, nStrats int) {
	const nBars = 256 // one trading year of timestamps
	b.ReportAllocs()
	ctx := context.Background()
	b.ResetTimer()
	var totalBars int
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sess, syms := benchSession(b, nStrats)
		bars := benchBars(syms, nBars)
		totalBars = len(bars)
		b.StartTimer()
		if err := sess.Replay(ctx, bars); err != nil {
			b.Fatalf("Replay: %v", err)
		}
	}
	b.StopTimer()
	// ns per individual bar (timestamp x symbol).
	nsPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	if totalBars > 0 {
		b.ReportMetric(nsPerOp/float64(totalBars), "ns/bar")
	}
}
