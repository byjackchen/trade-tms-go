package strategyassembly

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orbadapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairs"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairsadapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectoradapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectorrotation"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepaadapter"
)

// Canonical engine strategy ids (the allocator keys).
const (
	IDSEPA   = "SEPA-UNIVERSE-001"
	IDSector = "SectorRotation-001"
	IDPairs  = "Pairs-001"
	IDORB    = "IntradayBreakoutRunner-000"
)

// engineID maps a LOGICAL composition strategy id (the values a
// composition.Member carries) to its canonical ENGINE id (the allocator key). The
// weights and risk that used to be hardcoded constants here are now DATA carried
// by the Composition (docs/concept-alignment.md §1.2, §3.2).
func engineID(strategyID string) (string, error) {
	switch strategyID {
	case composition.StrategySEPA:
		return IDSEPA, nil
	case composition.StrategySectorRotation:
		return IDSector, nil
	case composition.StrategyPairs:
		return IDPairs, nil
	case composition.StrategyIntradayBreakout:
		return IDORB, nil
	default:
		return "", fmt.Errorf("strategyassembly: unknown strategy id %q", strategyID)
	}
}

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
	// Composition is the portfolio blueprint that drives assembly: its ACTIVE
	// members select which strategies to build (logical id -> engine id) and seed
	// the allocator budgets (CapitalPct = member.Weight), and composition.Risk seeds
	// the risk constraints. This replaces the old "multi"/"sepa"/… strategy switch
	// and the hardcoded weight/risk constants — risk is now DATA, not code branches.
	Composition composition.Composition
	// StartingBalance seeds the late-bound equity fallback (used until the
	// engine account is bound and for the allocator's pre-run sizing context).
	StartingBalance float64
	// Params carries the resolved per-strategy knobs.
	Params Params
	// SEPAStocks is the SEPA stock universe (one per-symbol generator each). Only
	// used when the Composition has a SEPA member.
	SEPAStocks []string
	// ORBSymbol is the single instrument the ORB path trades (intraday). Only used
	// when the Composition has an intraday_breakout member.
	ORBSymbol string
	// Context, when non-nil, is the look-ahead-safe per-bar context provider the
	// engine drives on the SPY heartbeat (regime / market-cap / earnings). Only
	// consumed when the Composition has a SEPA member.
	Context *riskgate.ContextProvider
	// SPYSymbol is the context heartbeat instrument (default "SPY").
	SPYSymbol string
}

// Assembly is the constructed strategy set + gate + context, ready to plug into
// engine.Config. ExtraTickers are instruments the strategies need beyond the
// caller's primary universe (ETFs, pair legs, the SPY heartbeat) — the caller
// unions them into engine.Config.Tickers, SPY FIRST so its bar dispatches
// before same-date stock bars (look-ahead-safe context).
type Assembly struct {
	Strategies   []engine.Strategy
	Gate         *riskgate.Gate
	Context      *riskgate.ContextProvider
	SPYSymbol    string
	ExtraTickers []string
	equity       *LiveEquity
}

// BindEquity binds the late equity holder to the engine's live account equity.
// Call AFTER engine.New and BEFORE eng.Run so every generator's sizing reflects
// the running book (the equity provider reads the venue account). Safe to call
// once.
func (a *Assembly) BindEquity(eng *engine.Engine) {
	if a.equity != nil && eng != nil {
		a.equity.bind(eng.EquityFloat)
	}
}

// Assemble builds the strategy set the Composition describes: it wires a
// generator for each ACTIVE member, an allocator from the member weights (logical
// id -> engine id, CapitalPct = member.Weight) and risk constraints from the
// Composition risk. This is the single Composition-driven assembler that replaced
// the old per-strategy switch + hardcoded weight/risk constants
// (docs/concept-alignment.md §3.2).
func Assemble(in Input) (*Assembly, error) {
	return assembleFromComposition(in)
}

// assembleFromComposition is the crux of the Composition-driven assembly: from
// in.Composition it builds the riskgate.Allocator (one StrategyAllocation per
// ACTIVE member, keyed by the member's ENGINE id with CapitalPct = member.Weight)
// and the riskgate.RiskConstraints (from composition.Risk), then wires the SEPA /
// Sector / Pairs / ORB generators for whichever members are present (reusing the
// buildSEPA / buildSector / buildPairs / buildORB helpers). ExtraTickers union
// the SPY heartbeat (FIRST, when a context provider is configured), then the
// sector universe and pair legs, deduped SPY-first (look-ahead-safe context).
func assembleFromComposition(in Input) (*Assembly, error) {
	if err := in.Composition.Validate(); err != nil {
		return nil, fmt.Errorf("strategyassembly: %w", err)
	}
	spy := in.SPYSymbol
	if spy == "" {
		spy = "SPY"
	}
	eq := NewLiveEquity(in.StartingBalance)

	var (
		strats   []engine.Strategy
		allocs   []riskgate.StrategyAllocation
		universe []string // sector ETF universe (if a sector member is present)
		legs     []string // pair legs (if a pairs member is present)
	)

	for _, mem := range in.Composition.Members {
		if !mem.Active {
			continue
		}
		id, err := engineID(mem.StrategyID)
		if err != nil {
			return nil, err
		}
		allocs = append(allocs, riskgate.StrategyAllocation{StrategyID: id, CapitalPct: mem.Weight})

		switch mem.StrategyID {
		case composition.StrategySEPA:
			sepaStrats, err := buildSEPA(in.Params.SEPA, in.SEPAStocks, eq)
			if err != nil {
				return nil, err
			}
			strats = append(strats, sepaStrats...)
		case composition.StrategySectorRotation:
			adp, uni, err := buildSector(in.Params.Sector, eq)
			if err != nil {
				return nil, err
			}
			strats = append(strats, adp)
			universe = uni
		case composition.StrategyPairs:
			adp, lg, err := buildPairs(in.Params.Pairs, eq)
			if err != nil {
				return nil, err
			}
			strats = append(strats, adp)
			legs = lg
		case composition.StrategyIntradayBreakout:
			adp, err := buildORB(in.Params.ORB, in.ORBSymbol, eq)
			if err != nil {
				return nil, err
			}
			strats = append(strats, adp)
			legs = append(legs, in.ORBSymbol)
		}
	}

	gate, err := buildGate(allocs, in.Composition.Risk)
	if err != nil {
		return nil, err
	}

	// Extra instruments: SPY heartbeat FIRST (context look-ahead safety) when a
	// context provider is configured — only the SEPA path consumes context — then
	// the sector ETFs and pair legs, deduped preserving the SPY-first order.
	var extra []string
	if in.Context != nil {
		extra = append(extra, spy)
	}
	extra = append(extra, universe...)
	extra = append(extra, legs...)

	a := &Assembly{
		Strategies:   strats,
		Gate:         gate,
		Context:      in.Context,
		SPYSymbol:    spy,
		ExtraTickers: dedupKeepOrder(extra),
		equity:       eq,
	}
	return a, nil
}

// buildGate builds the portfolio gate from the Composition-derived allocations
// and risk: the allocator budgets (one per active member) and the risk
// constraints (the Composition's SingleNamePct / ConcentrationPct /
// DailyLossHaltPct).
func buildGate(allocs []riskgate.StrategyAllocation, risk composition.Risk) (*riskgate.Gate, error) {
	alloc, err := riskgate.NewAllocator(allocs)
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: allocator: %w", err)
	}
	rc, err := riskgate.NewRiskConstraints(riskgate.RiskConstraintsConfig{
		MaxSingleNamePct: risk.SingleNamePct,
		ConcentrationPct: risk.ConcentrationPct,
		DailyLossHaltPct: risk.DailyLossHaltPct,
	})
	if err != nil {
		return nil, fmt.Errorf("strategyassembly: risk constraints: %w", err)
	}
	return riskgate.NewGate(alloc, rc), nil
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
		gen, err := sepa.New(sepa.Config{
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
		adapter, err := sepaadapter.New(IDSEPA, gen)
		if err != nil {
			return nil, fmt.Errorf("strategyassembly: sepa adapter %s: %w", sym, err)
		}
		out = append(out, adapter)
	}
	return out, nil
}

func buildSector(p params.SectorRotationParams, eq *LiveEquity) (engine.Strategy, []string, error) {
	gen, err := sectorrotation.New(sectorrotation.Config{
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
	// registration).
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
