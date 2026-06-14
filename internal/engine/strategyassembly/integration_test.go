package strategyassembly_test

// integration_test.go drives the real strategy adapters through the engine via
// the assembler, end to end: it builds a multi-strategy / single-strategy
// engine.Config from an Assembly, runs a short deterministic backtest over an
// in-memory feed, and asserts (a) the run is deterministic and produces
// strategy orders, (b) the portfolio gate ACTUALLY rejects an over-budget /
// over-concentration order (a non-zero num_rejected_orders), and (c) the
// context seam injects per-bar regime / market-cap into the SEPA generators.
//
// These tests are the Go analogue of scripts/multi_strategy_backtest.py's
// end-to-end assertions (it ran SEPA + SectorRotation + Pairs through the
// portfolio gate and reported num_rejected_orders).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepaadapter"
)

func ts(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func bar(sym string, y int, m time.Month, d int, close string, vol int64) domain.Bar {
	p := domain.MustPrice(close)
	return domain.Bar{
		Symbol: sym, TS: ts(y, m, d),
		Open: p, High: p, Low: p, Close: p, Volume: vol,
	}
}

// sectorParams returns a minimal SectorRotation param set: 2 ETFs, top_k=1,
// momentum_lookback=2 (so 3 bars per symbol fully warms it). top_k=1 sizes the
// FULL equity into the winner — used by the gate-rejection test.
func sectorParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"XLK", "XLF"},
		MomentumLookback: 2,
		TopK:             1,
		Timezone:         "America/New_York",
	}
}

// wideSectorParams holds 8 ETFs with top_k=8 (everything held) so each slice is
// ~12.5% NAV — under the lone-sector gate's canonical 50% single-name / 40%
// concentration caps, so the rebalance LONGs are APPROVED. Used by the end-to-end
// submit / determinism test.
func wideSectorParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"},
		MomentumLookback: 2,
		TopK:             8,
		Timezone:         "America/New_York",
	}
}

// wideETFBars builds 3 January warmup bars + a February rollover bar for each
// of the 8 ETFs, each rising so all are eligible top-K entries.
func wideETFBars() []engine.InstrumentBars {
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

// runAssembly builds the engine from an Assembly + feed and runs it.
func runAssembly(t *testing.T, asm *strategyassembly.Assembly, start, end calendar.Date, bal string, instruments []engine.InstrumentBars) *engine.Result {
	t.Helper()
	cfg := engine.Config{
		Tickers:            asm.ExtraTickers,
		Start:              start,
		End:                end,
		StartingBalance:    domain.MustMoney(bal),
		Profile:            engine.ProfileNautilusCompat,
		PrebuiltStrategies: asm.Strategies,
		Portfolio:          asm.Portfolio,
		Context:            asm.Context,
		SPYSymbol:          asm.SPYSymbol,
	}
	feed := engine.SliceFeed{Instruments: instruments}
	eng, err := engine.New(context.Background(), cfg, feed)
	require.NoError(t, err)
	asm.BindEquity(eng)
	res, err := eng.Run(context.Background())
	require.NoError(t, err)
	return res
}

// TestSectorRotationEndToEnd runs the SectorRotation adapter through the engine
// over a month-rollover so a rebalance fires a real LONG, and asserts the run
// is deterministic.
func TestSectorRotationEndToEnd(t *testing.T) {
	build := func() *strategyassembly.Assembly {
		asm, err := strategyassembly.Assemble(strategyassembly.Input{
			Strategy:        "sector_rotation",
			StartingBalance: 100000,
			Params:          strategyassembly.Params{Sector: wideSectorParams()},
		})
		require.NoError(t, err)
		return asm
	}
	asm := build()
	require.Len(t, asm.Strategies, 1)
	require.Len(t, asm.ExtraTickers, 8)

	start := calendar.NewDate(2024, time.January, 1)
	end := calendar.NewDate(2024, time.February, 28)
	res := runAssembly(t, asm, start, end, "100000", wideETFBars())

	// A rebalance fired: 8 LONG orders (all ETFs enter top-8), none rejected
	// (each ~12.5%% NAV is within the default caps).
	require.NotEmpty(t, res.Orders, "sector rebalance should have submitted orders")
	assert.Empty(t, res.RejectedOrders, "within-budget rebalance must not be rejected; rejects=%+v", res.RejectedOrders)
	buys := 0
	for _, o := range res.Orders {
		assert.Equal(t, strategyassembly.IDSector, o.StrategyID)
		if o.Side == domain.OrderSideBuy {
			buys++
		}
	}
	assert.Equal(t, 8, buys, "all 8 ETFs should be bought on the first rebalance; orders=%+v", res.Orders)

	// Determinism: a second identical run yields identical fills/balances.
	res2 := runAssembly(t, build(), start, end, "100000", wideETFBars())
	_ = res2
	assert.Equal(t, res.FinalBalance, res2.FinalBalance)
	require.Equal(t, len(res.Orders), len(res2.Orders))
	for i := range res.Orders {
		assert.Equal(t, res.Orders[i].Symbol, res2.Orders[i].Symbol)
		assert.Equal(t, res.Orders[i].Qty, res2.Orders[i].Qty)
		assert.Equal(t, res.Orders[i].Side, res2.Orders[i].Side)
	}
}

// loneTopK3SectorBars builds 3 SPDR ETFs rising at different rates so a
// rebalance picks a clear top-3 (all three), each sized at ~1/3 of the deployed
// book. With the lone-sector gate (100% budget, canonical 50% single-name cap)
// and budget-aware sizing, each ~33% pick is APPROVED — proving the baseline
// topK=3 default live profile strategy actually trades (FIXER round 2, finding 1).
func loneTopK3SectorBars() []engine.InstrumentBars {
	mk := func(s, c1, c2, c3, c4 string) engine.InstrumentBars {
		return engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			bar(s, 2024, time.January, 2, c1, 1000),
			bar(s, 2024, time.January, 16, c2, 1000),
			bar(s, 2024, time.January, 31, c3, 1000),
			bar(s, 2024, time.February, 1, c4, 1000),
		}}
	}
	return []engine.InstrumentBars{
		mk("XLK", "100.00", "108.00", "115.00", "116.00"), // strongest
		mk("XLF", "100.00", "106.00", "112.00", "113.00"),
		mk("XLE", "100.00", "104.00", "108.00", "109.00"),
	}
}

// TestLoneSectorBaselineTopK3Trades is the direct guard for FIXER round 2 finding
// 1: the baseline topK=3 SectorRotation — the default live/paper profile strategy
// — must produce executable orders. Before the fix it sized each pick at 1/topK
// (33%) of FULL equity, which the lone gate's single-name cap rejected, so the
// out-of-box live experience traded NOTHING. With budget-aware sizing + the
// canonical sector caps it now buys all three top-K ETFs, none rejected.
func TestLoneSectorBaselineTopK3Trades(t *testing.T) {
	// Run at two NAVs to refute the finding's "violation scales with NAV so NO
	// balance ever passes" claim: the per-name fraction (1/topK) is NAV-invariant,
	// so the fix holds at every balance.
	for _, bal := range []string{"100000", "1000000"} {
		t.Run("nav="+bal, func(t *testing.T) {
			asm, err := strategyassembly.Assemble(strategyassembly.Input{
				Strategy:        "sector_rotation",
				StartingBalance: mustFloat(bal),
				Params: strategyassembly.Params{Sector: params.SectorRotationParams{
					Universe:         []string{"XLK", "XLF", "XLE"},
					MomentumLookback: 2,
					TopK:             3,
					Timezone:         "America/New_York",
				}},
			})
			require.NoError(t, err)

			start := calendar.NewDate(2024, time.January, 1)
			end := calendar.NewDate(2024, time.February, 28)
			res := runAssembly(t, asm, start, end, bal, loneTopK3SectorBars())

			require.NotEmpty(t, res.Orders, "baseline topK=3 sector must submit orders (the finding: it traded nothing)")
			assert.Empty(t, res.RejectedOrders, "baseline topK=3 picks (~33%% each) must pass the canonical 50%% single-name cap; rejects=%+v", res.RejectedOrders)
			buys := map[string]bool{}
			for _, o := range res.Orders {
				assert.Equal(t, strategyassembly.IDSector, o.StrategyID)
				if o.Side == domain.OrderSideBuy {
					buys[o.Symbol] = true
				}
			}
			assert.True(t, buys["XLK"] && buys["XLF"] && buys["XLE"],
				"all three top-3 ETFs should be bought; buys=%v orders=%+v", buys, res.Orders)
		})
	}
}

// mustFloat parses a balance string for StartingBalance (float64), matching the
// StartingBalance the runner/API supply.
func mustFloat(s string) float64 { return domain.MustMoney(s).Float64() }

// TestPortfolioGateRejectsOverBudget proves the gate STILL rejects a genuinely
// over-budget order after the budget-aware-sizing fix. With topK=1 the sector
// pick consumes the strategy's ENTIRE 30% multi-strategy slice in a single name;
// when the gate prices it at the rebalance bar's (drifted-up) close it tips just
// over the 30% allocator budget, so the LONG is rejected: no order submitted, one
// RejectedOrder recorded. This is the negative control for finding 1 — the fix
// makes sized picks fit the gate WITHOUT defanging the gate.
func TestPortfolioGateRejectsOverBudget(t *testing.T) {
	// Build the SectorRotation adapter with the MULTI-strategy gate (30% allocator
	// slice; single-name 50% / concentration 40%), reproducing the multi backtest.
	// topK=1 sizes the strategy's whole 30% slice into the single winner, which
	// the allocator budget (a hard 30% ceiling) rejects once priced at the drifted
	// rebalance close.
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "multi",
		StartingBalance: 100000,
		SEPAStocks:      []string{"AAA"}, // a stock with no qualifying signal
		Params: strategyassembly.Params{
			SEPA:   sepaParams(),
			Sector: sectorParams(),
			Pairs:  pairsParams(),
		},
	})
	require.NoError(t, err)

	// Feed only the ETFs (SPY/stock/pair bars optional; absent legs simply never
	// trade). Same month-rollover that triggers the rebalance.
	xlk := engine.InstrumentBars{Symbol: "XLK", Bars: []domain.Bar{
		bar("XLK", 2024, time.January, 2, "100.00", 1000),
		bar("XLK", 2024, time.January, 16, "110.00", 1000),
		bar("XLK", 2024, time.January, 31, "120.00", 1000),
		bar("XLK", 2024, time.February, 1, "121.00", 1000),
	}}
	xlf := engine.InstrumentBars{Symbol: "XLF", Bars: []domain.Bar{
		bar("XLF", 2024, time.January, 2, "100.00", 1000),
		bar("XLF", 2024, time.January, 16, "100.00", 1000),
		bar("XLF", 2024, time.January, 31, "100.00", 1000),
		bar("XLF", 2024, time.February, 1, "100.00", 1000),
	}}

	start := calendar.NewDate(2024, time.January, 1)
	end := calendar.NewDate(2024, time.February, 28)
	res := runAssembly(t, asm, start, end, "100000", []engine.InstrumentBars{xlk, xlf})

	require.NotEmpty(t, res.RejectedOrders, "the over-budget sector LONG must be rejected")
	var sawSectorReject bool
	for _, rj := range res.RejectedOrders {
		if rj.StrategyID == strategyassembly.IDSector && rj.Symbol == "XLK" {
			sawSectorReject = true
			assert.Equal(t, domain.SideLong, rj.SignalSide)
			// allocator budget (30% slice) is checked first; the whole-slice topK=1
			// pick, priced at the drifted rebalance close, trips it. (single-name /
			// concentration accepted too, defensively, in case sizing changes.)
			assert.Contains(t, []string{"allocator.budget_exceeded", "risk.concentration", "risk.max_single_name"}, rj.RuleName)
		}
	}
	assert.True(t, sawSectorReject, "expected a rejected XLK sector LONG; rejects=%+v", res.RejectedOrders)
	// And no XLK order actually went to the book.
	for _, o := range res.Orders {
		assert.NotEqual(t, "XLK", o.Symbol, "rejected order must not be submitted")
	}
}

// TestPortfolioGateApprovesWithinBudget is the positive control: an order whose
// notional is under every cap is APPROVED through the same bridge the engine
// uses (NewProposedOrder + SnapshotFromDomain + Portfolio.Check). This asserts
// the gate's approve branch directly (the end-to-end approve path is covered by
// TestLoneSectorBaselineTopK3Trades / TestSectorRotationEndToEnd).
func TestPortfolioGateApprovesWithinBudget(t *testing.T) {
	alloc, err := portfolio.NewAllocator([]portfolio.StrategyAllocation{{StrategyID: "S", CapitalPct: 1.0}})
	require.NoError(t, err)
	rc, err := portfolio.NewRiskConstraints(portfolio.DefaultRiskConstraintsConfig())
	require.NoError(t, err)
	pf := portfolio.NewPortfolio(alloc, rc)

	snap := domain.NewAccountSnapshot(
		domain.MustMoney("100000"), domain.MustMoney("100000"), 0, 0,
		map[domain.StrategySymbol]domain.Qty{}, map[string]domain.Price{"Z": domain.MustPrice("10")},
	)
	approved := pf.Check(
		portfolio.NewProposedOrder("S", "Z", domain.SideLong, 100, domain.MustPrice("10"), ts(2024, 1, 2)),
		portfolio.SnapshotFromDomain(snap),
	)
	assert.True(t, approved.Approved, "a $1000 (1%% NAV) order must pass the gate: %s %s", approved.RuleName, approved.Reason)
}

// TestSEPAContextInjection proves the look-ahead-safe context seam: the engine
// advances the ContextProvider on the SPY heartbeat bar and injects the regime
// + market cap into the SEPA generator (visible via its StateSummary). Without
// this wiring the SEPA generator would stay at its cold-start "unknown" regime
// and 0 market cap.
func TestSEPAContextInjection(t *testing.T) {
	// 240 steadily-rising SPY closes (bull: above MA200, with a >=31-point MA200
	// so the 30-bar slope is computable and positive).
	const n = 240
	spy := make([]portfolio.SPYBar, 0, n)
	day := ts(2023, time.January, 1)
	for i := 0; i < n; i++ {
		spy = append(spy, portfolio.SPYBar{Date: day, Close: 100.0 + float64(i)})
		day = day.AddDate(0, 0, 1)
	}
	runDate := spy[len(spy)-1].Date // last SPY date == the in-window SPY bar date

	// One SF1 row giving AAA a qualifying market cap, dated BEFORE the run date
	// (look-ahead-safe: a filing dated after the bar would be ignored).
	sf1 := []portfolio.SF1Row{{
		Ticker: "AAA", DateKey: ts(2023, time.February, 1), MarketCap: 2.5e9,
		HasMarketCap: true, Dimension: "MRT", HasDimension: true,
	}}
	provider := portfolio.NewContextProvider(spy, sf1, nil, []string{"AAA"}, "MRT", 0)

	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sepa",
		StartingBalance: 100000,
		SEPAStocks:      []string{"AAA"},
		Params:          strategyassembly.Params{SEPA: sepaParams()},
		Context:         provider,
		SPYSymbol:       "SPY",
	})
	require.NoError(t, err)

	// Engine instruments: SPY heartbeat FIRST (the assembler put it in
	// ExtraTickers), then the stock. Both carry one bar on the run date.
	spyBar := domain.Bar{
		Symbol: "SPY", TS: runDate,
		Open: domain.MustPrice("309.00"), High: domain.MustPrice("309.00"),
		Low: domain.MustPrice("309.00"), Close: domain.MustPrice("309.00"), Volume: 1000,
	}
	feed := engine.SliceFeed{Instruments: []engine.InstrumentBars{
		{Symbol: "SPY", Bars: []domain.Bar{spyBar}},
		{Symbol: "AAA", Bars: []domain.Bar{bar("AAA", runDate.Year(), runDate.Month(), runDate.Day(), "50.00", 1000)}},
	}}

	cfg := engine.Config{
		Tickers:            asm.ExtraTickers, // ["SPY"]; AAA added below
		Start:              calendar.NewDate(runDate.Year(), runDate.Month(), runDate.Day()),
		End:                calendar.NewDate(runDate.Year(), runDate.Month(), runDate.Day()),
		StartingBalance:    domain.MustMoney("100000"),
		Profile:            engine.ProfileNautilusCompat,
		PrebuiltStrategies: asm.Strategies,
		Portfolio:          asm.Portfolio,
		Context:            asm.Context,
		SPYSymbol:          asm.SPYSymbol,
	}
	cfg.Tickers = []string{"SPY", "AAA"} // SPY first for look-ahead-safe context

	eng, err := engine.New(context.Background(), cfg, feed)
	require.NoError(t, err)
	asm.BindEquity(eng)
	_, err = eng.Run(context.Background())
	require.NoError(t, err)

	// The SEPA generator must now reflect the injected bull regime + market cap.
	gen := asm.Strategies[0].(*sepaadapter.Strategy).Generator()
	sm := gen.StateSummary()
	assert.Equal(t, portfolio.RegimeBull, sm.Regime, "SPY heartbeat should classify bull and inject it into SEPA")
	assert.Equal(t, 2.5e9, sm.MarketCapUSD, "SF1 market cap should be injected into SEPA")
}

// sepaParams / pairsParams: minimal valid configs for the multi assembly (they
// need not produce signals for the gate test — only the sector leg does).
func sepaParams() params.SEPAParams {
	return params.SEPAParams{
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000,
		Timezone: "America/New_York",
	}
}

func pairsParams() params.PairsParams {
	return params.PairsParams{
		Pairs:    []params.Pair{{LongLeg: "KO", ShortLeg: "PEP"}},
		Lookback: 5, EntryZ: 2.0, ExitZ: 0.5, CapitalPerPairPct: 0.30,
		Timezone: "America/New_York",
	}
}
