package runs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// TestMetaJSONByteCompatible asserts meta.json is byte-identical to this repo's
// golden artifact layout (api-ws-redis.md §7.1).
func TestMetaJSONByteCompatible(t *testing.T) {
	in := ArtifactInput{
		TS:              "2024-06-13_12-00-00",
		Kind:            "multi-strategy",
		StartDate:       "2024-01-02",
		EndDate:         "2024-12-31",
		StartingBalance: domain.MustMoney("100000.00"),
		FinalBalance:    domain.MustMoney("105247.31"),
		TotalPnL:        domain.MustMoney("5247.31"),
		Strategies:      []string{"Scripted-000"},
	}
	// NOTE: a naive float subtraction would emit total_pnl_usd as
	// 105247.31 - 100000.0 == 5247.309999999998 (IEEE-754 error). The engine
	// computes money in EXACT 1e-4 fixed point, so its total_pnl is 5247.31
	// exactly (api-ws-redis Q2). The surface FORM (trailing .0, shortest digits)
	// is the artifact spec; only the trailing float noise is absent. The
	// byte-equality guarantee therefore holds for every representable value;
	// non-representable float noise is intentionally absent.
	got := string(Marshal(metaObj(in)))
	want := `{
  "version": 1,
  "ts": "2024-06-13_12-00-00",
  "start_date": "2024-01-02",
  "end_date": "2024-12-31",
  "starting_balance_usd": 100000.0,
  "final_balance_usd": 105247.31,
  "total_pnl_usd": 5247.31,
  "strategies": [
    "Scripted-000"
  ],
  "kind": "multi-strategy"
}`
	if got != want {
		t.Errorf("meta.json mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAccountJSONByteCompatible(t *testing.T) {
	hist := []engine.AccountStatePoint{
		{TS: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), BalanceUSD: domain.MustMoney("100000.00")},
		{TS: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), BalanceUSD: domain.MustMoney("105247.31")},
	}
	got := string(Marshal(accountArr(hist)))
	want := `[
  {
    "ts": "2024-01-02T00:00:00+00:00",
    "balance_usd": 100000.0
  },
  {
    "ts": "2024-01-03T00:00:00+00:00",
    "balance_usd": 105247.31
  }
]`
	if got != want {
		t.Errorf("account.json mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteArtifactsLayout(t *testing.T) {
	dir := t.TempDir()
	in := ArtifactInput{
		TS:              "2024-06-13_12-00-00",
		Kind:            "smoke-test",
		StartDate:       "2024-01-02",
		EndDate:         "2024-01-03",
		StartingBalance: domain.MustMoney("100000.00"),
		FinalBalance:    domain.MustMoney("100200.00"),
		TotalPnL:        domain.MustMoney("200.00"),
		Strategies:      []string{"S-000"},
		Orders: []domain.Order{
			domain.NewMarketOrder("c1", "S-000", "AAPL", domain.OrderSideBuy, 100, "buy", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
		},
		Positions: []domain.Position{
			{StrategyID: "S-000", Symbol: "AAPL", SignedQty: 0, AvgPx: 0, RealizedPnL: domain.MustMoney("200.00"), UpdatedAt: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)},
		},
		AccountHistory: []engine.AccountStatePoint{
			{TS: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), BalanceUSD: domain.MustMoney("100000.00")},
		},
		RegimeSamples: map[string]int{"bull": 2},
		StrategyEquity: map[string][]EquityPoint{
			"S-000": {{TS: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), BalanceUSD: domain.MustMoney("200.00")}},
		},
		StrategySummary: map[string]map[string]any{
			"S-000": {"active_count": 1},
		},
	}
	out, err := WriteArtifacts(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"meta.json", "orders.json", "positions.json", "account.json", "regime_samples.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	// strategy_equity present because there is a point.
	if _, err := os.Stat(filepath.Join(out, "strategy_equity", "S-000.json")); err != nil {
		t.Errorf("missing strategy_equity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "strategy_summaries", "S-000.json")); err != nil {
		t.Errorf("missing strategy_summaries: %v", err)
	}
	// No tmp files left behind.
	matches, _ := filepath.Glob(filepath.Join(out, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("leftover tmp files: %v", matches)
	}
}

func TestWriteArtifactsNoEquityDirWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	in := ArtifactInput{
		TS:              "2024-06-13_12-00-00",
		StartDate:       "2024-01-02",
		EndDate:         "2024-01-03",
		StartingBalance: domain.MustMoney("100000.00"),
		FinalBalance:    domain.MustMoney("100000.00"),
		TotalPnL:        0,
		Strategies:      []string{"S-000"},
		StrategyEquity:  map[string][]EquityPoint{"S-000": {}}, // no points
	}
	out, err := WriteArtifacts(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "strategy_equity")); !os.IsNotExist(err) {
		t.Errorf("strategy_equity dir must be absent when no points (got err=%v)", err)
	}
}

func TestSanitizeID(t *testing.T) {
	if got := sanitizeID("AAPL.SIM:foo/bar"); got != "AAPL.SIM_foo_bar" {
		t.Errorf("sanitizeID = %q", got)
	}
}

func mustMoneyFromFloat(t *testing.T, f float64) domain.Money {
	t.Helper()
	m, err := domain.MoneyFromFloat64(f)
	if err != nil {
		t.Fatalf("MoneyFromFloat64(%v): %v", f, err)
	}
	return m
}
