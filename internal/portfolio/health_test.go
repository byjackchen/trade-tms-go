package portfolio

import (
	"encoding/json"
	"os"
	"testing"
)

// helper: build a 2-strategy portfolio with the given daily-loss-halt pct.
func healthPortfolio(t *testing.T, haltPct float64) *Portfolio {
	t.Helper()
	alloc, err := NewAllocator([]StrategyAllocation{{"S1", 0.5}, {"S2", 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := NewRiskConstraints(RiskConstraintsConfig{
		MaxSingleNamePct: 0.20, ConcentrationPct: 0.30, DailyLossHaltPct: haltPct,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewPortfolio(alloc, rc)
}

// TestHealthSnapshotParity replays golden PortfolioHealthSnapshot values captured
// from the Python reference (incl. non-terminating 28-sig-digit divisions) and
// asserts the Go snapshot is numerically identical for every field.
func TestHealthSnapshotParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/health_parity.json")
	if err != nil {
		t.Skipf("health parity fixture missing (%v)", err)
	}
	var cases []struct {
		Name      string `json:"name"`
		NAV       string `json:"nav"`
		Realized  string `json:"realized"`
		Unreal    string `json:"unrealized"`
		Halt      float64
		Positions []struct {
			Strat string `json:"strat"`
			Sym   string `json:"sym"`
			Qty   int64  `json:"qty"`
		} `json:"positions"`
		LastClose map[string]string `json:"last_close"`
		Want      struct {
			DayPnL    string `json:"day_pnl"`
			DayPnLPct string `json:"day_pnl_pct"`
			Halt      bool   `json:"daily_loss_halt"`
			Headroom  string `json:"halt_headroom_pct"`
			Conc      string `json:"concentration_pct"`
		} `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode: %v", err)
	}
	eq := func(t *testing.T, field string, got dec, want string) {
		w := MustDec(want)
		if got.Cmp(w) != 0 {
			t.Errorf("%s: got %s want %s", field, got.r.FloatString(34), want)
		}
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			pf := healthPortfolio(t, c.Halt)
			positions := map[PositionKey]int64{}
			for _, p := range c.Positions {
				positions[PositionKey{p.Strat, p.Sym}] = p.Qty
			}
			lastClose := map[string]dec{}
			for k, v := range c.LastClose {
				lastClose[k] = MustDec(v)
			}
			acct := AccountSnapshot{
				NAV: MustDec(c.NAV), Cash: MustDec(c.NAV),
				RealizedPnLToday: MustDec(c.Realized), UnrealizedPnLToday: MustDec(c.Unreal),
				Positions: positions, LastClose: lastClose,
			}
			s := pf.HealthSnapshot(acct)
			eq(t, "day_pnl", s.DayPnL, c.Want.DayPnL)
			eq(t, "day_pnl_pct", s.DayPnLPct, c.Want.DayPnLPct)
			if s.DailyLossHalt != c.Want.Halt {
				t.Errorf("daily_loss_halt: got %v want %v", s.DailyLossHalt, c.Want.Halt)
			}
			eq(t, "halt_headroom_pct", s.HaltHeadroomPct, c.Want.Headroom)
			eq(t, "concentration_pct", s.ConcentrationPct, c.Want.Conc)
		})
	}
}

// TestHealthSnapshotHandDerived re-derives the boundary cases by hand.
func TestHealthSnapshotHandDerived(t *testing.T) {
	pf := healthPortfolio(t, 0.05) // threshold = -5% NAV

	// nav 100k, pnl exactly -5000 == threshold -> NOT halted (strict <).
	atBoundary := pf.HealthSnapshot(AccountSnapshot{
		NAV: MustDec("100000"), RealizedPnLToday: MustDec("-5000"), UnrealizedPnLToday: decZero(),
	})
	if atBoundary.DailyLossHalt {
		t.Fatal("pnl exactly at -5% must NOT halt (strict <)")
	}
	if atBoundary.HaltHeadroomPct.Cmp(decZero()) != 0 {
		t.Fatalf("headroom at boundary must be 0, got %s", atBoundary.HaltHeadroomPct)
	}

	// one share below -> halted.
	below := pf.HealthSnapshot(AccountSnapshot{
		NAV: MustDec("100000"), RealizedPnLToday: MustDec("-5000.01"), UnrealizedPnLToday: decZero(),
	})
	if !below.DailyLossHalt {
		t.Fatal("pnl below -5% must halt")
	}
	if below.HaltHeadroomPct.Cmp(decZero()) != 0 {
		t.Fatal("headroom must clamp to 0 when halted")
	}

	// headroom: pnl=+100, threshold=-5000, nav=100000 -> (100 - -5000)/100000 = 0.051.
	hr := pf.HealthSnapshot(AccountSnapshot{
		NAV: MustDec("100000"), RealizedPnLToday: MustDec("100"), UnrealizedPnLToday: decZero(),
	})
	if hr.HaltHeadroomPct.Cmp(MustDec("0.051")) != 0 {
		t.Fatalf("headroom: got %s want 0.051", hr.HaltHeadroomPct)
	}

	// concentration NET not gross: +100/-100 AAPL -> 0; NVDA 100@500/200k = 0.25.
	conc := pf.HealthSnapshot(AccountSnapshot{
		NAV: MustDec("200000"),
		Positions: map[PositionKey]int64{
			{"S1", "AAPL"}: 100, {"S2", "AAPL"}: 50, {"S1", "NVDA"}: 100,
		},
		LastClose: map[string]dec{"AAPL": MustDec("200"), "NVDA": MustDec("500")},
	})
	if conc.ConcentrationPct.Cmp(MustDec("0.25")) != 0 {
		t.Fatalf("concentration: got %s want 0.25", conc.ConcentrationPct)
	}

	// nav <= 0 -> all ratios zero, no halt.
	zeroNav := pf.HealthSnapshot(AccountSnapshot{NAV: decZero(), RealizedPnLToday: MustDec("100")})
	if zeroNav.DayPnLPct.Cmp(decZero()) != 0 || zeroNav.HaltHeadroomPct.Cmp(decZero()) != 0 {
		t.Fatal("nav<=0 must zero the ratios")
	}
	if zeroNav.DailyLossHalt {
		// threshold = -0*pct = 0; pnl=100 not < 0 -> not halted.
		t.Fatal("nav 0 with positive pnl must not halt")
	}
}
