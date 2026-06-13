package livengine_test

// consistency_test.go is the ACCURACY ANCHOR for the live engine: with no
// Python live golden, internal consistency is the proof. It runs the LIVE engine
// (streaming, VirtualClock) over a day of bars delivered as a stream and asserts
// the emitted SignalIntents are IDENTICAL to what a BATCH replay of the same bars
// produces (decision 3 + 4). Both paths reuse the SAME strategy / portfolio /
// context / warmup code, so live path == batch path.
//
// The two runs use SEPARATE strategy assemblies (the generators mutate state +
// generation counters), built identically, so the comparison is meaningful.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

func ts(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func bar(sym string, y int, m time.Month, d int, close string, vol int64) domain.Bar {
	p := domain.MustPrice(close)
	return domain.Bar{Symbol: sym, TS: ts(y, m, d), Open: p, High: p, Low: p, Close: p, Volume: vol}
}

func wideSectorParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"},
		MomentumLookback: 2,
		TopK:             8,
		Timezone:         "America/New_York",
	}
}

// sectorInstruments builds 3 January warmup bars + a February rollover bar per
// ETF (rising, so all enter top-8 and a real rebalance fires).
func sectorInstruments() []engine.InstrumentBars {
	syms := []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"}
	out := make([]engine.InstrumentBars, 0, len(syms))
	for _, s := range syms {
		out = append(out, engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			bar(s, 2024, time.January, 2, "100.00", 1000),
			bar(s, 2024, time.January, 16, "105.00", 1000),
			bar(s, 2024, time.January, 31, "110.00", 1000),
			bar(s, 2024, time.February, 1, "111.00", 1000),
		}})
	}
	return out
}

// buildSectorSession assembles a signal-mode live Session over a fresh
// SectorRotation assembly writing into sink.
func buildSectorSession(t *testing.T, sink livengine.IntentSink) *livengine.Session {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sector_rotation",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: wideSectorParams()},
	})
	require.NoError(t, err)
	sess, err := livengine.NewSession(livengine.Config{
		Mode:            livengine.ModeSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            sink,
	})
	require.NoError(t, err)
	return sess
}

// canonicalIntents JSON-canonicalizes the recorded intents into comparable
// (AsOf, StrategyID, payloadJSON) tuples (the payload is the strategy's
// evaluate_intent result; JSON is the persistence/transport shape so equality
// of JSON is the right equality).
func canonicalIntents(t *testing.T, recs []livengine.IntentRecord) []string {
	t.Helper()
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r.Payload)
		require.NoError(t, err)
		out = append(out, r.AsOf.UTC().Format(time.RFC3339Nano)+"|"+r.StrategyID+"|"+string(b))
	}
	return out
}

// TestLiveStreamEqualsBatchReplay is the consistency proof: a streaming
// (virtual-clock) live run and a batch replay over the SAME bars emit IDENTICAL
// SignalIntents.
func TestLiveStreamEqualsBatchReplay(t *testing.T) {
	instruments := sectorInstruments()
	flat := livengine.BatchBars(instruments)
	require.NotEmpty(t, flat)

	// (1) Streaming live run over a VirtualClock.
	streamSink := livengine.NewMemSink()
	streamSess := buildSectorSession(t, streamSink)
	require.NoError(t, streamSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	feed := livengine.SliceStreamFeed{Bars: flat, Buffer: 4}
	require.NoError(t, streamSess.RunStream(context.Background(), feed, core.StreamVirtual, vc))

	// (2) Batch replay over the same bars.
	batchSink := livengine.NewMemSink()
	batchSess := buildSectorSession(t, batchSink)
	require.NoError(t, batchSess.Prime(context.Background()))
	require.NoError(t, batchSess.Replay(context.Background(), flat))

	// Both must have emitted intents (8 ETFs * 4 timestamps).
	require.NotEmpty(t, streamSink.Intents)
	require.Equal(t, len(streamSink.Intents), len(batchSink.Intents))

	// IDENTICAL intents (canonical order + canonical JSON payload).
	streamCanon := canonicalIntents(t, streamSink.SortedIntents())
	batchCanon := canonicalIntents(t, batchSink.SortedIntents())
	assert.Equal(t, batchCanon, streamCanon, "live stream intents must equal batch replay intents")

	// And the live engine placed NO orders (signal mode) but the strategy DID
	// fire (would-be orders > 0 on the rebalance).
	assert.Positive(t, streamSess.Executor().WouldSubmitCount(), "sector rebalance should have produced would-be orders")

	// Health snapshots emitted per timestamp (one per unique ts).
	assert.NotEmpty(t, streamSink.Health)
	for _, h := range streamSink.Health {
		assert.False(t, h.Snapshot.DailyLossHalt, "signal mode: no positions, no daily-loss halt")
	}
}

// TestLiveStreamDeterministic confirms two identical streaming runs emit
// identical intents (no wall-clock / map-iteration nondeterminism leaks in).
func TestLiveStreamDeterministic(t *testing.T) {
	flat := livengine.BatchBars(sectorInstruments())

	run := func() []string {
		sink := livengine.NewMemSink()
		sess := buildSectorSession(t, sink)
		require.NoError(t, sess.Prime(context.Background()))
		vc := core.NewVirtualClock(time.Time{})
		require.NoError(t, sess.RunStream(context.Background(),
			livengine.SliceStreamFeed{Bars: flat}, core.StreamVirtual, vc))
		return canonicalIntents(t, sink.SortedIntents())
	}
	assert.Equal(t, run(), run())
}

// sepaParams is a minimal valid SEPA param set.
func sepaParams() params.SEPAParams {
	return params.SEPAParams{
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000,
		Timezone: "America/New_York",
	}
}

// buildSEPASession assembles a signal-mode live SEPA session over one stock,
// with the given warmup provider + symbols.
func buildSEPASession(t *testing.T, sink livengine.IntentSink, warmup livengine.WarmupProvider, warmupSyms []string) *livengine.Session {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sepa",
		StartingBalance: 100000,
		SEPAStocks:      []string{"AAA"},
		Params:          strategyassembly.Params{SEPA: sepaParams()},
	})
	require.NoError(t, err)
	sess, err := livengine.NewSession(livengine.Config{
		Mode:            livengine.ModeSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		Warmup:          warmup,
		WarmupSymbols:   warmupSyms,
		Sink:            sink,
	})
	require.NoError(t, err)
	return sess
}

// TestLiveWarmupConsistency proves the warmup-priming seam (Prime) produces
// IDENTICAL live-stream and batch-replay intents: SEPA is primed from the SAME
// pre-window history in both paths, so its evaluate_intent state coincides.
func TestLiveWarmupConsistency(t *testing.T) {
	// 60 pre-window daily bars for AAA, gently rising (primes the SEPA history /
	// trend-template indicators out of band, exactly as warmup_ticker).
	hist := make([]domain.Bar, 0, 60)
	day := ts(2023, time.October, 2)
	for i := 0; i < 60; i++ {
		// 40.00 + i*0.25, formatted to 2dp so the exact price bridge is trivial.
		px := domain.MustPrice(fmt.Sprintf("%.2f", 40.0+float64(i)*0.25))
		hist = append(hist, domain.Bar{Symbol: "AAA", TS: day, Open: px, High: px, Low: px, Close: px, Volume: 1000})
		day = day.AddDate(0, 0, 1)
	}
	warmup := livengine.MapWarmupProvider{Bars: map[string][]domain.Bar{"AAA": hist}}

	// 5 in-window run bars.
	runInstruments := []engine.InstrumentBars{{Symbol: "AAA", Bars: []domain.Bar{
		bar("AAA", 2024, time.January, 2, "55.00", 2000),
		bar("AAA", 2024, time.January, 3, "56.00", 2000),
		bar("AAA", 2024, time.January, 4, "57.00", 2000),
		bar("AAA", 2024, time.January, 5, "58.00", 2000),
		bar("AAA", 2024, time.January, 8, "59.00", 2000),
	}}}
	flat := livengine.BatchBars(runInstruments)

	streamSink := livengine.NewMemSink()
	streamSess := buildSEPASession(t, streamSink, warmup, []string{"AAA"})
	require.NoError(t, streamSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, streamSess.RunStream(context.Background(),
		livengine.SliceStreamFeed{Bars: flat, Buffer: 2}, core.StreamVirtual, vc))

	batchSink := livengine.NewMemSink()
	batchSess := buildSEPASession(t, batchSink, warmup, []string{"AAA"})
	require.NoError(t, batchSess.Prime(context.Background()))
	require.NoError(t, batchSess.Replay(context.Background(), flat))

	require.NotEmpty(t, streamSink.Intents)
	assert.Equal(t,
		canonicalIntents(t, batchSink.SortedIntents()),
		canonicalIntents(t, streamSink.SortedIntents()),
		"warmed SEPA: live stream intents must equal batch replay intents")

	// The generation counter must advance identically (one EvaluateIntent per
	// timestamp): 5 run bars => generation 5 on the last intent.
	last := streamSink.SortedIntents()[len(streamSink.Intents)-1]
	b, err := json.Marshal(last.Payload)
	require.NoError(t, err)
	var got struct {
		Generation int64 `json:"generation"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, int64(5), got.Generation, "one intent generation per run-window timestamp")
}

// TestLiveSignalModeRejectsPaper confirms paper/live modes are not wired in P5.
func TestLiveSignalModeRejectsPaper(t *testing.T) {
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sector_rotation",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: wideSectorParams()},
	})
	require.NoError(t, err)
	_, err = livengine.NewSession(livengine.Config{
		Mode:            livengine.ModePaper,
		Strategies:      asm.Strategies,
		StartingBalance: domain.MustMoney("100000"),
	})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
