package preflight

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
)

// ErrNotConfigured is the sentinel a probe returns when a dependency is not
// wired (e.g. Redis-less deployment). For a BLOCKER dependency the check treats
// it as a failure — a live session must have its blockers actually present.
var ErrNotConfigured = errors.New("preflight: dependency not configured")

// pass / warn / fail / skip build a CheckResult for id with the given severity.
func result(id string, st Status, sev Severity, format string, args ...any) CheckResult {
	return CheckResult{Check: id, Status: st, Severity: sev, Detail: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// PG_REACHABLE — Postgres is the durable truth store; always a blocker.
// ---------------------------------------------------------------------------

func checkPostgres(ctx context.Context, _ Config, p Probes) CheckResult {
	if err := p.PingPostgres(ctx); err != nil {
		return result(CheckPostgres, StatusFail, SeverityBlocker, "postgres unreachable: %v", err)
	}
	return result(CheckPostgres, StatusPass, SeverityBlocker, "reachable")
}

// ---------------------------------------------------------------------------
// REDIS_REACHABLE — Redis carries streams + command notify; a blocker (a node
// whose control plane cannot notify is not go-live ready).
// ---------------------------------------------------------------------------

func checkRedis(ctx context.Context, _ Config, p Probes) CheckResult {
	err := p.PingRedis(ctx)
	if errors.Is(err, ErrNotConfigured) {
		return result(CheckRedis, StatusFail, SeverityBlocker, "redis not configured (control-plane notify + streams unavailable)")
	}
	if err != nil {
		return result(CheckRedis, StatusFail, SeverityBlocker, "redis unreachable: %v", err)
	}
	return result(CheckRedis, StatusPass, SeverityBlocker, "reachable")
}

// ---------------------------------------------------------------------------
// DATA_CURRENT — the data frontier (the SAME frontier EnsureFresh uses) must be
// within MaxStaleTradingDays of T-1. Blocker for paper/live; warn for signal so
// a signal dry-run is not fully blocked on a stale cache.
//
// The check gates on EVERY dataset the session trades, not just SEP: SEPA stocks
// are SEP-sourced, but the sector ETFs / SPY (sector_rotation) and the pair legs
// (pairs) are SFP-sourced funds. A state where SEP is current but SFP lags would
// otherwise PASS while sector/pairs operate on stale ETF bars (WARMUP_AVAILABLE
// only counts bar DEPTH, never recency, so years of ETF history still pass). We
// evaluate the OLDEST (min) frontier across the required datasets, so the worst-
// lagging leg decides freshness — the same min EnsureFresh effectively enforces
// by catching SEP+SFP up together each day.
// ---------------------------------------------------------------------------

func checkDataCurrent(ctx context.Context, cfg Config, p Probes) CheckResult {
	sev := SeverityWarn
	if cfg.isPaperOrLive() {
		sev = SeverityBlocker
	}

	tMinus1, err := p.TradingTMinus1(cfg.now())
	if err != nil {
		return result(CheckDataCurrent, StatusFail, sev, "resolving T-1 trading date: %v", err)
	}

	// The datasets this session's frontier depends on. SEP is always required
	// (the equities horizon + the bootstrap watermark). SFP is required whenever
	// a sector/pairs leg is enabled, because those legs trade SFP-sourced ETFs/
	// funds and a stale SFP frontier means stale ETF bars.
	datasets := []string{sharadar.DatasetSEP}
	if cfg.needsSFP() {
		datasets = append(datasets, sharadar.DatasetSFP)
	}

	// worstFrontier is the oldest frontier across the required datasets; worstDS
	// names which dataset is lagging (for the operator's detail line).
	var (
		worstFrontier calendar.Date
		worstDS       string
		haveWorst     bool
	)
	for _, ds := range datasets {
		frontier, ok, ferr := p.DataFrontier(ctx, ds)
		if ferr != nil {
			return result(CheckDataCurrent, StatusFail, sev, "reading %s data frontier: %v", ds, ferr)
		}
		if !ok {
			return result(CheckDataCurrent, StatusFail, sev,
				"no %s bars loaded — run a bootstrap/import before go-live", ds)
		}
		if !haveWorst || frontier.Before(worstFrontier) {
			worstFrontier, worstDS, haveWorst = frontier, ds, true
		}
	}

	if !worstFrontier.Before(tMinus1) {
		// Frontier at or beyond T-1: fully current (a frontier == today, e.g.
		// after an intraday import, is still "not stale").
		return result(CheckDataCurrent, StatusPass, sev, "data frontier %s (%s) >= T-1 %s", worstFrontier, worstDS, tMinus1)
	}

	gap, err := p.TradingDaysBetween(worstFrontier, tMinus1)
	if err != nil {
		return result(CheckDataCurrent, StatusFail, sev, "computing staleness gap: %v", err)
	}
	if gap <= cfg.maxStale() {
		return result(CheckDataCurrent, StatusPass, sev,
			"data frontier %s (%s) is %d trading day(s) behind T-1 %s (within tolerance %d)", worstFrontier, worstDS, gap, tMinus1, cfg.maxStale())
	}
	st := StatusFail
	if sev == SeverityWarn {
		st = StatusWarn // signal mode: surface staleness, do not block
	}
	return result(CheckDataCurrent, st, sev,
		"%s data frontier %s is %d trading day(s) behind T-1 %s (tolerance %d) — run a sync before go-live", worstDS, worstFrontier, gap, tMinus1, cfg.maxStale())
}

// ---------------------------------------------------------------------------
// UNIVERSE_RESOLVABLE — ListUniverseForWindow returns a non-empty survivor-bias-
// free set for the window. Blocker for sepa/multi (they need a stock universe);
// a pass-through note for sessions with no SEPA leg.
// ---------------------------------------------------------------------------

func checkUniverse(ctx context.Context, cfg Config, p Probes) CheckResult {
	res, err := p.ResolveStrategy(ctx, cfg)
	if err != nil {
		return result(CheckUniverse, StatusFail, SeverityBlocker, "resolving session: %v", err)
	}
	if !res.hasSEPA() {
		// No SEPA leg: there is no survivor-bias universe to resolve (sector ETFs
		// / pair legs / ORB are fixed instrument lists). Not applicable -> pass.
		return result(CheckUniverse, StatusPass, SeverityBlocker, "no SEPA leg — fixed instrument set (universe N/A)")
	}
	// The resolved SEPA universe is already the survivor-bias-free set (explicit
	// --tickers, or ListUniverseForWindow over the window). Re-probe the raw
	// query too, so an explicit --tickers list that happens to be tradable is
	// distinguished from a degenerate empty DB.
	if len(res.SEPAUniverse) == 0 {
		return result(CheckUniverse, StatusFail, SeverityBlocker,
			"resolved SEPA universe is empty for [%s, %s] — load bars / pass --tickers", res.WindowStart, res.WindowEnd)
	}
	dbUni, err := p.ListUniverseForWindow(ctx, res.WindowStart, res.WindowEnd, "SF1")
	if err != nil {
		return result(CheckUniverse, StatusFail, SeverityBlocker, "querying window universe: %v", err)
	}
	if len(dbUni) == 0 {
		return result(CheckUniverse, StatusFail, SeverityBlocker,
			"SF1 window universe [%s, %s] is empty (tickers table not loaded)", res.WindowStart, res.WindowEnd)
	}
	return result(CheckUniverse, StatusPass, SeverityBlocker,
		"%d-name SEPA universe resolvable for [%s, %s]", len(res.SEPAUniverse), res.WindowStart, res.WindowEnd)
}

// ---------------------------------------------------------------------------
// MARKET_DATA_FUNDAMENTALS — SF1 market caps present + current for the SEPA
// universe (SEPA sizes/filters on caps; the earlier all-degenerate-zero bug).
// Blocker for sepa/multi; pass-through for sessions with no SEPA leg.
// ---------------------------------------------------------------------------

// minCapCoverage is the fraction of the SEPA universe that must have a non-zero
// market cap. SEPA tolerates SOME unknown caps (they sort last / fail rule 8),
// but an all-zero (or near-all-zero) universe is the degenerate bug — every
// stock fails the cap rule and SEPA emits nothing.
const minCapCoverage = 0.5

func checkMarketDataFundamentals(ctx context.Context, cfg Config, p Probes) CheckResult {
	res, err := p.ResolveStrategy(ctx, cfg)
	if err != nil {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker, "resolving session: %v", err)
	}
	if !res.hasSEPA() {
		return result(CheckMarketDataFund, StatusPass, SeverityBlocker, "no SEPA leg — market caps not required")
	}
	if len(res.SEPAUniverse) == 0 {
		// The universe check owns the empty-universe failure; here it would be a
		// duplicate. Report pass so the operator sees one root cause.
		return result(CheckMarketDataFund, StatusPass, SeverityBlocker, "empty SEPA universe (see UNIVERSE_RESOLVABLE)")
	}

	// SF1 freshness: the fundamentals frontier should not be absent. SF1 datekeys
	// lag bar dates (quarterly filings), so we do NOT require it within T-1 — only
	// that SF1 exists at all and that the universe's caps are populated.
	if _, ok, ferr := p.FundamentalsFrontier(ctx); ferr != nil {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker, "reading SF1 frontier: %v", ferr)
	} else if !ok {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker, "tms.fundamentals_sf1 is empty — SEPA cannot size without market caps")
	}

	caps, err := p.MarketCaps(ctx, res.SEPAUniverse)
	if err != nil {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker, "loading market caps: %v", err)
	}
	have := 0
	for _, t := range res.SEPAUniverse {
		if caps[t] > 0 {
			have++
		}
	}
	frac := float64(have) / float64(len(res.SEPAUniverse))
	if have == 0 {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker,
			"0/%d SEPA names have a market cap — the all-degenerate case (SEPA would emit nothing)", len(res.SEPAUniverse))
	}
	if frac < minCapCoverage {
		return result(CheckMarketDataFund, StatusFail, SeverityBlocker,
			"only %d/%d (%.0f%%) SEPA names have a market cap (need >= %.0f%%)", have, len(res.SEPAUniverse), frac*100, minCapCoverage*100)
	}
	return result(CheckMarketDataFund, StatusPass, SeverityBlocker,
		"%d/%d SEPA names have market caps (%.0f%%)", have, len(res.SEPAUniverse), frac*100)
}

// ---------------------------------------------------------------------------
// WARMUP_AVAILABLE — for EACH enabled strategy, enough historical bars exist to
// warm its lookback for its warmup universe. Blocker. This is the gap that let
// a SEPA-only warmup ship while sector/pairs started cold.
// ---------------------------------------------------------------------------

func checkWarmupAvailable(ctx context.Context, cfg Config, p Probes) CheckResult {
	res, err := p.ResolveStrategy(ctx, cfg)
	if err != nil {
		return result(CheckWarmupAvailable, StatusFail, SeverityBlocker, "resolving session: %v", err)
	}
	if len(res.Strategies) == 0 {
		return result(CheckWarmupAvailable, StatusFail, SeverityBlocker, "no enabled strategies resolved")
	}

	// Collect every distinct warmup symbol across all strategies, probe their bar
	// depth in one pass, then verify each strategy's lookback is met by ALL its
	// symbols.
	symSet := map[string]struct{}{}
	for _, s := range res.Strategies {
		for _, sym := range s.WarmupSymbols {
			symSet[sym] = struct{}{}
		}
	}
	syms := make([]string, 0, len(symSet))
	for sym := range symSet {
		syms = append(syms, sym)
	}
	sort.Strings(syms)

	depth, err := p.BarsAvailable(ctx, syms, res.WindowEnd)
	if err != nil {
		return result(CheckWarmupAvailable, StatusFail, SeverityBlocker, "probing bar depth: %v", err)
	}

	var shortfalls []string
	for _, s := range res.Strategies {
		if len(s.WarmupSymbols) == 0 {
			shortfalls = append(shortfalls, fmt.Sprintf("%s: no warmup symbols resolved", s.Name))
			continue
		}
		// The worst-covered symbol decides the strategy: a single under-warmed
		// instrument leaves that strategy's state partially cold.
		worstSym, worst := "", -1
		for _, sym := range s.WarmupSymbols {
			d := depth[sym]
			if worst < 0 || d < worst {
				worst, worstSym = d, sym
			}
		}
		if worst < s.LookbackBars {
			shortfalls = append(shortfalls,
				fmt.Sprintf("%s: %s has %d bars < %d lookback", s.Name, worstSym, worst, s.LookbackBars))
		}
	}
	if len(shortfalls) > 0 {
		return result(CheckWarmupAvailable, StatusFail, SeverityBlocker, "warmup shortfall: %s", joinDetail(shortfalls))
	}
	return result(CheckWarmupAvailable, StatusPass, SeverityBlocker,
		"all %d strategies have enough warmup bars (%d symbols probed)", len(res.Strategies), len(syms))
}

// ---------------------------------------------------------------------------
// PARAMS_PROMOTED — each enabled strategy has a promoted active_params row (not
// just baseline). warn (live still runs on baseline; the operator is flagged).
// ---------------------------------------------------------------------------

func checkParamsPromoted(ctx context.Context, cfg Config, p Probes) CheckResult {
	res, err := p.ResolveStrategy(ctx, cfg)
	if err != nil {
		return result(CheckParamsPromoted, StatusFail, SeverityWarn, "resolving session: %v", err)
	}
	if len(res.Strategies) == 0 {
		return result(CheckParamsPromoted, StatusWarn, SeverityWarn, "no strategies resolved")
	}
	var baseline []string
	for _, s := range res.Strategies {
		if !s.Promoted {
			baseline = append(baseline, fmt.Sprintf("%s (%s)", s.Name, s.ParamSource))
		}
	}
	if len(baseline) > 0 {
		return result(CheckParamsPromoted, StatusWarn, SeverityWarn,
			"running on un-promoted params: %s — live runs but consider promoting tuned params", joinDetail(baseline))
	}
	return result(CheckParamsPromoted, StatusPass, SeverityWarn,
		"all %d strategies on promoted (db) params", len(res.Strategies))
}

// ---------------------------------------------------------------------------
// OPEND_REACHABLE — connect + GetGlobalState. Blocker for paper/live; for signal
// it is SKIPPED unless --check-opend (a signal dry-check should not be blocked on
// a broker that may be intentionally absent).
// ---------------------------------------------------------------------------

func checkOpenD(ctx context.Context, cfg Config, p Probes) CheckResult {
	sev := SeverityBlocker
	if !cfg.isPaperOrLive() {
		// signal mode
		if !cfg.CheckOpenD {
			return result(CheckOpenD, StatusSkip, SeverityWarn, "skipped (signal mode; pass --check-opend to probe OpenD)")
		}
		sev = SeverityWarn // requested in signal mode: surface, do not block
	}
	if err := p.OpenDState(ctx); err != nil {
		return result(CheckOpenD, StatusFail, sev, "OpenD not reachable / GetGlobalState failed: %v", err)
	}
	return result(CheckOpenD, StatusPass, sev, "OpenD connected, GetGlobalState ok")
}

// joinDetail joins detail fragments with "; ".
func joinDetail(parts []string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
