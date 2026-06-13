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
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// sepaWarmupCalendarDays mirrors the backtest handler: 400 calendar days of
// out-of-band SEPA warmup before the window start.
const sepaWarmupCalendarDays = 400

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
	// WarmupSymbols are the symbols to prime (the SEPA stock universe).
	WarmupSymbols []string
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
		loader: params.NewLoader(params.DBPayloadReader{Q: pool}, paramsDir),
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
	asmIn := strategyassembly.Input{
		Strategy:        in.Strategy,
		StartingBalance: in.StartingBalance,
		SEPAStocks:      in.Tickers,
		ORBSymbol:       in.ORBSymbol,
		SPYSymbol:       spy,
	}

	var (
		warmup        livengine.WarmupProvider
		warmupSymbols []string
		err           error
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
	case "pairs":
		if asmIn.Params.Pairs, _, err = a.loader.Pairs(ctx); err != nil {
			return nil, fmt.Errorf("runner: resolve pairs params: %w", err)
		}
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
	return &Assembled{
		Assembly:      asm,
		Tickers:       tickers,
		Warmup:        warmup,
		WarmupSymbols: warmupSymbols,
		SPYSymbol:     asm.SPYSymbol,
	}, nil
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

// buildContext mirrors the backtest handler's buildContext exactly (SPY-driven
// regime + as-of market caps; empty earnings blackout). Returns nil when SPY
// bars are unavailable (cold-start defaults).
func (a *Assembler) buildContext(ctx context.Context, start, end calendar.Date, stocks []string) (*portfolio.ContextProvider, error) {
	warmupStart := calendar.NewDate(start.Year-2, start.Month, start.Day)
	spyRows, err := a.uni.GetBars(ctx, "SPY", warmupStart, end)
	if err != nil {
		return nil, fmt.Errorf("runner: loading SPY for context: %w", err)
	}
	if len(spyRows) == 0 {
		return nil, nil
	}
	spy := make([]portfolio.SPYBar, 0, len(spyRows))
	for _, r := range spyRows {
		spy = append(spy, portfolio.SPYBar{Date: r.TS.UTC(), Close: r.Close})
	}
	caps, err := a.uni.MarketCaps(ctx, stocks)
	if err != nil {
		return nil, fmt.Errorf("runner: loading market caps for context: %w", err)
	}
	asOf := time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)
	sf1 := make([]portfolio.SF1Row, 0, len(caps))
	for _, t := range stocks {
		mc := caps[t]
		sf1 = append(sf1, portfolio.SF1Row{
			Ticker: t, DateKey: asOf, MarketCap: mc, HasMarketCap: mc != 0,
			Dimension: "MRT", HasDimension: true,
		})
	}
	return portfolio.NewContextProvider(spy, sf1, nil, stocks, "MRT", 0), nil
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
