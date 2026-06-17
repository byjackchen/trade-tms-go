package params

// validate_test.go covers the typed-struct validation predicates and messages.
// Tests live in-package to exercise the unexported *FromMap decoders directly.

import (
	"strings"
	"testing"
)

func TestSEPAValidate(t *testing.T) {
	base := SEPAParams{RiskPct: 1, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5, BreakoutVolumeMultiple: 1.5, VCPLookback: 5, HistoryMaxBars: 1000, Timezone: "America/New_York"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid sepa rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*SEPAParams)
		want string
	}{
		{"risk_pct=0", func(p *SEPAParams) { p.RiskPct = 0 }, "risk_pct must be in (0, 100]"},
		{"risk_pct>100", func(p *SEPAParams) { p.RiskPct = 100.1 }, "risk_pct must be in (0, 100]"},
		{"risk_pct=100 ok", func(p *SEPAParams) { p.RiskPct = 100 }, ""},
		{"hard_stop=0", func(p *SEPAParams) { p.HardStopPct = 0 }, "hard_stop_pct must be > 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			err := p.Validate()
			assertErr(t, err, c.want)
		})
	}
}

func TestPairsValidate(t *testing.T) {
	base := PairsParams{Pairs: []Pair{{"KO", "PEP"}}, Lookback: 60, EntryZ: 2.0, ExitZ: 0.5, CapitalPerPairPct: 0.3, Timezone: "America/New_York"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid pairs rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*PairsParams)
		want string
	}{
		{"empty pairs", func(p *PairsParams) { p.Pairs = nil }, "pairs must not be empty"},
		{"lookback<5", func(p *PairsParams) { p.Lookback = 4 }, "lookback must be >= 5"},
		{"lookback=5 ok", func(p *PairsParams) { p.Lookback = 5 }, ""},
		{"entry_z=0", func(p *PairsParams) { p.EntryZ = 0 }, "entry_z must be > 0 and exit_z must be >= 0"},
		{"exit_z<0", func(p *PairsParams) { p.ExitZ = -0.1 }, "entry_z must be > 0 and exit_z must be >= 0"},
		{"exit_z>=entry_z", func(p *PairsParams) { p.ExitZ = 2.0 }, "exit_z must be < entry_z (else no entry/exit gap)"},
		{"cap=0", func(p *PairsParams) { p.CapitalPerPairPct = 0 }, "capital_per_pair_pct must be in (0, 1]"},
		{"cap>1", func(p *PairsParams) { p.CapitalPerPairPct = 1.1 }, "capital_per_pair_pct must be in (0, 1]"},
		{"cap=1 ok", func(p *PairsParams) { p.CapitalPerPairPct = 1.0 }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			assertErr(t, p.Validate(), c.want)
		})
	}
}

func TestSectorRotationValidate(t *testing.T) {
	base := SectorRotationParams{Universe: []string{"A", "B", "C"}, MomentumLookback: 63, TopK: 3, Timezone: "America/New_York"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid sector rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*SectorRotationParams)
		want string
	}{
		{"empty universe", func(p *SectorRotationParams) { p.Universe = nil }, "universe must not be empty"},
		{"momentum<2", func(p *SectorRotationParams) { p.MomentumLookback = 1 }, "momentum_lookback must be >= 2"},
		{"top_k=0", func(p *SectorRotationParams) { p.TopK = 0 }, "top_k must be in [1, 3], got 0"},
		{"top_k>len", func(p *SectorRotationParams) { p.TopK = 4 }, "top_k must be in [1, 3], got 4"},
		{"top_k=len ok", func(p *SectorRotationParams) { p.TopK = 3 }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			assertErr(t, p.Validate(), c.want)
		})
	}
}

func TestIntradayBreakoutValidate(t *testing.T) {
	base := IntradayBreakoutParams{RiskPct: 1, RangeMinutes: 30, VolMultiple: 1.5, ProfitTargetR: 2.0, HardStopPct: 1.0, EODExitTime: "15:55", Timezone: "America/New_York"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid intraday rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*IntradayBreakoutParams)
		want string
	}{
		{"risk=0", func(p *IntradayBreakoutParams) { p.RiskPct = 0 }, "risk_pct must be in (0, 100]"},
		{"range<1", func(p *IntradayBreakoutParams) { p.RangeMinutes = 0 }, "range_minutes must be >= 1"},
		{"vol=0", func(p *IntradayBreakoutParams) { p.VolMultiple = 0 }, "vol_multiple must be > 0"},
		{"target=0", func(p *IntradayBreakoutParams) { p.ProfitTargetR = 0 }, "profit_target_r must be > 0"},
		{"hardstop=0", func(p *IntradayBreakoutParams) { p.HardStopPct = 0 }, "hard_stop_pct must be in (0, 50]"},
		{"hardstop>50", func(p *IntradayBreakoutParams) { p.HardStopPct = 51 }, "hard_stop_pct must be in (0, 50]"},
		{"hardstop=50 ok", func(p *IntradayBreakoutParams) { p.HardStopPct = 50 }, ""},
		{"eod no colon", func(p *IntradayBreakoutParams) { p.EODExitTime = "1555" }, "eod_exit_time must be HH:MM"},
		{"eod non-numeric", func(p *IntradayBreakoutParams) { p.EODExitTime = "aa:bb" }, "eod_exit_time must be HH:MM"},
		{"eod hour 24", func(p *IntradayBreakoutParams) { p.EODExitTime = "24:00" }, "eod_exit_time must be HH:MM"},
		{"eod min 60", func(p *IntradayBreakoutParams) { p.EODExitTime = "15:60" }, "eod_exit_time must be HH:MM"},
		{"eod 00:00 ok", func(p *IntradayBreakoutParams) { p.EODExitTime = "00:00" }, ""},
		{"eod 23:59 ok", func(p *IntradayBreakoutParams) { p.EODExitTime = "23:59" }, ""},
		{"bad tz", func(p *IntradayBreakoutParams) { p.Timezone = "Mars/Phobos" }, `timezone "Mars/Phobos" not recognized`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			assertErr(t, p.Validate(), c.want)
		})
	}
}

// TestIntegerDecodeRejectsNonIntegral ensures an int-typed param with a
// non-whole tuned value is rejected at decode (int params must be integral).
func TestIntegerDecodeRejectsNonIntegral(t *testing.T) {
	m := pmap{"vcp_lookback": 5.5}
	if _, err := m.integer("vcp_lookback"); err == nil || !strings.Contains(err.Error(), "expected integer") {
		t.Fatalf("expected integer-decode error, got %v", err)
	}
}

func assertErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}
}
