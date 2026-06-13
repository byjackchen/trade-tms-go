package portfolio

import (
	"testing"
	"time"
)

func TestSharedContextStateDefaults(t *testing.T) {
	s := NewSharedContextState()
	if s.Regime() != RegimeNeutral {
		t.Fatalf("default regime %q want neutral", s.Regime())
	}
	if _, ok := s.MarketCap("AAPL"); ok {
		t.Fatal("market cap empty by default")
	}
	if s.EarningsBlackout("AAPL") {
		t.Fatal("blackout false (absent) by default")
	}
}

func TestSharedContextStateMutation(t *testing.T) {
	s := NewSharedContextState()
	s.SetRegime(RegimeBull)
	if s.Regime() != RegimeBull {
		t.Fatal("regime not set")
	}
	s.SetMarketCap("AAPL", MustDec("2700000000000.0"))
	v, ok := s.MarketCap("AAPL")
	if !ok || v.Cmp(MustDec("2700000000000.0")) != 0 {
		t.Fatalf("market cap %v ok=%v", v, ok)
	}
	// wholesale replacement.
	s.ReplaceMarketCap(map[string]dec{"MSFT": MustDec("2e12")})
	if _, ok := s.MarketCap("AAPL"); ok {
		t.Fatal("AAPL should be gone after replace")
	}
	s.SetEarningsBlackout("AAPL", true)
	if !s.EarningsBlackout("AAPL") {
		t.Fatal("blackout not set")
	}
}

// buildRising builds n rising SPY bars ending on endDate (one calendar day apart).
func buildRising(n int, endDate time.Time, base float64) []SPYBar {
	bars := make([]SPYBar, n)
	start := endDate.AddDate(0, 0, -(n - 1))
	for i := 0; i < n; i++ {
		bars[i] = SPYBar{Date: start.AddDate(0, 0, i), Close: base + float64(i)}
	}
	return bars
}

func TestContextProviderRegimeWarmupAndLookAhead(t *testing.T) {
	end := mustDate("2021-01-01")
	// 250 rising bars. Provider should classify look-ahead-safe.
	bars := buildRising(250, end, 100)
	p := NewContextProvider(bars, nil, nil, nil, "", 0)

	// As of the 150th bar's date: only 150 bars <= as_of -> < 200 -> neutral.
	asOf150 := bars[149].Date
	if got := p.RegimeAt(asOf150); got != RegimeNeutral {
		t.Fatalf("150-bar as_of -> %q want neutral", got)
	}
	// As of the last bar: 250 bars, rising -> bull.
	if got := p.RegimeAt(bars[249].Date); got != RegimeBull {
		t.Fatalf("full as_of -> %q want bull", got)
	}
}

func TestContextProviderOnBarRegimeTransitions(t *testing.T) {
	end := mustDate("2021-01-01")
	bars := buildRising(250, end, 100)
	p := NewContextProvider(bars, nil, nil, nil, "", 0)
	state := NewSharedContextState()

	// Bar at index 198 (199 bars available) -> < 200 -> no classification, no publish.
	u := p.OnBar(state, bars[198].Date)
	if u.Regime != nil {
		t.Fatal("warmup bar must not publish regime")
	}
	if state.Regime() != RegimeNeutral {
		t.Fatal("warmup must not write regime")
	}
	// Bar at index 199 (200 bars) -> classifies; first classification publishes.
	u = p.OnBar(state, bars[199].Date)
	if u.Regime == nil {
		t.Fatal("first classification must publish")
	}
	first := u.Regime.Value
	if state.Regime() != first {
		t.Fatal("regime must be written to state")
	}
	// Next bar same regime -> no publish (transition-only), but state still written.
	u = p.OnBar(state, bars[200].Date)
	if u.Regime != nil && u.Regime.Value == first {
		t.Fatal("unchanged regime must not re-publish")
	}
}

func TestContextProviderMarketCapDedup(t *testing.T) {
	sf1 := []SF1Row{
		{Ticker: "AAPL", DateKey: mustDate("2024-01-31"), MarketCap: 2.7e12, HasMarketCap: true, Dimension: "MRT", HasDimension: true},
		{Ticker: "AAPL", DateKey: mustDate("2024-05-31"), MarketCap: 2.9e12, HasMarketCap: true, Dimension: "MRT", HasDimension: true},
	}
	p := NewContextProvider(nil, sf1, nil, []string{"AAPL"}, "MRT", 0)
	state := NewSharedContextState()

	// As of Feb 15: only the Jan filing in scope -> publish 2.7e12.
	u := p.OnBar(state, mustDate("2024-02-15"))
	if len(u.MarketCap) != 1 || u.MarketCap[0].ValueDec.Cmp(MustDec("2700000000000.0")) != 0 {
		t.Fatalf("first market cap publish: %+v", u.MarketCap)
	}
	// Same date again -> value unchanged -> no publish.
	u = p.OnBar(state, mustDate("2024-02-16"))
	if len(u.MarketCap) != 0 {
		t.Fatalf("unchanged market cap must not re-publish: %+v", u.MarketCap)
	}
	// After the May filing comes into scope -> new value published.
	u = p.OnBar(state, mustDate("2024-06-01"))
	if len(u.MarketCap) != 1 || u.MarketCap[0].ValueDec.Cmp(MustDec("2900000000000.0")) != 0 {
		t.Fatalf("updated market cap: %+v", u.MarketCap)
	}
	v, _ := state.MarketCap("AAPL")
	if v.Cmp(MustDec("2900000000000.0")) != 0 {
		t.Fatalf("state market cap %s", v.r.FloatString(1))
	}
}

func TestContextProviderEarningsFirstAndTransition(t *testing.T) {
	earn := []EarningsRow{{Ticker: "AAPL", ReportDate: mustDate("2024-06-10")}}
	p := NewContextProvider(nil, nil, earn, []string{"AAPL"}, "", 5)
	state := NewSharedContextState()

	// First observation publishes even when value is false.
	u := p.OnBar(state, mustDate("2024-01-01"))
	if len(u.Earnings) != 1 || u.Earnings[0].Value != false {
		t.Fatalf("first-observation publish (false) expected: %+v", u.Earnings)
	}
	// Same false value again -> no publish.
	u = p.OnBar(state, mustDate("2024-01-02"))
	if len(u.Earnings) != 0 {
		t.Fatalf("unchanged false must not re-publish: %+v", u.Earnings)
	}
	// Transition to true inside the ±5 window (June 6..15).
	u = p.OnBar(state, mustDate("2024-06-08"))
	if len(u.Earnings) != 1 || u.Earnings[0].Value != true {
		t.Fatalf("transition to blackout expected: %+v", u.Earnings)
	}
	if !state.EarningsBlackout("AAPL") {
		t.Fatal("state must reflect blackout")
	}
	// Exit the window -> transition back to false.
	u = p.OnBar(state, mustDate("2024-06-20"))
	if len(u.Earnings) != 1 || u.Earnings[0].Value != false {
		t.Fatalf("transition out of blackout expected: %+v", u.Earnings)
	}
}
