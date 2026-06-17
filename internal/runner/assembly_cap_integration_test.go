//go:build integration

package runner_test

// assembly_cap_integration_test.go proves the LIVE-only universe subscription
// cap on runner.Assembler.Assemble (P5 universe-limit). The cap is gated by
// AssemblyInput.SubscriptionCap (the OpenD per-connection quota): when > 0 the
// SEPA stock universe is truncated top-N BY MARKET CAP so the TOTAL distinct
// subscription set (capped SEPA + always-on fixed baskets: SPY + sector ETFs +
// pair legs) fits the quota; when 0 (backtest / hyperopt / EOD) the FULL
// survivor-bias-free universe is assembled (no cap — survivorship-bias-safe).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// seedCapRow inserts an SF1 MRT market cap for a ticker (dated as_of).
func seedCapRow(t *testing.T, pool *pgxpool.Pool, ticker string, cap float64, dk time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO tms.fundamentals_sf1 (ticker, datekey, dimension, marketcap)
		 VALUES ($1, $2, 'MRT', $3)
		 ON CONFLICT DO NOTHING`, ticker, dk, cap)
	require.NoError(t, err)
}

// liveMultiUniverse seeds a >100-name SEPA universe plus the multi fixed baskets
// (SPY + 11 sector ETFs + 6 pair legs) with daily bars and descending SEPA
// market caps (STK0 highest), returning the SEPA names in cap-descending order.
func liveMultiUniverse(t *testing.T, pool *pgxpool.Pool, asOf time.Time, nSepa int) []string {
	t.Helper()
	dates := tradingDates(asOf, 40)

	fixed := append([]string{"SPY"}, sectorETFs...)
	fixed = append(fixed, universe.PairLegTickers()...) // CVX KO MA PEP V XOM
	seedDailyBars(t, pool, fixed, dates)

	sepa := make([]string, nSepa)
	for i := 0; i < nSepa; i++ {
		// Zero-padded so lexical order != cap order (the cap must sort by CAP,
		// not by name): STK000 has the HIGHEST cap.
		sepa[i] = "STK" + pad3(i)
	}
	seedDailyBars(t, pool, sepa, dates)
	dk := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	for i, tk := range sepa {
		seedCapRow(t, pool, tk, float64(nSepa-i)*1e9, dk) // STK000 highest, descending
	}
	return sepa // already cap-descending (STK000 first)
}

func pad3(n int) string {
	s := itoa3(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func itoa3(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func contains(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// TestLiveAssemblyCapsSubscriptionSet is the core fix proof: a multi live
// assembly over a 150-name SEPA universe + fixed baskets caps the distinct
// subscription set to <= the OpenD quota (100), keeps EVERY fixed basket, and
// admits the SEPA names TOP-N BY MARKET CAP (deterministic).
func TestLiveAssemblyCapsSubscriptionSet(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	asOf := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	sepaByCap := liveMultiUniverse(t, pool, asOf, 150) // 150 > the 100 OpenD cap

	asm := runner.NewAssembler(pool, "")
	end := calendar.NewDate(2024, time.March, 15)
	start := end.AddDays(-60)

	const openDCap = moomoo.DefaultMaxSubscriptions    // 100
	const envLimit = universe.DefaultLiveUniverseLimit // 85

	as, err := asm.Assemble(ctx, runner.AssemblyInput{
		Strategy:        "multi",
		Tickers:         sepaByCap,
		StartingBalance: 100000,
		SubscriptionCap: openDCap,
		UniverseLimit:   envLimit,
	}, start, end)
	require.NoError(t, err)

	// (1) The total distinct subscription set fits the OpenD cap (strictly under,
	// the shared helper reserves universe.SubscriptionSafetyMargin).
	assert.LessOrEqual(t, len(as.Tickers), openDCap,
		"capped live subscription set must fit the OpenD per-connection cap")
	assert.LessOrEqual(t, len(as.Tickers), openDCap-universe.SubscriptionSafetyMargin,
		"the set stays under the hard cap by the safety margin")

	// (2) EVERY fixed basket is present (always subscribed, never capped).
	fixed := append([]string{"SPY"}, sectorETFs...)
	fixed = append(fixed, universe.PairLegTickers()...)
	for _, f := range fixed {
		assert.True(t, contains(as.Tickers, f), "fixed basket %s must be subscribed", f)
	}

	// (3) The admitted SEPA names are the TOP-N BY MARKET CAP, deterministically:
	// STK000 (highest cap) must be in; a low-cap tail name must be out.
	assert.True(t, contains(as.Tickers, "STK000"), "highest-market-cap SEPA name must be admitted")
	assert.False(t, contains(as.Tickers, sepaByCap[len(sepaByCap)-1]),
		"the lowest-cap SEPA name must be dropped by the cap")

	// (4) The SEPA count honours BOTH the env top-N (85) and the OpenD-fit budget:
	// fixed = 18 distinct, budget = 100-5-18 = 77 SEPA slots, env clamps to 85 (>77
	// so non-binding here) -> 77 SEPA admitted, total = 95.
	sepaAdmitted := 0
	fixedSet := map[string]struct{}{}
	for _, f := range fixed {
		fixedSet[f] = struct{}{}
	}
	for _, tk := range as.Tickers {
		if _, isFixed := fixedSet[tk]; !isFixed {
			sepaAdmitted++
		}
	}
	assert.LessOrEqual(t, sepaAdmitted, envLimit, "SEPA count never exceeds the env top-N limit")
	assert.Positive(t, sepaAdmitted, "some top-cap SEPA names are admitted")
	t.Logf("capped multi live subscription set: %d total (%d fixed + %d SEPA), cap %d",
		len(as.Tickers), len(fixed), sepaAdmitted, openDCap)
}

// promoteSectorUniverse points active_params.sector_rotation at a promoted
// param_set whose `universe` JSONB field is the given (expanded) ETF list, so the
// live assembly resolves THIS universe as the sector fixed basket — exactly the
// operator action (promoting a sector_rotation param_set) the cap-divergence
// finding describes.
func promoteSectorUniverse(t *testing.T, pool *pgxpool.Pool, etfs []string, lookback, topK int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	quoted := make([]string, len(etfs))
	for i, e := range etfs {
		quoted[i] = fmt.Sprintf("%q", e)
	}
	// A FULL params document (the shape the DB reader + params resolver expect):
	// strategy / schema_version / parameters{<name>:{default,type}}.
	payload := fmt.Sprintf(`{
		"strategy": "sector_rotation",
		"schema_version": 1,
		"metadata": {"source": "manual"},
		"parameters": {
			"momentum_lookback": {"default": %d, "type": "int"},
			"top_k": {"default": %d, "type": "int"},
			"universe": {"default": [%s], "type": "list"},
			"timezone": {"default": "America/New_York", "type": "str"}
		}
	}`, lookback, topK, strings.Join(quoted, ","))
	var psID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO tms.param_sets (strategy, version, schema_version, source, payload)
		VALUES ('sector_rotation', 1, 1, 'manual', $1::jsonb)
		RETURNING id`, payload).Scan(&psID))
	_, err := pool.Exec(ctx, `
		INSERT INTO tms.active_params (strategy, param_set_id, source_id, promoted_by, promoted_at)
		VALUES ('sector_rotation', $1, 'external', 'test', now())
		ON CONFLICT (strategy) DO UPDATE SET param_set_id = EXCLUDED.param_set_id`, psID)
	require.NoError(t, err)
}

// TestLiveCapReservesPromotedFixedBaskets is the regression proof for the
// preflight/live cap-divergence finding: when an operator promotes a sector_rotation
// param_set whose universe EXPANDS the fixed basket well beyond the 11 hardcoded
// default ETFs, the live cap must reserve slots for the PROMOTED (params-resolved)
// baskets the session actually subscribes — NOT the smaller hardcoded defaults.
// Before the fix, capSEPAUniverse reserved against the 18 default fixed symbols and
// over-admitted SEPA, so the distinct subscription set exceeded the OpenD cap (the
// crash-loop) while preflight (params-accurate) passed green. After the fix the live
// set is sized against the same expanded baskets and stays under the cap.
func TestLiveCapReservesPromotedFixedBaskets(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	truncateParams := func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := pool.Exec(c,
			`TRUNCATE tms.active_params, tms.param_sets, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
	}
	truncateParams()
	// Promoted params + caps must not leak into sibling tests (same DB, same binary).
	t.Cleanup(truncateParams)

	asOf := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	dates := tradingDates(asOf, 40)

	// A 25-ETF promoted sector universe (the finding's scenario): 11 default ETFs
	// plus 14 synthetic sector ETFs. With SPY + 6 pair legs this is 32 distinct
	// fixed symbols — far above the 18 hardcoded defaults.
	expandedETFs := append([]string(nil), sectorETFs...)
	for i := 0; i < 14; i++ {
		expandedETFs = append(expandedETFs, "ETF"+pad3(i))
	}
	fixed := append([]string{"SPY"}, expandedETFs...)
	fixed = append(fixed, universe.PairLegTickers()...) // CVX KO MA PEP V XOM
	seedDailyBars(t, pool, fixed, dates)
	promoteSectorUniverse(t, pool, expandedETFs, 20, 3)

	// 150-name SEPA universe, descending caps (STK000 highest).
	sepaByCap := make([]string, 150)
	for i := range sepaByCap {
		sepaByCap[i] = "STK" + pad3(i)
	}
	seedDailyBars(t, pool, sepaByCap, dates)
	dk := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	for i, tk := range sepaByCap {
		seedCapRow(t, pool, tk, float64(len(sepaByCap)-i)*1e9, dk)
	}

	asm := runner.NewAssembler(pool, "")
	end := calendar.NewDate(2024, time.March, 15)
	start := end.AddDays(-60)

	const openDCap = moomoo.DefaultMaxSubscriptions // 100

	as, err := asm.Assemble(ctx, runner.AssemblyInput{
		Strategy:        "multi",
		Tickers:         sepaByCap,
		StartingBalance: 100000,
		SubscriptionCap: openDCap,
		UniverseLimit:   universe.DefaultLiveUniverseLimit,
	}, start, end)
	require.NoError(t, err)

	// (1) The cap holds DESPITE the expanded fixed baskets: total distinct set fits
	// the OpenD cap minus the safety margin (the bug would have produced > 100 here).
	assert.LessOrEqual(t, len(as.Tickers), openDCap,
		"capped set must fit the OpenD cap even with a promoted (expanded) sector universe")
	assert.LessOrEqual(t, len(as.Tickers), openDCap-universe.SubscriptionSafetyMargin)

	// (2) EVERY promoted fixed-basket symbol is subscribed (never dropped by the cap).
	for _, f := range fixed {
		assert.True(t, contains(as.Tickers, f),
			"promoted fixed-basket symbol %s must be subscribed", f)
	}

	// (3) The SEPA count was sized against the EXPANDED fixed count: 32 distinct
	// fixed + safety margin 5 => budget 100-5 = 95, sepaSlot = 95-32 = 63 SEPA. The
	// admitted SEPA must be <= that (and strictly fewer than the 77 a default-18
	// reservation would have wrongly admitted), proving the reservation tracked the
	// promoted baskets.
	fixedSet := map[string]struct{}{}
	for _, f := range fixed {
		fixedSet[f] = struct{}{}
	}
	sepaAdmitted := 0
	for _, tk := range as.Tickers {
		if _, isFixed := fixedSet[tk]; !isFixed {
			sepaAdmitted++
		}
	}
	assert.LessOrEqual(t, len(fixed)+sepaAdmitted, openDCap-universe.SubscriptionSafetyMargin)
	assert.Less(t, sepaAdmitted, 77,
		"SEPA admitted must reflect the EXPANDED fixed reservation, not the smaller default")
	t.Logf("promoted-basket capped set: %d total (%d fixed + %d SEPA), cap %d",
		len(as.Tickers), len(fixed), sepaAdmitted, openDCap)
}

// TestBacktestEODAssemblyKeepsFullUniverse proves the cap is LIVE-ONLY: the SAME
// 150-name universe assembled with SubscriptionCap=0 (the backtest / hyperopt /
// EOD path) keeps ALL SEPA names — no survivorship-bias-reintroducing cap.
func TestBacktestEODAssemblyKeepsFullUniverse(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	asOf := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	sepaByCap := liveMultiUniverse(t, pool, asOf, 150)

	asm := runner.NewAssembler(pool, "")
	end := calendar.NewDate(2024, time.March, 15)
	start := end.AddDays(-60)

	as, err := asm.Assemble(ctx, runner.AssemblyInput{
		Strategy:        "multi",
		Tickers:         sepaByCap,
		StartingBalance: 100000,
		// SubscriptionCap: 0 -> NO cap (full survivor-bias-free universe).
	}, start, end)
	require.NoError(t, err)

	// ALL 150 SEPA names must survive (no cap), so the assembled set is the full
	// universe plus the fixed baskets — far above the OpenD cap (proving the EOD/
	// backtest path is unaffected by the live cap).
	for _, tk := range sepaByCap {
		assert.True(t, contains(as.Tickers, tk), "uncapped assembly must keep SEPA name %s", tk)
	}
	assert.Greater(t, len(as.Tickers), moomoo.DefaultMaxSubscriptions,
		"uncapped assembly keeps the full universe (> the OpenD cap — backtest/hyperopt path)")
	t.Logf("uncapped (backtest/EOD) assembly: %d total (full %d-name universe + fixed)",
		len(as.Tickers), len(sepaByCap))
}
