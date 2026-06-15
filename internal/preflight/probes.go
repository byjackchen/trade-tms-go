package preflight

import (
	"context"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Probes is the read-only seam the preflight checks run against. Every method
// is a cheap point-in-time read; the real implementation (pg.go) reads
// PostgreSQL / sharadar / moomoo, and the unit tests supply trivial fakes. The
// interface is intentionally narrow — each method maps to exactly the probe one
// check needs — so the checks stay pure and table-testable.
type Probes interface {
	// PingPostgres reports PG reachability (nil = reachable).
	PingPostgres(ctx context.Context) error
	// PingRedis reports Redis reachability (nil = reachable). A nil-returning
	// probe that is "not configured" should surface a sentinel the check treats
	// as a failure (Redis is a blocker), not a pass.
	PingRedis(ctx context.Context) error

	// DataFrontier returns the newest stored DATA date for a dataset (the same
	// frontier EnsureFresh uses), ok=false when the dataset has no rows. The
	// preflight gates on the SEP frontier (the equities bar horizon).
	DataFrontier(ctx context.Context, dataset string) (frontier calendar.Date, ok bool, err error)
	// TradingTMinus1 returns the most recent NYSE trading date strictly before
	// "today", where today is the exchange-local (America/New_York) calendar
	// date of the now instant — the SAME T-1 the sync's catchup target uses.
	TradingTMinus1(now time.Time) (calendar.Date, error)
	// TradingDaysBetween counts NYSE sessions in (from, to] — the staleness gap
	// in trading days. Returns 0 when from >= to.
	TradingDaysBetween(from, to calendar.Date) (int, error)

	// ResolveStrategy resolves the enabled strategies for the session exactly as
	// the live Assembler would: the per-strategy warmup symbols + lookback bars,
	// the resolved stock universe, and the param provenance (promoted vs
	// baseline). This is the single resolution shared with the live path, so the
	// preflight validates the SAME inputs the session will run on.
	ResolveStrategy(ctx context.Context, cfg Config) (*ResolvedSession, error)

	// BarsAvailable returns, for each requested symbol, how many daily bars
	// exist on/before asOf (the warm-up depth probe). Symbols with no bars map
	// to 0. One pass over tms.bars_daily.
	BarsAvailable(ctx context.Context, symbols []string, asOf calendar.Date) (map[string]int, error)

	// MarketCaps returns the latest market cap per ticker (0.0 when absent/NaN),
	// the SF1 fundamentals probe for the SEPA universe. Same query the live
	// context builder uses.
	MarketCaps(ctx context.Context, tickers []string) (map[string]float64, error)
	// FundamentalsFrontier returns the SF1 datekey frontier (newest filing
	// date), ok=false when SF1 is empty.
	FundamentalsFrontier(ctx context.Context) (frontier calendar.Date, ok bool, err error)

	// ListUniverseForWindow returns the survivor-bias-free tradable universe for
	// the window (the same query live's default-universe fallback uses). table
	// is the universe.Table* filter.
	ListUniverseForWindow(ctx context.Context, start, end calendar.Date, table string) ([]string, error)

	// OpenDState probes OpenD: connect + GetGlobalState. nil = reachable and the
	// market state is readable. Implementations dial TMS_MOOMOO_ADDR.
	OpenDState(ctx context.Context) error
}

// EnabledStrategy is one strategy in the session and the warmup it needs.
type EnabledStrategy struct {
	// Name is the canonical strategy id ("sepa" | "sector_rotation" | "pairs" |
	// "orb").
	Name string
	// WarmupSymbols are the instruments whose history must be deep enough to warm
	// this strategy's lookback (SEPA stocks / sector ETFs+SPY / pair legs / the
	// ORB symbol).
	WarmupSymbols []string
	// LookbackBars is the minimum number of daily bars each warmup symbol needs
	// for the strategy's rolling state to be fully formed (SEPA 200, sector
	// momentum_lookback, pairs OLS lookback).
	LookbackBars int
	// Promoted reports whether the strategy's active params came from a promoted
	// active_params row (true) rather than the embedded baseline (false).
	Promoted bool
	// ParamSource is the human-readable provenance ("db" | "file" | "baseline").
	ParamSource string
	// Screened is true when the strategy trades a survivor-bias-free SCREENED
	// universe (SEPA) rather than a FIXED basket (sector ETFs, pair legs). A
	// screened universe legitimately contains newly-listed / short-history names
	// the strategy skips at runtime until they reach their lookback, so the
	// warmup check tolerates an under-warmed tail for screened strategies but
	// requires every symbol of a fixed basket.
	Screened bool
}

// ResolvedSession is the live-equivalent resolution of the session: the enabled
// strategies (+ their warmup) and the SEPA stock universe the session will load
// caps/bars for. window{Start,End} is the assembly window the universe + bars
// were resolved against.
type ResolvedSession struct {
	// Strategies are the enabled strategies in the session.
	Strategies []EnabledStrategy
	// SEPAUniverse is the resolved SEPA stock universe (explicit --tickers, or
	// the default SF1 window universe). Empty for sessions with no SEPA leg
	// (pure sector/pairs/orb).
	SEPAUniverse []string
	// WindowStart / WindowEnd bound the assembly window (warmup horizon ->
	// asOf). The warmup-depth probe counts bars on/before WindowEnd.
	WindowStart calendar.Date
	WindowEnd   calendar.Date
}

// hasSEPA reports whether any enabled strategy needs the SEPA stock universe
// (sepa leg present) — gates the MARKET_DATA_FUNDAMENTALS + universe blockers.
func (r *ResolvedSession) hasSEPA() bool {
	for _, s := range r.Strategies {
		if s.Name == "sepa" {
			return true
		}
	}
	return false
}
