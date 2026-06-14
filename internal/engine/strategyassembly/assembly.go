// Package strategyassembly is the Go port of the Python multi-strategy wiring
// (scripts/multi_strategy_backtest.py + src/runner/strategy_assembly.py): it
// constructs the real strategy adapters (SEPA / SectorRotation / Pairs / ORB)
// from resolved params, the Allocator capital split + RiskConstraints portfolio
// gate, and the per-bar look-ahead-safe context provider, returning everything
// the engine assembler needs as a single Assembly.
//
// It is the ONLY package that imports both the engine seam and every strategy
// adapter, so the per-strategy adapter packages stay decoupled from each other.
// No cycle: adapters import engine; this package imports adapters + engine +
// params + portfolio; engine imports portfolio (not this).
//
// Equity-provider late binding: the strategy SignalGenerators need an
// EquityProvider closure that reads the LIVE account equity, but the account is
// created inside engine.New AFTER the strategies are constructed. We resolve the
// ordering with a LiveEquity holder: generators are built over holder.Get
// (which returns the starting balance until bound), then the caller binds the
// holder to the engine's account via Assembly.BindEquity(eng) before Run. This
// mirrors the Python equity_provider that pulls engine.portfolio.account(VENUE)
// .balance_total at sizing time.
package strategyassembly

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orbadapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairs"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairsadapter"
	sr "github.com/byjackchen/trade-tms-go/internal/strategy/sector_rotation"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectoradapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepaadapter"
)

// Canonical engine strategy ids (the allocator keys; mirror
// strategy_assembly.py:_build_strategies / multi_strategy_backtest.py).
const (
	IDSEPA   = "SEPA-UNIVERSE-001"
	IDSector = "SectorRotation-001"
	IDPairs  = "Pairs-001"
	IDORB    = "IntradayBreakoutRunner-000"
)

// Canonical multi-strategy capital allocation + risk thresholds
// (strategy_assembly.py:_build_portfolio): SEPA 40% / SectorRotation 30% /
// Pairs 20% (10% cash); single-name 50%, concentration 40%, daily-loss-halt
// 10%. These are the multi-strategy weights, NOT the single-strategy defaults.
const (
	allocSEPA   = 0.40
	allocSector = 0.30
	allocPairs  = 0.20

	riskMaxSingleName = 0.50
	riskConcentration = 0.40
	riskDailyLossHalt = 0.10
)

// Canonical SectorRotation risk caps. SectorRotation is a CONCENTRATED rotation:
// it holds 1/topK of its deployed capital in each of topK sector ETFs (33% per
// name at the baseline topK=3). The generic single-strategy default caps
// (single-name 20%, concentration 30%) are structurally incompatible with that
// shape — a 33% pick can never pass a 20% single-name cap regardless of NAV. The
// ONLY risk config the reference defines for SectorRotation is the multi-strategy
// gate (single-name 50% / concentration 40% / daily-loss 10%; see
// scripts/multi_strategy_backtest._build_portfolio), which is sized precisely to
// admit a topK rotation. The lone-strategy SectorRotation path therefore uses
// these SAME canonical caps (NOT the generic 20/30/5 default) so the default live
// profile strategy can actually trade. This does not weaken any parity-critical
// gate: the P4 hyperopt objective path always sets MultiStrategyGate=true and
// already uses these caps; only single-strategy backtest/live is affected.
const (
	sectorMaxSingleName = riskMaxSingleName
	sectorConcentration = riskConcentration
	sectorDailyLossHalt = riskDailyLossHalt
)

// LiveEquity is a late-bound equity source. Before binding, Get returns the
// fallback (the starting balance); after BindEquity it reads the live account.
// Atomic pointer swap keeps it safe even though the engine drives a single
// goroutine (defensive; cheap).
type LiveEquity struct {
	fallback float64
	fn       atomic.Pointer[func() float64]
}

// NewLiveEquity returns a holder that yields fallback until bound.
func NewLiveEquity(fallback float64) *LiveEquity {
	le := &LiveEquity{fallback: fallback}
	return le
}

// Get returns the current equity (the bound source, or the fallback).
func (le *LiveEquity) Get() float64 {
	if p := le.fn.Load(); p != nil {
		return (*p)()
	}
	return le.fallback
}

// bind installs the live source.
func (le *LiveEquity) bind(fn func() float64) { le.fn.Store(&fn) }

// Params bundles the resolved, typed, validated per-strategy params the caller
// pulls from internal/params (db active_params -> param_sets -> file ->
// baseline). Only the params for the selected strategy need be populated.
type Params struct {
	SEPA   params.SEPAParams
	Sector params.SectorRotationParams
	Pairs  params.PairsParams
	ORB    params.IntradayBreakoutParams
}

// Input is the assembly request.
type Input struct {
	// Strategy selects what to build: "sepa" | "sector_rotation" | "pairs" |
	// "orb" | "multi".
	Strategy string
	// StartingBalance seeds the late-bound equity fallback (used until the
	// engine account is bound and for the allocator's pre-run sizing context).
	StartingBalance float64
	// Params carries the resolved per-strategy knobs.
	Params Params
	// SEPAStocks is the SEPA stock universe (one per-symbol generator each). Only
	// used by the "sepa" and "multi" paths.
	SEPAStocks []string
	// ORBSymbol is the single instrument the ORB path trades (intraday).
	ORBSymbol string
	// Context, when non-nil, is the look-ahead-safe per-bar context provider the
	// engine drives on the SPY heartbeat (regime / market-cap / earnings). Only
	// the SEPA / multi paths consume it.
	Context *portfolio.ContextProvider
	// SPYSymbol is the context heartbeat instrument (default "SPY").
	SPYSymbol string
	// MultiStrategyGate, when true, makes a SINGLE-strategy path (sepa /
	// sector_rotation / pairs) install the canonical MULTI-strategy portfolio gate
	// (SEPA 40 / Sector 30 / Pairs 20; single-name 50%, concentration 40%,
	// daily-loss 10%) instead of the lone-strategy 100%/default-caps gate. The
	// selected strategy then receives EXACTLY its canonical multi-strategy capital
	// slice and risk caps.
	//
	// This is the parity contract for the P4 hyperopt objective: Python's
	// scripts/multi_strategy_backtest.run_backtest ALWAYS builds all three runners
	// under _build_portfolio's multi-strategy Allocator+RiskConstraints, even when
	// a hyperopt trial only overrides one sub-strategy's params (the others run on
	// their JSON defaults). The single-strategy 100%-budget gate would admit/reject
	// a DIFFERENT order set, so objectives would diverge. Ignored by the "multi"
	// and "orb" paths (multi already uses this gate; ORB is never in the multi set).
	MultiStrategyGate bool
}

// Assembly is the constructed strategy set + gate + context, ready to plug into
// engine.Config. ExtraTickers are instruments the strategies need beyond the
// caller's primary universe (ETFs, pair legs, the SPY heartbeat) — the caller
// unions them into engine.Config.Tickers, SPY FIRST so its bar dispatches
// before same-date stock bars (look-ahead-safe context).
type Assembly struct {
	Strategies   []engine.Strategy
	Portfolio    *portfolio.Portfolio
	Context      *portfolio.ContextProvider
	SPYSymbol    string
	ExtraTickers []string
	equity       *LiveEquity
}

// BindEquity binds the late equity holder to the engine's live account equity.
// Call AFTER engine.New and BEFORE eng.Run so every generator's sizing reflects
// the running book (mirroring the Python equity_provider reading the venue
// account). Safe to call once.
func (a *Assembly) BindEquity(eng *engine.Engine) {
	if a.equity != nil && eng != nil {
		a.equity.bind(eng.EquityFloat)
	}
}

// Assemble builds the strategy set selected by in.Strategy.
func Assemble(in Input) (*Assembly, error) {
	spy := in.SPYSymbol
	if spy == "" {
		spy = "SPY"
	}
	eq := NewLiveEquity(in.StartingBalance)

	switch in.Strategy {
	case "sepa":
		return assembleSEPA(in, eq, spy, true)
	case "sector_rotation":
		return assembleSector(in, eq)
	case "pairs":
		return assemblePairs(in, eq)
	case "orb":
		return assembleORB(in, eq)
	case "multi":
		return assembleMulti(in, eq, spy)
	default:
		return nil, fmt.Errorf("strategyassembly: unknown strategy %q (want sepa|sector_rotation|pairs|orb|multi)", in.Strategy)
	}
}

// ---------------------------------------------------------------------------
// per-strategy builders
// ---------------------------------------------------------------------------

// buildSEPA constructs one per-symbol SEPA adapter for each stock, all sharing
// the IDSEPA allocator key (the universe-runner model: one logical strategy id,
// N SignalGenerators).
func buildSEPA(p params.SEPAParams, stocks []string, eq *LiveEquity) ([]engine.Strategy, error) {
	if len(stocks) == 0 {
		return nil, fmt.Errorf("strategyassembly: sepa needs at least one stock")
	}
	out := make([]engine.Strategy, 0, len(stocks))
	for _, sym := range stocks {
		gen, err := sepa.NewGenerator(sepa.Config{
			Symbol:                 sym,
			EquityProvider:         eq.Get,
			RiskPct:                p.RiskPct,
			MarketCapMinUSD:        p.MarketCapMinUSD,
			HardStopPct:            p.HardStopPct,
			PivotBufferPct:         p.PivotBufferPct,
			BreakoutVolumeMultiple: p.BreakoutVolumeMultiple,
			VCPLookback:            int(p.VCPLookback),
			HistoryMaxBars:         int(p.HistoryMaxBars),
			Timezone:               p.Timezone,
		})
		if err != nil {
			return nil, fmt.Errorf("strategyassembly: sepa generator %s: %w", sym, err)
		}
		out = append(out, sepaadapter.New(IDSEPA, gen))
	}
	return out, nil
}

func buildSector(p params.SectorRotationParams, eq *LiveEquity) (engine.Strategy, []string, error) {
	gen, err := sr.New(sr.Config{
		EquityProvider:   eq.Get,
		Universe:         p.Universe,
		MomentumLookback: int(p.MomentumLookback),
		TopK:             int(p.TopK),
		Timezone:         p.Timezone,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("strategyassembly: sector generator: %w", err)
	}
	adp, err := sectoradapter.New(IDSector, gen)
	if err != nil {
		return nil, nil, fmt.Errorf("strategyassembly: sector adapter: %w", err)
	}
	universe := append([]string(nil), p.Universe...)
	return adp, universe, nil
}

func buildPairs(p params.PairsParams, eq *LiveEquity) (engine.Strategy, []string, error) {
	prs := make([]pairs.Pair, 0, len(p.Pairs))
	for _, pr := range p.Pairs {
		prs = append(prs, pairs.Pair{LongLeg: pr.LongLeg, ShortLeg: pr.ShortLeg})
	}
	gen, err := pairs.New(pairs.Config{
		EquityProvider:    eq.Get,
		Pairs:             prs,
		Lookback:          int(p.Lookback),
		EntryZ:            p.EntryZ,
		ExitZ:             p.ExitZ,
		CapitalPerPairPct: p.CapitalPerPairPct,
		Timezone:          p.Timezone,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("strategyassembly: pairs generator: %w", err)
	}
	adp, err := pairsadapter.New(IDPairs, gen)
	if err != nil {
		return nil, nil, fmt.Errorf("strategyassembly: pairs adapter: %w", err)
	}
	// Pair legs are the instruments to register (deduped, sorted for stable
	// registration), mirroring multi_strategy_backtest.py's pair_tickers.
	legSet := make(map[string]struct{}, 2*len(prs))
	for _, pr := range prs {
		legSet[pr.LongLeg] = struct{}{}
		legSet[pr.ShortLeg] = struct{}{}
	}
	legs := make([]string, 0, len(legSet))
	for s := range legSet {
		legs = append(legs, s)
	}
	sort.Strings(legs)
	return adp, legs, nil
}

func buildORB(p params.IntradayBreakoutParams, symbol string, eq *LiveEquity) (engine.Strategy, error) {
	if symbol == "" {
		return nil, fmt.Errorf("strategyassembly: orb needs an instrument symbol")
	}
	gen, err := orb.New(orb.Config{
		Symbol:         symbol,
		EquityProvider: eq.Get,
		RiskPct:        p.RiskPct,
		RangeMinutes:   int(p.RangeMinutes),
		VolMultiple:    p.VolMultiple,
		ProfitTargetR:  p.ProfitTargetR,
		HardStopPct:    p.HardStopPct,
		EODExitTime:    p.EODExitTime,
		Timezone:       p.Timezone,
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: orb generator: %w", err)
	}
	adp, err := orbadapter.New(IDORB, gen)
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: orb adapter: %w", err)
	}
	return adp, nil
}

// ---------------------------------------------------------------------------
// strategy-set assemblers
// ---------------------------------------------------------------------------

func assembleSEPA(in Input, eq *LiveEquity, spy string, singleAlloc bool) (*Assembly, error) {
	strats, err := buildSEPA(in.Params.SEPA, in.SEPAStocks, eq)
	if err != nil {
		return nil, err
	}
	// Single-strategy SEPA: the full risk budget (100%) plus default risk caps,
	// so a lone strategy is never starved by the multi-strategy 40% slice — unless
	// MultiStrategyGate is set (P4 objective parity), in which case it receives its
	// canonical multi-strategy slice + caps.
	pf, err := strategyGate(IDSEPA, in.MultiStrategyGate)
	if err != nil {
		return nil, err
	}
	a := &Assembly{
		Strategies: strats, Portfolio: pf, Context: in.Context, SPYSymbol: spy, equity: eq,
	}
	if in.Context != nil {
		a.ExtraTickers = []string{spy} // SPY heartbeat for context refresh
	}
	return a, nil
}

func assembleSector(in Input, eq *LiveEquity) (*Assembly, error) {
	adp, universe, err := buildSector(in.Params.Sector, eq)
	if err != nil {
		return nil, err
	}
	pf, err := strategyGate(IDSector, in.MultiStrategyGate)
	if err != nil {
		return nil, err
	}
	return &Assembly{
		Strategies: []engine.Strategy{adp}, Portfolio: pf,
		ExtraTickers: universe, equity: eq,
	}, nil
}

func assemblePairs(in Input, eq *LiveEquity) (*Assembly, error) {
	adp, legs, err := buildPairs(in.Params.Pairs, eq)
	if err != nil {
		return nil, err
	}
	pf, err := strategyGate(IDPairs, in.MultiStrategyGate)
	if err != nil {
		return nil, err
	}
	return &Assembly{
		Strategies: []engine.Strategy{adp}, Portfolio: pf,
		ExtraTickers: legs, equity: eq,
	}, nil
}

func assembleORB(in Input, eq *LiveEquity) (*Assembly, error) {
	adp, err := buildORB(in.Params.ORB, in.ORBSymbol, eq)
	if err != nil {
		return nil, err
	}
	pf, err := singleStrategyPortfolio(IDORB)
	if err != nil {
		return nil, err
	}
	return &Assembly{
		Strategies: []engine.Strategy{adp}, Portfolio: pf,
		ExtraTickers: []string{in.ORBSymbol}, equity: eq,
	}, nil
}

// assembleMulti builds the canonical 3-daily-strategy set (SEPA + Sector +
// Pairs) with the multi-strategy Allocator split and risk constraints — the Go
// port of scripts/multi_strategy_backtest.py + strategy_assembly.py. ORB is
// intraday and intentionally NOT part of the multi set (runs as its own
// single-strategy backtest path).
func assembleMulti(in Input, eq *LiveEquity, spy string) (*Assembly, error) {
	sepaStrats, err := buildSEPA(in.Params.SEPA, in.SEPAStocks, eq)
	if err != nil {
		return nil, err
	}
	sectorAdp, universe, err := buildSector(in.Params.Sector, eq)
	if err != nil {
		return nil, err
	}
	pairsAdp, legs, err := buildPairs(in.Params.Pairs, eq)
	if err != nil {
		return nil, err
	}

	strats := make([]engine.Strategy, 0, len(sepaStrats)+2)
	strats = append(strats, sepaStrats...)
	strats = append(strats, sectorAdp, pairsAdp)

	pf, err := multiStrategyPortfolio()
	if err != nil {
		return nil, err
	}

	// Extra instruments: SPY heartbeat FIRST (context look-ahead safety), then
	// the sector ETFs and pair legs. Deduped, preserving the SPY-first order.
	extra := dedupKeepOrder(append([]string{spy}, append(universe, legs...)...))

	return &Assembly{
		Strategies: strats, Portfolio: pf, Context: in.Context, SPYSymbol: spy,
		ExtraTickers: extra, equity: eq,
	}, nil
}

// ---------------------------------------------------------------------------
// portfolio builders
// ---------------------------------------------------------------------------

// multiStrategyPortfolio builds the canonical multi-strategy gate
// (strategy_assembly.py:_build_portfolio): SEPA 40 / Sector 30 / Pairs 20,
// single-name 50%, concentration 40%, daily-loss-halt 10%.
func multiStrategyPortfolio() (*portfolio.Portfolio, error) {
	alloc, err := portfolio.NewAllocator([]portfolio.StrategyAllocation{
		{StrategyID: IDSEPA, CapitalPct: allocSEPA},
		{StrategyID: IDSector, CapitalPct: allocSector},
		{StrategyID: IDPairs, CapitalPct: allocPairs},
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: allocator: %w", err)
	}
	rc, err := portfolio.NewRiskConstraints(portfolio.RiskConstraintsConfig{
		MaxSingleNamePct: riskMaxSingleName,
		ConcentrationPct: riskConcentration,
		DailyLossHaltPct: riskDailyLossHalt,
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: risk constraints: %w", err)
	}
	return portfolio.NewPortfolio(alloc, rc), nil
}

// strategyGate selects the portfolio gate for a single-strategy path: the
// canonical multi-strategy gate (when multiGate is set — the P4 hyperopt
// objective-parity contract, mirroring multi_strategy_backtest.run_backtest
// which always builds all three runners under the multi-strategy
// Allocator+RiskConstraints) or the lone-strategy 100%-budget gate otherwise
// (the default single-strategy backtest path). When multiGate is set, the
// strategy id MUST be one of the three daily multi-strategy ids so the allocator
// registers a budget for it; ORB has no multi slice and always uses the
// single-strategy gate.
func strategyGate(id string, multiGate bool) (*portfolio.Portfolio, error) {
	if multiGate && (id == IDSEPA || id == IDSector || id == IDPairs) {
		return multiStrategyPortfolio()
	}
	// Lone SectorRotation needs its canonical caps (50/40/10), NOT the generic
	// 20/30/5 default — a topK rotation holds 1/topK (33% at topK=3) per name,
	// which the 20% single-name default would reject outright. The reference only
	// ever runs SectorRotation under these caps (FIXER round 2, finding 1). Other
	// lone strategies keep the default gate (their P4 parity contract).
	if id == IDSector {
		return loneSectorPortfolio()
	}
	return singleStrategyPortfolio(id)
}

// loneSectorPortfolio builds the lone-strategy gate for SectorRotation: the whole
// book (100% budget) under SectorRotation's canonical risk caps (single-name 50%,
// concentration 40%, daily-loss 10%) — the only caps the reference defines for a
// topK rotation. Paired with full-equity sizing (budget 100% -> unscaled), each
// pick is 1/topK (33% at topK=3), within the 50% single-name cap.
func loneSectorPortfolio() (*portfolio.Portfolio, error) {
	alloc, err := portfolio.NewAllocator([]portfolio.StrategyAllocation{
		{StrategyID: IDSector, CapitalPct: 1.0},
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: allocator: %w", err)
	}
	rc, err := portfolio.NewRiskConstraints(portfolio.RiskConstraintsConfig{
		MaxSingleNamePct: sectorMaxSingleName,
		ConcentrationPct: sectorConcentration,
		DailyLossHaltPct: sectorDailyLossHalt,
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: risk constraints: %w", err)
	}
	return portfolio.NewPortfolio(alloc, rc), nil
}

// singleStrategyPortfolio builds a gate for a lone strategy: the whole budget
// (100%) to that id, with the reference DEFAULT risk caps (single-name 20%,
// concentration 30%, daily-loss-halt 5%). This still enforces the aggregate
// risk rules a single-strategy backtest must respect, while not under-funding
// the only strategy present.
func singleStrategyPortfolio(id string) (*portfolio.Portfolio, error) {
	alloc, err := portfolio.NewAllocator([]portfolio.StrategyAllocation{
		{StrategyID: id, CapitalPct: 1.0},
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: allocator: %w", err)
	}
	rc, err := portfolio.NewRiskConstraints(portfolio.DefaultRiskConstraintsConfig())
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: risk constraints: %w", err)
	}
	return portfolio.NewPortfolio(alloc, rc), nil
}

func dedupKeepOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
