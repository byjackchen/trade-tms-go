package runner

// assembly.go resolves a live/EOD strategy set from the DB exactly as the
// backtest handler does, so the live path and the EOD path reuse the SAME
// strategy / portfolio / context / warmup code as backtest (P5 decision 3).
//
// It is a thin orchestration over internal/params (param resolution),
// internal/engine/strategyassembly (adapter + gate + context construction) and
// internal/data/universe (bars / market caps / SPY / warmup-tail loading) —
// the byte-for-byte same inputs a backtest receives. The ONLY difference from
// backtest is the consumer: instead of engine.New + eng.Run, the caller drives
// the assembled strategies through a livengine.Session (streaming for live,
// Replay for EOD) with a NoopExecutor (records intents, places no orders).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/model"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/params/paramsdb"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

// sepaWarmupCalendarDays mirrors the backtest handler: 400 calendar days of
// out-of-band SEPA warmup before the window start.
const sepaWarmupCalendarDays = 400

// tradingDaysToCalendarDays converts a count of TRADING bars (the lookback params
// are expressed in trading days) into the calendar-day horizon needed to load at
// least that many daily bars, padded for weekends/holidays (~252 trading days a
// year => ~365/252 ≈ 1.45 calendar days per trading day) plus a safety margin.
func tradingDaysToCalendarDays(tradingDays int) int {
	if tradingDays <= 0 {
		return 0
	}
	return int(math.Ceil(float64(tradingDays)*1.55)) + 10
}

// sectorWarmupCalendarDays sizes the LIVE batch-warmup horizon for SectorRotation:
// enough daily history that (a) every universe ETF has > momentum_lookback closes
// AND (b) at least one MONTH-rollover rebalance has fired before session start, so
// the momentum ranking + currentPositions are fully formed (not all no_setup). The
// rebalance cadence is monthly, so we add ~2 extra months (~63 trading days) on top
// of the lookback window to guarantee a formed ranking spanning a rebalance.
func sectorWarmupCalendarDays(momentumLookback int) int {
	return tradingDaysToCalendarDays(momentumLookback + 1 + 63)
}

// pairsWarmupCalendarDays sizes the LIVE batch-warmup horizon for Pairs: enough
// daily history that each leg has >= lookback closes so the OLS/z-score spread is
// formed at session start (a few extra bars let the z-score state machine settle).
func pairsWarmupCalendarDays(lookback int) int {
	return tradingDaysToCalendarDays(lookback + 20)
}

// AssemblyInput selects what live/EOD strategy set to build.
type AssemblyInput struct {
	// Strategy is "sepa" | "sector_rotation" | "pairs" | "orb" | "multi".
	Strategy string
	// Tickers is the SEPA stock universe (SEPA / multi); the other strategies
	// derive their instruments (ETFs / pair legs / SPY) from params.
	Tickers []string
	// ORBSymbol is the single intraday instrument (ORB path).
	ORBSymbol string
	// StartingBalance seeds the informational health NAV + equity fallback (USD).
	StartingBalance float64
	// SubscriptionCap is the LIVE-ONLY moomoo OpenD per-connection subscription
	// quota (MoomooMaxSub, default 100) the assembled instrument set must fit. 0
	// means NO cap — the FULL survivor-bias-free universe is assembled (the
	// backtest / hyperopt / EOD path MUST pass 0 so capping never reintroduces
	// survivorship bias). When > 0 and the strategy carries a SEPA leg (sepa /
	// multi), the fixed baskets (SPY + sector ETFs + pair legs) are ALWAYS kept and
	// the SEPA stock universe is truncated to the top-N names BY MARKET CAP via the
	// SHARED universe.ResolveLiveSubscriptionSet (the same sizing the
	// SUBSCRIPTION_CAP preflight uses), so the total distinct subscription set fits
	// the quota minus universe.SubscriptionSafetyMargin.
	SubscriptionCap int
	// UniverseLimit is the LIVE-ONLY top-N SEPA cap (TMS_LIVE_UNIVERSE_LIMIT,
	// default 85), an ADDITIONAL deterministic clamp on the SEPA stock count above
	// and beyond the OpenD-quota fit: the SEPA universe is held to the top
	// UniverseLimit names by market cap even when the quota would admit more. 0
	// (or backtest/hyperopt/EOD) applies no SEPA-count clamp. Only meaningful when
	// SubscriptionCap > 0 (the live path).
	UniverseLimit int
}

// Assembled is the resolved live/EOD strategy set plus the instrument universe
// and the warmup provider, ready to feed a livengine.Session.
type Assembled struct {
	// Assembly is the strategy adapters + portfolio gate + context provider.
	Assembly *strategyassembly.Assembly
	// Tickers is the full instrument universe to load bars for (SPY first, then
	// stocks / ETFs / pair legs), deduped.
	Tickers []string
	// Warmup is the out-of-band SEPA warmup provider (nil when no warmup).
	Warmup livengine.WarmupProvider
	// WarmupSymbols are the per-symbol prime symbols (the SEPA stock universe) fed
	// to WarmupConsumer strategies.
	WarmupSymbols []string
	// WarmupBatchSymbols are the symbols whose INTERLEAVED pre-window history primes
	// the multi-symbol BatchWarmupConsumer strategies (SectorRotation ETFs + SPY,
	// Pairs legs). Empty for a pure-SEPA session. The LIVE path turns these into an
	// interleaved bar stream (BuildWarmupBatch) — the EOD path leaves them unused
	// because its replay already covers the full [as_of-window, as_of] in-band.
	WarmupBatchSymbols []string
	// WarmupCalendarDays is the per-session warmup horizon (max of the per-strategy
	// lookbacks resolved below): how many calendar days of pre-window history the
	// LIVE batch warmup must load so every strategy's rolling state is fully formed
	// at session start. 0 when no batch warmup is needed.
	WarmupCalendarDays int
	// SPYSymbol is the context heartbeat instrument.
	SPYSymbol string
}

// Assembler builds live/EOD strategy sets from DB-resolved params + bars.
type Assembler struct {
	uni    *universe.Store
	loader *params.Loader
}

// NewAssembler builds an Assembler over the pool. paramsDir is the strategy
// params override directory (config.StrategyParamsDir).
func NewAssembler(pool *pgxpool.Pool, paramsDir string) *Assembler {
	return &Assembler{
		uni:    universe.NewStore(pool),
		loader: params.NewLoader(paramsdb.NewReader(pool), paramsDir),
	}
}

// Universe returns the underlying universe store (bar loading for the window).
func (a *Assembler) Universe() *universe.Store { return a.uni }

// Assemble resolves the strategy params, builds the adapters / gate / context,
// and prepares the warmup provider for the window [start, end]. It mirrors the
// backtest handler's assembleRealStrategy + buildContext + buildWarmup so the
// live/EOD strategy state coincides with a backtest's.
func (a *Assembler) Assemble(ctx context.Context, in AssemblyInput, start, end calendar.Date) (*Assembled, error) {
	switch in.Strategy {
	case "sepa", "sector_rotation", "pairs", "orb", "multi":
	default:
		return nil, fmt.Errorf("runner: unsupported strategy %q (want sepa|sector_rotation|pairs|orb|multi)", in.Strategy)
	}

	// SEPA-bearing strategies (sepa / multi) need a non-empty stock universe or
	// strategyassembly hard-fails ("sepa needs at least one stock"). When the
	// caller passes no explicit --tickers, fall back to the survivor-bias-free
	// SF1 (common-stock) universe tradable over the assembly window — the same
	// source a backtest's universe.{start,end,table=SF1} resolves against — so a
	// node/refresh started with a bare `--strategy multi` runs a real session
	// instead of crash-looping. An explicit Tickers list always wins.
	if (in.Strategy == "sepa" || in.Strategy == "multi") && len(in.Tickers) == 0 {
		def, derr := a.uni.ListUniverseForWindow(ctx, start, end, universe.TableSF1)
		if derr != nil {
			return nil, fmt.Errorf("runner: resolving default SEPA universe [%s, %s]: %w", start, end, derr)
		}
		if len(def) == 0 {
			return nil, fmt.Errorf("runner: strategy %q needs a stock universe but none was supplied and the default SF1 universe for [%s, %s] is empty (load bars / pass --tickers)", in.Strategy, start, end)
		}
		in.Tickers = def
	}

	spy := "SPY"

	// LIVE-ONLY market-cap cap (P5 universe-limit). SubscriptionCap > 0 caps the
	// TOTAL distinct subscription set to fit the moomoo OpenD per-connection quota;
	// 0 (backtest / hyperopt / EOD) keeps the FULL survivor-bias-free universe. The
	// fixed baskets (SPY + sector ETFs + pair legs) are ALWAYS subscribed, so the
	// SEPA stock universe is truncated to the top-N names BY MARKET CAP (via the
	// SHARED universe.ResolveLiveSubscriptionSet — the same sizing the
	// SUBSCRIPTION_CAP preflight uses) BEFORE building the context / warmup, so the
	// whole live pipeline only touches the capped set. The reserved fixed baskets
	// are resolved from the SAME promoted params the session will subscribe (NOT the
	// hardcoded defaults), so the cap reserves exactly the slots the live subscribe
	// consumes and can never under-reserve when a promoted sector/pairs param_set
	// expands the baskets (preflight/live cap parity).
	if in.SubscriptionCap > 0 && (in.Strategy == "sepa" || in.Strategy == "multi") && len(in.Tickers) > 0 {
		fixed, err := a.resolveFixedBaskets(ctx, in.Strategy, spy)
		if err != nil {
			return nil, err
		}
		capped, err := a.capSEPAUniverse(ctx, fixed, in.Tickers, in.SubscriptionCap, in.UniverseLimit)
		if err != nil {
			return nil, err
		}
		in.Tickers = capped
	}

	// Resolve the SEED Model the legacy strategy selector maps to (multi ->
	// default-multi; the singles -> their *-only single-member Model). The
	// assembler is Model-driven: weights + risk come from this Model, not
	// hardcoded constants (docs/concept-alignment.md §3.2).
	mdl, err := seedModelForStrategy(in.Strategy)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	asmIn := strategyassembly.Input{
		Model:           mdl,
		StartingBalance: in.StartingBalance,
		SEPAStocks:      in.Tickers,
		ORBSymbol:       in.ORBSymbol,
		SPYSymbol:       spy,
	}

	var (
		warmup        livengine.WarmupProvider
		warmupSymbols []string
		// batchDays is the LIVE batch-warmup horizon (max per-strategy lookback in
		// calendar days). The interleaved pre-window history that primes the
		// multi-symbol BatchWarmupConsumer strategies (sector / pairs) is loaded over
		// it by the live path; 0 means no batch warmup (sepa-only / orb).
		batchDays int
	)
	switch in.Strategy {
	case "sepa":
		if asmIn.Params.SEPA, _, err = a.loader.SEPA(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve sepa params: %w", err)
		}
		if asmIn.Context, err = a.buildContext(ctx, start, end, in.Tickers); err != nil {
			return nil, err
		}
		if warmup, warmupSymbols, err = a.buildWarmup(ctx, start, in.Tickers); err != nil {
			return nil, err
		}
	case "sector_rotation":
		if asmIn.Params.Sector, _, err = a.loader.SectorRotation(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve sector params: %w", err)
		}
		batchDays = sectorWarmupCalendarDays(int(asmIn.Params.Sector.MomentumLookback))
	case "pairs":
		if asmIn.Params.Pairs, _, err = a.loader.Pairs(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve pairs params: %w", err)
		}
		batchDays = pairsWarmupCalendarDays(int(asmIn.Params.Pairs.Lookback))
	case "orb":
		if asmIn.ORBSymbol == "" {
			if len(in.Tickers) == 1 {
				asmIn.ORBSymbol = in.Tickers[0]
			} else {
				return nil, errors.New("runner: orb strategy requires an orb symbol (or exactly one ticker)")
			}
		}
		if asmIn.Params.ORB, _, err = a.loader.IntradayBreakout(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve orb params: %w", err)
		}
	case "multi":
		if asmIn.Params.SEPA, _, err = a.loader.SEPA(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve sepa params: %w", err)
		}
		if asmIn.Params.Sector, _, err = a.loader.SectorRotation(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve sector params: %w", err)
		}
		if asmIn.Params.Pairs, _, err = a.loader.Pairs(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve pairs params: %w", err)
		}
		if asmIn.Context, err = a.buildContext(ctx, start, end, in.Tickers); err != nil {
			return nil, err
		}
		if warmup, warmupSymbols, err = a.buildWarmup(ctx, start, in.Tickers); err != nil {
			return nil, err
		}
		// The multi set's batch horizon is the MAX of the sector + pairs lookbacks
		// (each multi-symbol generator is primed over the same interleaved stream;
		// the longer-lookback strategy dictates how far back the stream must reach).
		batchDays = max(
			sectorWarmupCalendarDays(int(asmIn.Params.Sector.MomentumLookback)),
			pairsWarmupCalendarDays(int(asmIn.Params.Pairs.Lookback)),
		)
	}

	asm, err := strategyassembly.Assemble(asmIn)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	// BindEquity to the starting-balance fallback: signal mode has no live
	// account (the NoopExecutor places no orders, so the engine account never
	// moves). The fallback equity is the informational NAV — exactly what the
	// reference signal path sizes against (no real book).
	tickers := unionTickers(asm.ExtraTickers, in.Tickers)
	// The multi-symbol BatchWarmupConsumer strategies (sector / pairs) prime from
	// the interleaved pre-window history of THEIR instruments — the assembled
	// ExtraTickers (sector universe / pair legs; SPY heartbeat is harmlessly
	// included and self-filtered by the generators). Only populated when a
	// batch-warmup strategy is present (batchDays > 0).
	var batchSyms []string
	if batchDays > 0 {
		batchSyms = append([]string(nil), asm.ExtraTickers...)
	}
	return &Assembled{
		Assembly:           asm,
		Tickers:            tickers,
		Warmup:             warmup,
		WarmupSymbols:      warmupSymbols,
		WarmupBatchSymbols: batchSyms,
		WarmupCalendarDays: batchDays,
		SPYSymbol:          asm.SPYSymbol,
	}, nil
}

// seedModelForStrategy maps a legacy strategy selector
// (sepa|sector_rotation|pairs|orb|multi) to its backward-compatible SEED Model
// (multi -> default-multi; the singles -> their *-only single-member Model),
// resolved in-process from model.SeedModels (no DB pool). The Model carries the
// weights + risk the assembler used to hardcode (docs/concept-alignment.md §3.2).
func seedModelForStrategy(strategy string) (model.Model, error) {
	id := map[string]string{
		"multi":           "default-multi",
		"sepa":            "sepa-only",
		"sector_rotation": "sector-only",
		"pairs":           "pairs-only",
		"orb":             "orb-only",
	}[strategy]
	if id == "" {
		return model.Model{}, fmt.Errorf("no seed model for strategy %q", strategy)
	}
	return model.Seed(id)
}

// resolveFixedBaskets returns the always-subscribed instruments the live cap MUST
// reserve slots for — resolved from the SAME promoted params the session will
// actually subscribe, NOT the hardcoded universe.SectorETFTickers / PairLegTickers
// defaults. This is the single-source-of-truth fix for the preflight/live cap
// divergence: the live session's subscription set is built from the params-resolved
// sector universe (strategyassembly.buildSector uses p.Universe) and pair legs
// (buildPairs uses p.Pairs), and the SUBSCRIPTION_CAP preflight likewise sizes
// against the params-resolved baskets (probes.go resolveSector/resolvePairs). If
// the cap reserved against the smaller hardcoded defaults while a promoted
// sector/pairs param_set expanded the baskets, the cap would UNDER-reserve and
// admit more SEPA than there is room for — a green preflight but an over-cap live
// subscribe (the exact crash-loop the fix prevents). Resolving the baskets the same
// way here keeps the fixed half identical across preflight and live.
//
// spy is the heartbeat instrument (always reserved for sepa and multi). For multi,
// the sector ETF universe (p.Universe) and pair legs (from p.Pairs) are added; for
// pure sepa there is no fixed basket beyond SPY. The set is deduped + sorted by
// ResolveLiveSubscriptionSet downstream.
func (a *Assembler) resolveFixedBaskets(ctx context.Context, strategy, spy string) ([]string, error) {
	fixed := []string{spy}
	if strategy != "multi" {
		return fixed, nil // sepa-only: SPY heartbeat is the only fixed instrument.
	}
	sp, _, err := a.loader.SectorRotation(ctx)
	if err != nil {
		return nil, fmt.Errorf("runner: resolve sector params for live cap: %w", err)
	}
	fixed = append(fixed, sp.Universe...)
	pp, _, err := a.loader.Pairs(ctx)
	if err != nil {
		return nil, fmt.Errorf("runner: resolve pairs params for live cap: %w", err)
	}
	for _, pr := range pp.Pairs {
		fixed = append(fixed, pr.LongLeg, pr.ShortLeg)
	}
	return fixed, nil // ResolveLiveSubscriptionSet dedupes + sorts.
}

// capSEPAUniverse truncates the SEPA stock universe to the top-N names BY MARKET
// CAP so the TOTAL distinct subscription set (SEPA + the always-included fixed
// baskets, passed by the caller from the params-resolved baskets the session will
// subscribe) fits the moomoo OpenD per-connection quota. It delegates the
// quota-fit sizing to the SHARED universe.ResolveLiveSubscriptionSet — the SAME
// single source of truth the SUBSCRIPTION_CAP preflight uses — so preflight and
// the live assembly admit EXACTLY the same names. After the quota-fit, the env
// top-N SEPA limit (TMS_LIVE_UNIVERSE_LIMIT, passed as envLimit; <= 0 = no extra
// clamp) is applied as a further deterministic top-by-cap truncation, so the SEPA
// count never exceeds the operator's configured limit even when the quota would
// admit more.
func (a *Assembler) capSEPAUniverse(ctx context.Context, fixed, sepa []string, openDCap, envLimit int) ([]string, error) {
	caps, err := a.uni.MarketCaps(ctx, sepa)
	if err != nil {
		return nil, fmt.Errorf("runner: loading market caps for live universe cap: %w", err)
	}
	lookup := func(t string) float64 { return caps[t] }
	set := universe.ResolveLiveSubscriptionSet(fixed, sepa, lookup, openDCap)
	capped := set.SEPA
	// Additional env top-N clamp (TMS_LIVE_UNIVERSE_LIMIT). set.SEPA is already
	// top-by-cap, so re-applying ApplyUniverseLimit with the smaller envLimit just
	// trims the lowest-cap tail (still deterministic, still top-by-cap).
	if envLimit > 0 && len(capped) > envLimit {
		capped = universe.ApplyUniverseLimit(capped, lookup, envLimit)
	}
	return capped, nil
}

// LoadWindowBars loads the dispatch-ordered run-window bars for the assembled
// universe over [start, end], wrangled to domain bars and interleaved by
// timestamp with the SPY heartbeat FIRST within each timestamp (look-ahead-safe
// context) — the exact ordering livengine.BatchBars produces and the engine
// seed expects. This is the bar stream the EOD Replay (and the test-driven live
// path) consumes.
func (a *Assembler) LoadWindowBars(ctx context.Context, as *Assembled, start, end calendar.Date) ([]domain.Bar, error) {
	// Register SPY first (if present in Tickers) so its bar dispatches before
	// same-timestamp stock bars. Assembled.Tickers already has SPY first when a
	// context provider is configured (assembly unions SPY first).
	instruments := make([]engine.InstrumentBars, 0, len(as.Tickers))
	for _, t := range as.Tickers {
		rows, err := a.uni.GetBars(ctx, t, start, end)
		if err != nil {
			return nil, fmt.Errorf("runner: loading %s window bars: %w", t, err)
		}
		bars := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			if hasNaN(r) {
				continue
			}
			bar, werr := engine.WrangleOHLCV(t, r)
			if werr != nil {
				return nil, fmt.Errorf("runner: wrangling %s window bar: %w", t, werr)
			}
			bars = append(bars, bar)
		}
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		instruments = append(instruments, engine.InstrumentBars{Symbol: t, Bars: bars})
	}
	return livengine.BatchBars(instruments), nil
}

// BuildWarmupBatch turns the per-symbol pre-window history served by a
// WarmupProvider into a single INTERLEAVED (dispatch-ordered) bar stream that
// primes the multi-symbol BatchWarmupConsumer strategies (sector / pairs) in the
// LIVE path. It queries the provider for each of as.WarmupBatchSymbols, drops any
// bar dated at/after runStart (look-ahead safety: warmup bars must be strictly
// before the session start), then interleaves per-symbol series by timestamp via
// the SAME BatchBars merge the backtest seed uses — so the generators see the bar
// ordering an in-loop backtest replay would, and the primed state matches a
// backtest over [runStart-lookback, runStart). A nil provider, no batch symbols,
// or an empty result is a no-op (returns nil), leaving the cold-start behaviour.
//
// runStart is the run-window start (UTC, day-aligned "now" for live). The EOD
// replay path does NOT call this — its replay already covers the full window
// in-band, so batch-priming there would double-build the state.
func (a *Assembler) BuildWarmupBatch(ctx context.Context, as *Assembled, provider livengine.WarmupProvider, runStart time.Time) ([]domain.Bar, error) {
	if provider == nil || len(as.WarmupBatchSymbols) == 0 {
		return nil, nil
	}
	syms := append([]string(nil), as.WarmupBatchSymbols...)
	sort.Strings(syms)
	instruments := make([]engine.InstrumentBars, 0, len(syms))
	for _, sym := range syms {
		hist, err := provider.WarmupBars(ctx, sym)
		if err != nil {
			return nil, fmt.Errorf("runner: batch warmup %s: %w", sym, err)
		}
		bars := make([]domain.Bar, 0, len(hist))
		for _, b := range hist {
			if !b.TS.UTC().Before(runStart) {
				continue // strictly-before-start guard (look-ahead safety)
			}
			bars = append(bars, b)
		}
		sort.SliceStable(bars, func(i, j int) bool { return bars[i].TS.Before(bars[j].TS) })
		if len(bars) > 0 {
			instruments = append(instruments, engine.InstrumentBars{Symbol: sym, Bars: bars})
		}
	}
	if len(instruments) == 0 {
		return nil, nil
	}
	return livengine.BatchBars(instruments), nil
}

// buildContext mirrors the backtest handler's buildContext exactly (SPY-driven
// regime + as-of market caps; empty earnings blackout). Returns nil when SPY
// bars are unavailable (cold-start defaults).
func (a *Assembler) buildContext(ctx context.Context, start, end calendar.Date, stocks []string) (*riskgate.ContextProvider, error) {
	warmupStart := calendar.NewDate(start.Year-2, start.Month, start.Day)
	spyRows, err := a.uni.GetBars(ctx, "SPY", warmupStart, end)
	if err != nil {
		return nil, fmt.Errorf("runner: loading SPY for context: %w", err)
	}
	if len(spyRows) == 0 {
		return nil, nil
	}
	spy := make([]riskgate.SPYBar, 0, len(spyRows))
	for _, r := range spyRows {
		spy = append(spy, riskgate.SPYBar{Date: r.TS.UTC(), Close: r.Close})
	}
	caps, err := a.uni.MarketCaps(ctx, stocks)
	if err != nil {
		return nil, fmt.Errorf("runner: loading market caps for context: %w", err)
	}
	asOf := time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)
	sf1 := make([]riskgate.SF1Row, 0, len(caps))
	for _, t := range stocks {
		mc := caps[t]
		sf1 = append(sf1, riskgate.SF1Row{
			Ticker: t, DateKey: asOf, MarketCap: mc, HasMarketCap: mc != 0,
			Dimension: "MRT", HasDimension: true,
		})
	}
	return riskgate.NewContextProvider(spy, sf1, nil, stocks, "MRT", 0), nil
}

// buildWarmup mirrors the backtest handler's buildWarmup: the 400-calendar-day
// pre-window tail per SEPA stock, strictly before the window start. Returns a
// MapWarmupProvider + the symbol list (nil when no stock has pre-window bars).
func (a *Assembler) buildWarmup(ctx context.Context, start calendar.Date, stocks []string) (livengine.WarmupProvider, []string, error) {
	if len(stocks) == 0 {
		return nil, nil, nil
	}
	warmupStart := start.AddDays(-sepaWarmupCalendarDays)
	runStart := time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)
	bars := make(map[string][]domain.Bar, len(stocks))
	for _, t := range stocks {
		rows, err := a.uni.GetBars(ctx, t, warmupStart, start)
		if err != nil {
			return nil, nil, fmt.Errorf("runner: loading %s warmup: %w", t, err)
		}
		hist := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			if !r.TS.UTC().Before(runStart) {
				continue
			}
			if hasNaN(r) {
				continue
			}
			bar, werr := engine.WrangleOHLCV(t, r)
			if werr != nil {
				return nil, nil, fmt.Errorf("runner: wrangling %s warmup bar: %w", t, werr)
			}
			hist = append(hist, bar)
		}
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].TS.Before(hist[j].TS) })
		if len(hist) > 0 {
			bars[t] = hist
		}
	}
	if len(bars) == 0 {
		return nil, nil, nil
	}
	syms := make([]string, 0, len(bars))
	for s := range bars {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	return livengine.MapWarmupProvider{Bars: bars}, syms, nil
}

// hasNaN reports whether any OHLCV field is NaN (source NULL), which cannot be
// a valid bar — mirrors the StoreFeed / backtest handler skip.
func hasNaN(r universe.OHLCV) bool {
	return math.IsNaN(r.Open) || math.IsNaN(r.High) || math.IsNaN(r.Low) ||
		math.IsNaN(r.Close) || math.IsNaN(r.Volume)
}

// unionTickers concatenates two ticker slices deduped, preserving first-seen
// order (extras — SPY first — then the rest).
func unionTickers(first, second []string) []string {
	seen := make(map[string]struct{}, len(first)+len(second))
	out := make([]string, 0, len(first)+len(second))
	for _, s := range append(append([]string(nil), first...), second...) {
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
