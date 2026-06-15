package preflight

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
)

// fakeProbes is a fully scriptable Probes for the unit tests: every method reads
// a field, so each test seeds exactly the condition under test and leaves the
// rest in their "healthy" default.
type fakeProbes struct {
	pgErr    error
	redisErr error

	frontier   map[string]calendar.Date // dataset -> frontier date
	frontierOK map[string]bool
	frontErr   error

	tMinus1    calendar.Date
	tMinus1Err error

	gap     int
	gapErr  error
	gapFunc func(from, to calendar.Date) (int, error)

	resolved   *ResolvedSession
	resolveErr error

	bars    map[string]int
	barsErr error

	caps    map[string]float64
	capsErr error

	fundFrontierOK  bool
	fundFrontierErr error

	windowUni    []string
	windowUniErr error

	opendErr error

	// openDMaxSub is the OpenD per-connection cap the SUBSCRIPTION_CAP check
	// sizes against; 0 -> the moomoo default (100) via the accessor below.
	openDMaxSub int
}

func (f *fakeProbes) PingPostgres(context.Context) error { return f.pgErr }
func (f *fakeProbes) PingRedis(context.Context) error    { return f.redisErr }

func (f *fakeProbes) DataFrontier(_ context.Context, ds string) (calendar.Date, bool, error) {
	if f.frontErr != nil {
		return calendar.Date{}, false, f.frontErr
	}
	return f.frontier[ds], f.frontierOK[ds], nil
}

func (f *fakeProbes) FundamentalsFrontier(context.Context) (calendar.Date, bool, error) {
	return calendar.Date{}, f.fundFrontierOK, f.fundFrontierErr
}

func (f *fakeProbes) TradingTMinus1(time.Time) (calendar.Date, error) {
	return f.tMinus1, f.tMinus1Err
}

func (f *fakeProbes) TradingDaysBetween(from, to calendar.Date) (int, error) {
	if f.gapFunc != nil {
		return f.gapFunc(from, to)
	}
	return f.gap, f.gapErr
}

func (f *fakeProbes) ResolveStrategy(context.Context, Config) (*ResolvedSession, error) {
	return f.resolved, f.resolveErr
}

func (f *fakeProbes) BarsAvailable(_ context.Context, syms []string, _ calendar.Date) (map[string]int, error) {
	if f.barsErr != nil {
		return nil, f.barsErr
	}
	out := map[string]int{}
	for _, s := range syms {
		out[s] = f.bars[s]
	}
	return out, nil
}

func (f *fakeProbes) MarketCaps(_ context.Context, tickers []string) (map[string]float64, error) {
	if f.capsErr != nil {
		return nil, f.capsErr
	}
	out := map[string]float64{}
	for _, t := range tickers {
		out[t] = f.caps[t]
	}
	return out, nil
}

func (f *fakeProbes) ListUniverseForWindow(context.Context, calendar.Date, calendar.Date, string) ([]string, error) {
	return f.windowUni, f.windowUniErr
}

func (f *fakeProbes) OpenDState(context.Context) error { return f.opendErr }

func (f *fakeProbes) OpenDMaxSubscriptions() int {
	if f.openDMaxSub > 0 {
		return f.openDMaxSub
	}
	return 100 // moomoo.DefaultMaxSubscriptions
}

// healthyProbes returns a fakeProbes seeded so EVERY check passes for a
// paper-mode multi-strategy session. Tests mutate one field to exercise a
// failure mode.
func healthyProbes() *fakeProbes {
	tMinus1 := calendar.NewDate(2026, time.June, 11)
	frontier := calendar.NewDate(2026, time.June, 11)
	uni := []string{"AAPL", "MSFT", "NVDA", "AMZN"}
	return &fakeProbes{
		frontier:       map[string]calendar.Date{sharadar.DatasetSEP: frontier, sharadar.DatasetSFP: frontier},
		frontierOK:     map[string]bool{sharadar.DatasetSEP: true, sharadar.DatasetSFP: true},
		tMinus1:        tMinus1,
		fundFrontierOK: true,
		windowUni:      uni,
		bars: map[string]int{
			"AAPL": 500, "MSFT": 500, "NVDA": 500, "AMZN": 500,
			"XLK": 500, "XLF": 500, "SPY": 500, "GLD": 500, "GDX": 500,
		},
		caps: map[string]float64{
			"AAPL": 3e12, "MSFT": 2.5e12, "NVDA": 2e12, "AMZN": 1.5e12,
		},
		resolved: &ResolvedSession{
			WindowStart:  calendar.NewDate(2025, time.May, 1),
			WindowEnd:    tMinus1,
			SEPAUniverse: uni,
			Strategies: []EnabledStrategy{
				{Name: "sepa", WarmupSymbols: uni, LookbackBars: 200, Promoted: true, ParamSource: "db"},
				{Name: "sector_rotation", WarmupSymbols: []string{"XLK", "XLF", "SPY"}, LookbackBars: 90, Promoted: true, ParamSource: "db"},
				{Name: "pairs", WarmupSymbols: []string{"GLD", "GDX"}, LookbackBars: 60, Promoted: true, ParamSource: "db"},
			},
		},
		// gap not used in the healthy path (frontier >= T-1).
	}
}

func paperCfg() Config {
	return Config{Mode: "paper", Strategy: "multi", MaxStaleTradingDays: 1, CheckOpenD: true,
		Now: func() time.Time { return time.Date(2026, time.June, 12, 14, 0, 0, 0, time.UTC) }}
}

// findCheck returns the result for id (t.Fatal if absent).
func findCheck(t *testing.T, r Report, id string) CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Check == id {
			return c
		}
	}
	t.Fatalf("check %s not in report", id)
	return CheckResult{}
}

func TestRun_AllPass(t *testing.T) {
	r := Run(context.Background(), paperCfg(), healthyProbes())
	if !r.OK {
		t.Fatalf("expected OK report, got blockers: %v", r.Blockers())
	}
	if len(r.Checks) != 9 {
		t.Fatalf("expected 9 checks, got %d", len(r.Checks))
	}
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			t.Errorf("check %s unexpectedly failed: %s", c.Check, c.Detail)
		}
	}
}

func TestCheckPostgres(t *testing.T) {
	f := healthyProbes()
	f.pgErr = errors.New("conn refused")
	r := Run(context.Background(), paperCfg(), f)
	c := findCheck(t, r, CheckPostgres)
	if c.Status != StatusFail || c.Severity != SeverityBlocker {
		t.Fatalf("pg down must be a blocker fail, got %+v", c)
	}
	if r.OK {
		t.Fatal("report must not be OK when PG is down")
	}
}

func TestCheckRedis(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		f := healthyProbes()
		f.redisErr = errors.New("timeout")
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckRedis)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("redis down must be a blocker fail, got %+v", c)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		f := healthyProbes()
		f.redisErr = ErrNotConfigured
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckRedis)
		if c.Status != StatusFail {
			t.Fatalf("redis not configured must fail, got %+v", c)
		}
	})
}

func TestCheckDataCurrent(t *testing.T) {
	t.Run("fresh frontier passes", func(t *testing.T) {
		c := findCheck(t, Run(context.Background(), paperCfg(), healthyProbes()), CheckDataCurrent)
		if c.Status != StatusPass {
			t.Fatalf("fresh data must pass, got %+v", c)
		}
	})

	t.Run("within tolerance passes", func(t *testing.T) {
		f := healthyProbes()
		// frontier two days before T-1, tolerance 1 -> still need gap<=1; set gap=1.
		f.frontier[sharadar.DatasetSEP] = calendar.NewDate(2026, time.June, 10)
		f.gap = 1
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckDataCurrent)
		if c.Status != StatusPass {
			t.Fatalf("1-day-stale within tolerance must pass, got %+v", c)
		}
	})

	t.Run("stale blocks paper/live", func(t *testing.T) {
		f := healthyProbes()
		f.frontier[sharadar.DatasetSEP] = calendar.NewDate(2026, time.June, 1)
		f.gap = 7
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckDataCurrent)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("stale data must block paper, got %+v", c)
		}
		if r.OK {
			t.Fatal("stale data must make report not OK in paper mode")
		}
	})

	t.Run("stale only warns in signal", func(t *testing.T) {
		f := healthyProbes()
		f.frontier[sharadar.DatasetSEP] = calendar.NewDate(2026, time.June, 1)
		f.gap = 7
		cfg := paperCfg()
		cfg.Mode = "signal"
		r := Run(context.Background(), cfg, f)
		c := findCheck(t, r, CheckDataCurrent)
		if c.Severity != SeverityWarn {
			t.Fatalf("signal mode data freshness must be warn-severity, got %+v", c)
		}
		if c.Status != StatusWarn {
			t.Fatalf("signal stale data must be warn status, got %+v", c)
		}
		if r.Blockers() != nil {
			t.Fatalf("stale data must not block a signal session, blockers=%v", r.Blockers())
		}
	})

	t.Run("no bars loaded fails", func(t *testing.T) {
		f := healthyProbes()
		f.frontierOK[sharadar.DatasetSEP] = false
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckDataCurrent)
		if c.Status != StatusFail {
			t.Fatalf("no bars must fail, got %+v", c)
		}
	})

	t.Run("fresh SEP but stale SFP blocks sector/pairs (finding 2)", func(t *testing.T) {
		// The exact latent split: a parquet import advanced SEP to T-1 but left SFP
		// days behind. SEP-only gating would PASS; the session's sector/pairs legs
		// would then trade on stale ETF bars. DATA_CURRENT must take the oldest
		// frontier (SFP here) and block.
		f := healthyProbes()
		f.frontier[sharadar.DatasetSFP] = calendar.NewDate(2026, time.June, 1) // SFP lags
		f.gap = 7
		r := Run(context.Background(), paperCfg(), f) // strategy "multi" -> needs SFP
		c := findCheck(t, r, CheckDataCurrent)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("stale SFP must block a multi (sector/pairs) session, got %+v", c)
		}
		if r.OK {
			t.Fatal("stale SFP must make a multi report not OK")
		}
		if !contains(c.Detail, sharadar.DatasetSFP) {
			t.Fatalf("detail must name the lagging dataset SFP, got %q", c.Detail)
		}
	})

	t.Run("stale SFP is irrelevant to a sepa-only session", func(t *testing.T) {
		// A sepa-only session does not trade SFP funds, so a stale SFP frontier must
		// NOT block it (only SEP is gated).
		f := healthyProbes()
		f.frontier[sharadar.DatasetSFP] = calendar.NewDate(2026, time.June, 1)
		f.frontierOK[sharadar.DatasetSFP] = false // even an empty SFP must not matter
		f.resolved.Strategies = []EnabledStrategy{
			{Name: "sepa", WarmupSymbols: f.resolved.SEPAUniverse, LookbackBars: 200, Promoted: true, ParamSource: "db"},
		}
		cfg := paperCfg()
		cfg.Strategy = "sepa"
		c := findCheck(t, Run(context.Background(), cfg, f), CheckDataCurrent)
		if c.Status != StatusPass {
			t.Fatalf("sepa-only session must ignore SFP staleness, got %+v", c)
		}
	})
}

// contains reports whether s contains substr (avoids importing strings just for
// this assertion).
func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestCheckUniverse(t *testing.T) {
	t.Run("empty resolved universe fails", func(t *testing.T) {
		f := healthyProbes()
		f.resolved.SEPAUniverse = nil
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckUniverse)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("empty universe must block, got %+v", c)
		}
	})

	t.Run("empty DB window universe fails", func(t *testing.T) {
		f := healthyProbes()
		f.windowUni = nil
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckUniverse)
		if c.Status != StatusFail {
			t.Fatalf("empty window universe must fail, got %+v", c)
		}
	})

	t.Run("no SEPA leg is N/A pass", func(t *testing.T) {
		f := healthyProbes()
		f.resolved.SEPAUniverse = nil
		f.resolved.Strategies = []EnabledStrategy{
			{Name: "sector_rotation", WarmupSymbols: []string{"XLK", "SPY"}, LookbackBars: 90, Promoted: true, ParamSource: "db"},
		}
		cfg := paperCfg()
		cfg.Strategy = "sector_rotation"
		c := findCheck(t, Run(context.Background(), cfg, f), CheckUniverse)
		if c.Status != StatusPass {
			t.Fatalf("sector-only session universe check must pass (N/A), got %+v", c)
		}
	})
}

func TestCheckMarketDataFundamentals(t *testing.T) {
	t.Run("all-degenerate caps fail", func(t *testing.T) {
		f := healthyProbes()
		f.caps = map[string]float64{} // every name -> 0
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckMarketDataFund)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("all-zero caps must block, got %+v", c)
		}
		if r.OK {
			t.Fatal("all-degenerate caps must make report not OK")
		}
	})

	t.Run("below-coverage caps fail", func(t *testing.T) {
		f := healthyProbes()
		f.caps = map[string]float64{"AAPL": 3e12} // 1/4 = 25% < 50%
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckMarketDataFund)
		if c.Status != StatusFail {
			t.Fatalf("low cap coverage must fail, got %+v", c)
		}
	})

	t.Run("empty SF1 fails", func(t *testing.T) {
		f := healthyProbes()
		f.fundFrontierOK = false
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckMarketDataFund)
		if c.Status != StatusFail {
			t.Fatalf("empty SF1 must fail, got %+v", c)
		}
	})

	t.Run("no SEPA leg passes", func(t *testing.T) {
		f := healthyProbes()
		f.resolved.SEPAUniverse = nil
		f.resolved.Strategies = []EnabledStrategy{
			{Name: "pairs", WarmupSymbols: []string{"GLD", "GDX"}, LookbackBars: 60, Promoted: true, ParamSource: "db"},
		}
		cfg := paperCfg()
		cfg.Strategy = "pairs"
		c := findCheck(t, Run(context.Background(), cfg, f), CheckMarketDataFund)
		if c.Status != StatusPass {
			t.Fatalf("pairs-only session must pass caps check, got %+v", c)
		}
	})
}

func TestCheckWarmupAvailable(t *testing.T) {
	t.Run("shortfall in one strategy blocks", func(t *testing.T) {
		f := healthyProbes()
		f.bars["XLK"] = 10 // sector needs 90
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckWarmupAvailable)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("warmup shortfall must block, got %+v", c)
		}
		if r.OK {
			t.Fatal("warmup shortfall must make report not OK")
		}
	})

	t.Run("SEPA-only-warmup gap caught", func(t *testing.T) {
		// The exact latent bug: SEPA stocks warmed (200+ bars) but sector/pairs
		// instruments cold. The check must catch sector/pairs even though SEPA is fine.
		f := healthyProbes()
		f.bars["SPY"] = 5
		f.bars["GLD"] = 5
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckWarmupAvailable)
		if c.Status != StatusFail {
			t.Fatalf("cold sector/pairs warmup must fail even when SEPA is warm, got %+v", c)
		}
	})

	t.Run("missing warmup symbols fails", func(t *testing.T) {
		f := healthyProbes()
		f.resolved.Strategies[1].WarmupSymbols = nil // sector has no symbols resolved
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckWarmupAvailable)
		if c.Status != StatusFail {
			t.Fatalf("no warmup symbols must fail, got %+v", c)
		}
	})
}

func TestCheckSubscriptionCap(t *testing.T) {
	t.Run("capped set fits passes", func(t *testing.T) {
		// Healthy multi session: 4 SEPA + fixed baskets (SPY, XLK, XLF, GLD, GDX)
		// is well under the 100-sub OpenD cap, so the live subscription set fits.
		c := findCheck(t, Run(context.Background(), paperCfg(), healthyProbes()), CheckSubscriptionCap)
		if c.Status != StatusPass || c.Severity != SeverityBlocker {
			t.Fatalf("a fitting subscription set must PASS (blocker severity), got %+v", c)
		}
	})

	t.Run("large SEPA universe is capped to fit (still passes)", func(t *testing.T) {
		// A huge survivor-bias-free SEPA universe (the real 4682-name set) must be
		// CAPPED by market cap to fit, not rejected: SUBSCRIPTION_CAP passes because
		// the shared helper sizes SEPA to the budget left under the OpenD cap.
		f := healthyProbes()
		big := make([]string, 4682)
		caps := map[string]float64{}
		for i := range big {
			tk := "T" + strconv.Itoa(i)
			big[i] = tk
			caps[tk] = float64(4682 - i) // descending caps so ranking is deterministic
		}
		f.resolved.SEPAUniverse = big
		f.resolved.Strategies[0].WarmupSymbols = big
		f.caps = caps
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckSubscriptionCap)
		if c.Status != StatusPass {
			t.Fatalf("a 4682-name SEPA universe must be CAPPED to fit (pass), got %+v", c)
		}
	})

	t.Run("over-cap fixed baskets block", func(t *testing.T) {
		// If the always-on fixed baskets alone exceed the OpenD cap, no SEPA budget
		// can save the session — it would crash-loop at subscribe time. Shrink the
		// cap below the fixed-basket count to exercise the blocker.
		f := healthyProbes()
		f.openDMaxSub = 3 // SPY + XLK + XLF + GLD + GDX = 5 fixed > 3
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckSubscriptionCap)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("over-cap fixed baskets must BLOCK, got %+v", c)
		}
		if r.OK {
			t.Fatal("over-cap subscription set must make the report not OK")
		}
	})

	t.Run("blocks in signal mode too (subscribing happens in all live modes)", func(t *testing.T) {
		f := healthyProbes()
		f.openDMaxSub = 3
		cfg := paperCfg()
		cfg.Mode = "signal"
		r := Run(context.Background(), cfg, f)
		c := findCheck(t, r, CheckSubscriptionCap)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("subscription cap must block even in signal mode, got %+v", c)
		}
		if r.OK {
			t.Fatal("over-cap subscription set must block a signal session (it subscribes too)")
		}
	})

	t.Run("resolve error fails", func(t *testing.T) {
		f := healthyProbes()
		f.resolveErr = errors.New("boom")
		c := findCheck(t, Run(context.Background(), paperCfg(), f), CheckSubscriptionCap)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("a resolve error must fail the cap check, got %+v", c)
		}
	})
}

func TestCheckParamsPromoted(t *testing.T) {
	t.Run("all promoted passes", func(t *testing.T) {
		c := findCheck(t, Run(context.Background(), paperCfg(), healthyProbes()), CheckParamsPromoted)
		if c.Status != StatusPass {
			t.Fatalf("all-promoted must pass, got %+v", c)
		}
	})

	t.Run("baseline warns but does not block", func(t *testing.T) {
		f := healthyProbes()
		f.resolved.Strategies[0].Promoted = false
		f.resolved.Strategies[0].ParamSource = "baseline"
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckParamsPromoted)
		if c.Status != StatusWarn || c.Severity != SeverityWarn {
			t.Fatalf("baseline params must warn (not block), got %+v", c)
		}
		if !r.OK {
			t.Fatal("baseline params must NOT block go-live (warn only)")
		}
	})
}

func TestCheckOpenD(t *testing.T) {
	t.Run("paper unreachable blocks", func(t *testing.T) {
		f := healthyProbes()
		f.opendErr = errors.New("dial tcp: refused")
		r := Run(context.Background(), paperCfg(), f)
		c := findCheck(t, r, CheckOpenD)
		if c.Status != StatusFail || c.Severity != SeverityBlocker {
			t.Fatalf("OpenD down must block paper, got %+v", c)
		}
		if r.OK {
			t.Fatal("OpenD down must make paper report not OK")
		}
	})

	t.Run("signal skips without --check-opend", func(t *testing.T) {
		f := healthyProbes()
		f.opendErr = errors.New("refused")
		cfg := paperCfg()
		cfg.Mode = "signal"
		cfg.CheckOpenD = false
		c := findCheck(t, Run(context.Background(), cfg, f), CheckOpenD)
		if c.Status != StatusSkip {
			t.Fatalf("signal mode without --check-opend must skip OpenD, got %+v", c)
		}
	})

	t.Run("signal with --check-opend warns not blocks", func(t *testing.T) {
		f := healthyProbes()
		f.opendErr = errors.New("refused")
		cfg := paperCfg()
		cfg.Mode = "signal"
		cfg.CheckOpenD = true
		r := Run(context.Background(), cfg, f)
		c := findCheck(t, r, CheckOpenD)
		if c.Status != StatusFail || c.Severity != SeverityWarn {
			t.Fatalf("signal --check-opend must fail at warn severity, got %+v", c)
		}
		if !r.OK {
			t.Fatal("OpenD unreachable must not block a signal session")
		}
	})
}

func TestReport_Helpers(t *testing.T) {
	f := healthyProbes()
	f.frontier[sharadar.DatasetSEP] = calendar.NewDate(2026, time.June, 1)
	f.gap = 7                                 // stale -> blocker fail in paper
	f.resolved.Strategies[0].Promoted = false // baseline -> warn
	f.resolved.Strategies[0].ParamSource = "baseline"
	r := Run(context.Background(), paperCfg(), f)

	if r.OK {
		t.Fatal("expected not OK")
	}
	if len(r.Blockers()) == 0 {
		t.Fatal("expected at least one blocker")
	}
	if len(r.Warnings()) == 0 {
		t.Fatal("expected at least one warning (baseline params)")
	}
}
