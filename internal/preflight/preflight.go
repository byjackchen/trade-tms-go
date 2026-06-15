// Package preflight is the go-live PREFLIGHT: a structured set of precondition
// checks that MUST pass before a paper/live (and, with relaxed severity, a
// signal) session is started. It exists because an earlier audit found three
// latent gaps that nothing caught at startup — warmup that only primed SEPA,
// silently stale market data, and the data-freshness watermark bug — each of
// which a preflight would have BLOCKED. The system, not the operator's eyeball,
// now enforces the preconditions.
//
// Design: preflight depends on small read-only seams (the Probes interface),
// every one of which has a real PG/sharadar/moomoo implementation (pg.go) and a
// trivial fake (the unit tests). Run executes the checks, returns a structured
// Report, and the report knows whether any BLOCKER failed for the session mode.
// The same Report backs the CLI table, the `tms live` startup gate, and the
// GET /api/v1/live/preflight endpoint — one source of truth.
package preflight

import (
	"context"
	"sort"
	"time"
)

// Status is a check outcome.
type Status string

const (
	// StatusPass — the precondition holds.
	StatusPass Status = "pass"
	// StatusWarn — a non-fatal concern (e.g. running on baseline params, or
	// stale data in signal mode where freshness is advisory).
	StatusWarn Status = "warn"
	// StatusFail — the precondition does not hold.
	StatusFail Status = "fail"
	// StatusSkip — the check did not run (e.g. OpenD reachability in a signal
	// dry-check without --check-opend). A skipped check never blocks.
	StatusSkip Status = "skip"
)

// Severity classifies how a failing check affects go-live.
type Severity string

const (
	// SeverityBlocker — a failing check REFUSES the session (exit non-zero /
	// startup aborted).
	SeverityBlocker Severity = "blocker"
	// SeverityWarn — a failing check is surfaced loudly but does not refuse the
	// session.
	SeverityWarn Severity = "warn"
)

// Check ids (stable strings — the UI/runbook/api key off these).
const (
	CheckDataCurrent     = "DATA_CURRENT"
	CheckWarmupAvailable = "WARMUP_AVAILABLE"
	CheckParamsPromoted  = "PARAMS_PROMOTED"
	CheckMarketDataFund  = "MARKET_DATA_FUNDAMENTALS"
	CheckUniverse        = "UNIVERSE_RESOLVABLE"
	CheckOpenD           = "OPEND_REACHABLE"
	CheckRedis           = "REDIS_REACHABLE"
	CheckPostgres        = "PG_REACHABLE"
)

// CheckResult is one precondition's outcome.
type CheckResult struct {
	// Check is the stable check id (CheckDataCurrent, ...).
	Check string `json:"check"`
	// Status is pass | warn | fail | skip.
	Status Status `json:"status"`
	// Severity is blocker | warn — how a fail/skip affects go-live FOR THIS
	// session (DATA_CURRENT is a blocker for paper/live but only a warn for
	// signal, so severity is resolved per-mode, not fixed on the check).
	Severity Severity `json:"severity"`
	// Detail is a human-readable elaboration (the probe value, the failure
	// reason). Never carries secrets.
	Detail string `json:"detail"`
}

// blocking reports whether this result blocks go-live: a fail whose severity is
// blocker. A warn-severity fail, a pass, a skip and a warn never block.
func (r CheckResult) blocking() bool {
	return r.Status == StatusFail && r.Severity == SeverityBlocker
}

// Report is the full preflight outcome.
type Report struct {
	// Mode is the session mode the report was evaluated for (signal|paper|live).
	Mode string `json:"mode"`
	// Strategy is the strategy set the report was evaluated for.
	Strategy string `json:"strategy"`
	// TS is when the preflight ran (UTC).
	TS time.Time `json:"ts"`
	// Checks are the per-check results, in a stable presentation order.
	Checks []CheckResult `json:"checks"`
	// OK is true iff no BLOCKER check failed (the go/no-go bit). A report with
	// only warnings is OK=true (live still runs, the warnings are surfaced).
	OK bool `json:"ok"`
}

// Blockers returns the failing blocker checks (empty when OK).
func (r Report) Blockers() []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		if c.blocking() {
			out = append(out, c)
		}
	}
	return out
}

// Warnings returns the checks that warrant operator attention but do not refuse
// go-live (warn status, or a warn-severity fail).
func (r Report) Warnings() []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		if c.Status == StatusWarn || (c.Status == StatusFail && c.Severity == SeverityWarn) {
			out = append(out, c)
		}
	}
	return out
}

// checkOrder is the stable presentation order (most fundamental first).
var checkOrder = map[string]int{
	CheckPostgres:        0,
	CheckRedis:           1,
	CheckDataCurrent:     2,
	CheckUniverse:        3,
	CheckMarketDataFund:  4,
	CheckWarmupAvailable: 5,
	CheckParamsPromoted:  6,
	CheckOpenD:           7,
}

// Config selects what session the preflight is validating.
type Config struct {
	// Mode is "signal" | "paper" | "live". Drives per-check severity (signal
	// treats DATA_CURRENT + OPEND as advisory; paper/live require them).
	Mode string
	// Strategy is "sepa" | "sector_rotation" | "pairs" | "orb" | "multi".
	Strategy string
	// Tickers is the explicit SEPA stock universe (sepa/multi); empty falls back
	// to the default SF1 window universe (the same fallback live uses).
	Tickers []string
	// ORBSymbol is the ORB instrument (orb path).
	ORBSymbol string
	// MaxStaleTradingDays is the DATA_CURRENT tolerance: the data frontier must
	// be within this many NYSE trading days of T-1. Default 1 (<= one trading
	// day behind T-1).
	MaxStaleTradingDays int
	// CheckOpenD forces the OPEND_REACHABLE probe even in signal mode (the
	// `--check-opend` flag). Paper/live always probe it.
	CheckOpenD bool
	// Now overrides the clock (tests); nil = time.Now.
	Now func() time.Time
}

// maxStale resolves the staleness tolerance (default 1 trading day).
func (c Config) maxStale() int {
	if c.MaxStaleTradingDays > 0 {
		return c.MaxStaleTradingDays
	}
	return 1
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// isPaperOrLive reports whether the mode submits orders (paper/live), where the
// strict severities apply.
func (c Config) isPaperOrLive() bool {
	return c.Mode == "paper" || c.Mode == "live"
}

// needsSFP reports whether the session trades SFP-sourced funds (the sector ETFs
// + SPY of sector_rotation, the ETF/fund pair legs of pairs). When true the
// DATA_CURRENT check additionally gates on the SFP frontier, so a fresh-SEP/
// stale-SFP split cannot pass a sector/pairs go-live. "multi" runs both legs, so
// it needs SFP too. sepa-only and orb (a single intraday SEP/SFP instrument
// gated by its own warmup) do not pull in the SFP frontier requirement here.
func (c Config) needsSFP() bool {
	switch c.Strategy {
	case "sector_rotation", "pairs", "multi":
		return true
	default:
		return false
	}
}

// Run executes the preflight checks for cfg against probes and returns the
// Report. It never returns an error — a probe failure becomes a failing check
// (a preflight that itself errored is useless; the whole point is a verdict).
// Checks run sequentially: they are cheap point-in-time reads and the ordering
// keeps the report deterministic.
func Run(ctx context.Context, cfg Config, probes Probes) Report {
	checks := []CheckResult{
		checkPostgres(ctx, cfg, probes),
		checkRedis(ctx, cfg, probes),
		checkDataCurrent(ctx, cfg, probes),
		checkUniverse(ctx, cfg, probes),
		checkMarketDataFundamentals(ctx, cfg, probes),
		checkWarmupAvailable(ctx, cfg, probes),
		checkParamsPromoted(ctx, cfg, probes),
		checkOpenD(ctx, cfg, probes),
	}
	sort.SliceStable(checks, func(i, j int) bool {
		return checkOrder[checks[i].Check] < checkOrder[checks[j].Check]
	})

	ok := true
	for _, c := range checks {
		if c.blocking() {
			ok = false
		}
	}
	return Report{
		Mode:     cfg.Mode,
		Strategy: cfg.Strategy,
		TS:       cfg.now(),
		Checks:   checks,
		OK:       ok,
	}
}
