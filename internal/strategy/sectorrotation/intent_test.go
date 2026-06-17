package sectorrotation

import (
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

var etfs = []string{"XLB", "XLE", "XLF", "XLI", "XLK", "XLP", "XLU", "XLV", "XLY", "XLRE", "XLC"}

func intentSG(t *testing.T, topK, lookback int) *SignalGenerator {
	t.Helper()
	sg, err := New(Config{
		EquityProvider:   func() float64 { return 100000 },
		Universe:         etfs,
		MomentumLookback: lookback,
		TopK:             topK,
		Timezone:         "America/New_York",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sg
}

// seedHistory seeds each symbol so first-vs-last close yields the desired return
// (lookback 100s then 100*(1+r)).
func seedHistory(t *testing.T, sg *SignalGenerator, returns map[string]float64, lookback int) {
	t.Helper()
	for sym, r := range returns {
		end := 100.0 * (1.0 + r)
		dq := sg.history[sym]
		dq.buf = dq.buf[:0]
		for i := 0; i < lookback; i++ {
			p, _ := domain.PriceFromFloat64(100.0)
			dq.push(p)
		}
		ep, _ := domain.PriceFromFloat64(end)
		dq.push(ep)
		sg.lastClose[sym] = ep
	}
}

func TestStrengthFromRank(t *testing.T) {
	if domain.StrengthFromRank(1, 11) != 100.0 {
		t.Errorf("rank1 = %v", domain.StrengthFromRank(1, 11))
	}
	if domain.StrengthFromRank(11, 11) != 0.0 {
		t.Errorf("rank11 = %v", domain.StrengthFromRank(11, 11))
	}
	mid := domain.StrengthFromRank(6, 11)
	if !(mid > 40 && mid < 60) {
		t.Errorf("mid = %v", mid)
	}
}

func TestEvaluateIntentOnePerETF(t *testing.T) {
	sg := intentSG(t, 3, 5)
	its := sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(its) != len(etfs) {
		t.Fatalf("got %d intents", len(its))
	}
	for _, it := range its {
		if it.StrategyID != domain.StrategyIDSectorRotation {
			t.Errorf("strategy_id = %q", it.StrategyID)
		}
	}
}

func TestEvaluateIntentWarmingUpAllNoSetup(t *testing.T) {
	sg := intentSG(t, 3, 5)
	for _, it := range sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		if it.State != domain.StateNoSetup || it.Rank != 0 {
			t.Errorf("%s: state=%s rank=%d", it.Symbol, it.State, it.Rank)
		}
	}
}

var rankReturns = map[string]float64{
	"XLK": 0.50, "XLE": 0.45, "XLF": 0.40,
	"XLI": 0.10, "XLB": 0.05, "XLP": 0.04, "XLY": 0.03,
	"XLV": 0.02, "XLU": 0.01, "XLRE": -0.01, "XLC": -0.05,
}

func bySymbol(its []domain.SectorRotationIntent) map[string]domain.SectorRotationIntent {
	m := map[string]domain.SectorRotationIntent{}
	for _, it := range its {
		m[it.Symbol] = it
	}
	return m
}

func TestEvaluateIntentTopHeldIsHold(t *testing.T) {
	sg := intentSG(t, 3, 5)
	seedHistory(t, sg, rankReturns, 5)
	for _, s := range []string{"XLK", "XLE", "XLF"} {
		sg.currentPositions[s] = 100
	}
	m := bySymbol(sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
	for _, s := range []string{"XLK", "XLE", "XLF"} {
		if m[s].State != domain.StateHold || m[s].Rank < 1 || m[s].Rank > 3 {
			t.Errorf("%s: state=%s rank=%d", s, m[s].State, m[s].Rank)
		}
	}
	for _, s := range []string{"XLRE", "XLC"} {
		if m[s].State != domain.StateNoSetup {
			t.Errorf("%s: state=%s want no_setup", s, m[s].State)
		}
	}
}

func TestEvaluateIntentPromotedIsBuy(t *testing.T) {
	sg := intentSG(t, 3, 5)
	seedHistory(t, sg, rankReturns, 5)
	for _, s := range []string{"XLY", "XLV", "XLU"} {
		sg.currentPositions[s] = 100
	}
	m := bySymbol(sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
	for _, s := range []string{"XLK", "XLE", "XLF"} {
		if m[s].State != domain.StateBuy {
			t.Errorf("%s: state=%s want buy", s, m[s].State)
		}
	}
}

func TestEvaluateIntentDemotedIsExit(t *testing.T) {
	sg := intentSG(t, 3, 5)
	seedHistory(t, sg, rankReturns, 5)
	for _, s := range []string{"XLY", "XLV", "XLU"} {
		sg.currentPositions[s] = 100
	}
	m := bySymbol(sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
	for _, s := range []string{"XLY", "XLV", "XLU"} {
		if m[s].State != domain.StateExit {
			t.Errorf("%s: state=%s want exit", s, m[s].State)
		}
	}
}

func TestEvaluateIntentJustOutsideTopIsForming(t *testing.T) {
	sg := intentSG(t, 3, 5)
	vals := []float64{0.50, 0.45, 0.40, 0.39, 0.38, 0.01, 0.01, 0.01, 0.01, 0.01, 0.01}
	r := map[string]float64{}
	for i, s := range etfs {
		r[s] = vals[i]
	}
	seedHistory(t, sg, r, 5)
	m := bySymbol(sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))
	// etfs[3], etfs[4] are ranks 4 and 5 -> FORMING (top_k+2 = 5).
	if m[etfs[3]].State != domain.StateForming {
		t.Errorf("%s: state=%s want forming", etfs[3], m[etfs[3]].State)
	}
	if m[etfs[4]].State != domain.StateForming {
		t.Errorf("%s: state=%s want forming", etfs[4], m[etfs[4]].State)
	}
}

func TestEvaluateIntentGenerationMonotonic(t *testing.T) {
	sg := intentSG(t, 3, 5)
	g1 := sg.EvaluateIntent(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))[0].Generation
	g2 := sg.EvaluateIntent(time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC))[0].Generation
	if g2 <= g1 {
		t.Errorf("generation not monotonic: %d -> %d", g1, g2)
	}
}
