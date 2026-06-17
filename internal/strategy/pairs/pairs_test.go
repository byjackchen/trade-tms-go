package pairs

// pairs_test.go: targeted unit tests for behavior not fully exercised by the
// golden replay — config validation (order + messages, spec §4.2), state_dict /
// load_state round-trip (spec §12), warmup / degenerate / std==0 guards
// (spec §7), shared-symbol semantics (spec §4.3), sizing edge cases (spec §8),
// the look-ahead sync guard (spec §6), and the pyDecimalStr bridge.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func baseCfg() Config {
	return Config{
		EquityProvider:    ConstantEquity(100000),
		Pairs:             DefaultPairs(),
		Lookback:          60,
		EntryZ:            2.0,
		ExitZ:             0.5,
		CapitalPerPairPct: 0.30,
		Timezone:          "America/New_York",
	}
}

func bar(sym, date string, close string) domain.Bar {
	ts, _ := time.Parse("2006-01-02", date)
	p := domain.MustPrice(close)
	return domain.Bar{Symbol: sym, TS: ts.UTC(), Open: p, High: p, Low: p, Close: p, Volume: 1}
}

func TestConfigValidationOrderAndMessages(t *testing.T) {
	// Rule 1: nil provider -> type error (distinct sentinel).
	c := baseCfg()
	c.EquityProvider = nil
	if err := c.Validate(); err == nil || !errors.Is(err, ErrConfigType) ||
		!strings.Contains(err.Error(), "equity_provider") {
		t.Fatalf("rule1: got %v", err)
	}
	// Rule 2: empty pairs.
	c = baseCfg()
	c.Pairs = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "pairs must not be empty") {
		t.Fatalf("rule2: got %v", err)
	}
	// Rule 3: lookback < 5.
	c = baseCfg()
	c.Lookback = 4
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "lookback") {
		t.Fatalf("rule3: got %v", err)
	}
	// Rule 4: entry_z <= 0 OR exit_z < 0. exit_z == 0 is LEGAL.
	c = baseCfg()
	c.EntryZ = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "entry_z must be > 0") {
		t.Fatalf("rule4a: got %v", err)
	}
	c = baseCfg()
	c.ExitZ = -0.1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "exit_z must be >= 0") {
		t.Fatalf("rule4b: got %v", err)
	}
	c = baseCfg()
	c.ExitZ = 0.0 // legal
	if err := c.Validate(); err != nil {
		t.Fatalf("exit_z==0 must be legal: %v", err)
	}
	// Rule 5: exit_z >= entry_z.
	c = baseCfg()
	c.ExitZ = 2.0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "exit_z must be < entry_z") {
		t.Fatalf("rule5: got %v", err)
	}
	// Rule 6: capital_per_pair_pct outside (0,1].
	c = baseCfg()
	c.CapitalPerPairPct = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "capital_per_pair_pct") {
		t.Fatalf("rule6a: got %v", err)
	}
	c = baseCfg()
	c.CapitalPerPairPct = 1.5
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "capital_per_pair_pct") {
		t.Fatalf("rule6b: got %v", err)
	}
	// Provider must NOT be invoked during validation.
	called := false
	c = baseCfg()
	c.EquityProvider = func() float64 { called = true; return 1 }
	_ = c.Validate()
	if called {
		t.Fatal("equity_provider invoked during validation")
	}
}

func TestUnknownSymbolIgnored(t *testing.T) {
	g, _ := New(baseCfg())
	if sigs := g.OnDomainBar(bar("AAPL", "2020-01-02", "100")); sigs != nil {
		t.Fatalf("unknown symbol should yield no signals, no state: %v", sigs)
	}
	if _, ok := g.lastBarDate["AAPL"]; ok {
		t.Fatal("unknown symbol mutated state")
	}
}

func TestSyncGuardSingleLeg(t *testing.T) {
	g, _ := New(baseCfg())
	// Stream only KO for many days: never in-sync -> never evaluates.
	for i := 0; i < 100; i++ {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		b := domain.Bar{Symbol: "KO", TS: d, Close: domain.MustPrice("100"), Open: domain.MustPrice("100"), High: domain.MustPrice("100"), Low: domain.MustPrice("100")}
		if sigs := g.OnDomainBar(b); sigs != nil {
			t.Fatalf("single-leg stream produced signals at i=%d", i)
		}
	}
}

func TestWarmupGate(t *testing.T) {
	cfg := baseCfg()
	cfg.Lookback = 5
	cfg.Pairs = []Pair{{LongLeg: "KO", ShortLeg: "PEP"}}
	g, _ := New(cfg)
	// Feed 4 synced days (< lookback) -> warmup, no signals.
	for i := 0; i < 4; i++ {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		for _, sym := range []string{"KO", "PEP"} {
			p := domain.MustPrice("100")
			sigs := g.OnDomainBar(domain.Bar{Symbol: sym, TS: d, Open: p, High: p, Low: p, Close: p})
			if sigs != nil {
				t.Fatalf("warmup produced signals at day %d sym %s", i, sym)
			}
		}
	}
}

func TestStdZeroNoSignal(t *testing.T) {
	cfg := baseCfg()
	cfg.Lookback = 5
	cfg.Pairs = []Pair{{LongLeg: "KO", ShortLeg: "PEP"}}
	g, _ := New(cfg)
	// Constant prices -> spread is constant -> std==0 -> no signals, telemetry
	// not updated (current_z stays nil).
	for i := 0; i < 10; i++ {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		for _, sym := range []string{"KO", "PEP"} {
			p := domain.MustPrice("100")
			if sigs := g.OnDomainBar(domain.Bar{Symbol: sym, TS: d, Open: p, High: p, Low: p, Close: p}); sigs != nil {
				t.Fatalf("std==0 produced signals")
			}
		}
	}
	sum := g.StateSummary()["pairs"][0]
	if sum.CurrentZ != nil || sum.CurrentBeta != nil {
		t.Fatalf("telemetry should be nil when std==0 / degenerate: z=%v beta=%v", sum.CurrentZ, sum.CurrentBeta)
	}
}

func TestSizingFloorAndEquityLivePull(t *testing.T) {
	// Pin the documented sizing example (spec §8): equity 100000, pct 0.30 ->
	// 15000 per leg; price 97.5 -> 153; price 120 -> 125.
	cfg := baseCfg()
	cfg.Pairs = []Pair{{LongLeg: "KO", ShortLeg: "PEP"}}
	g, _ := New(cfg)
	g.lastClose["KO"] = domain.MustPrice("97.5")
	g.lastClose["PEP"] = domain.MustPrice("120")
	lq, sq := g.computeLegQuantities(Pair{LongLeg: "KO", ShortLeg: "PEP"})
	if lq != 153 || sq != 125 {
		t.Fatalf("sizing got (%d,%d) want (153,125)", lq, sq)
	}
	// Live equity pull: equity is re-read every call (no caching). 100000 ->
	// 15000/97.5 = floor 153; 200000 -> 30000/97.5 = floor 307 (floor drift,
	// spec I-5 — NOT exactly 2x due to independent flooring).
	eq := 100000.0
	g.cfg.EquityProvider = func() float64 { return eq }
	lq1, _ := g.computeLegQuantities(Pair{LongLeg: "KO", ShortLeg: "PEP"})
	eq = 200000.0
	lq2, _ := g.computeLegQuantities(Pair{LongLeg: "KO", ShortLeg: "PEP"})
	if lq1 != 153 || lq2 != 307 {
		t.Fatalf("live equity pull: got (%d,%d) want (153,307)", lq1, lq2)
	}
	// Missing/zero price aborts entry (0,0).
	g.lastClose["KO"] = 0
	if a, b := g.computeLegQuantities(Pair{LongLeg: "KO", ShortLeg: "PEP"}); a != 0 || b != 0 {
		t.Fatalf("zero price should abort sizing, got (%d,%d)", a, b)
	}
}

func TestStateDictLoadStateRoundTrip(t *testing.T) {
	cfg := baseCfg()
	cfg.Lookback = 10
	cfg.Pairs = []Pair{{LongLeg: "KO", ShortLeg: "PEP"}}
	g, _ := New(cfg)
	// Drive enough synced bars with varied prices to open a position.
	prices := []string{"100.5", "101.2", "99.8", "102.3", "98.4", "103.7", "97.1", "104.9", "96.0", "105.5", "95.0", "106.0", "94.0"}
	for i, px := range prices {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		g.OnDomainBar(bar("KO", d.Format("2006-01-02"), px))
		// PEP moves opposite to force a spread divergence.
		ppx := prices[len(prices)-1-i]
		g.OnDomainBar(bar("PEP", d.Format("2006-01-02"), ppx))
	}
	sd := g.StateDict()
	raw, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sd2 StateDict
	if err := json.Unmarshal(raw, &sd2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	g2, _ := New(cfg)
	if err := g2.LoadState(sd2); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Round-trip invariant: pair_state, leg_position, last_close, history equal.
	sd3 := g2.StateDict()
	a, _ := json.Marshal(sd.PairState)
	b, _ := json.Marshal(sd3.PairState)
	if string(a) != string(b) {
		t.Fatalf("pair_state drift:\n%s\n%s", a, b)
	}
	a, _ = json.Marshal(sd.LegPosition)
	b, _ = json.Marshal(sd3.LegPosition)
	if string(a) != string(b) {
		t.Fatalf("leg_position drift:\n%s\n%s", a, b)
	}
	a, _ = json.Marshal(sd.History)
	b, _ = json.Marshal(sd3.History)
	if string(a) != string(b) {
		t.Fatalf("history drift:\n%s\n%s", a, b)
	}
	a, _ = json.Marshal(sd.LastClose)
	b, _ = json.Marshal(sd3.LastClose)
	if string(a) != string(b) {
		t.Fatalf("last_close drift:\n%s\n%s", a, b)
	}
	// No legacy account_size key anywhere in config.
	if strings.Contains(string(raw), "account_size") {
		t.Fatal("state_dict must not contain account_size")
	}
}

func TestSplitFirstPipe(t *testing.T) {
	// pair_state keys split on FIRST '|' (symbols with '|' are pathological but
	// the rule is positional).
	l, s := splitFirstPipe("KO|PEP")
	if l != "KO" || s != "PEP" {
		t.Fatalf("got (%q,%q)", l, s)
	}
	l, s = splitFirstPipe("A|B|C")
	if l != "A" || s != "B|C" {
		t.Fatalf("first-pipe split got (%q,%q)", l, s)
	}
}

func TestPyDecimalStr(t *testing.T) {
	cases := map[string]string{
		"120.46": "120.46",
		"120.6":  "120.6",
		"58":     "58.0", // integer-valued close -> ".0" surface form
		"250.12": "250.12",
	}
	for in, want := range cases {
		p := domain.MustPrice(in)
		if got := pyDecimalStr(p); got != want {
			t.Fatalf("pyDecimalStr(%s)=%q want %q", in, got, want)
		}
	}
}

func TestRingEvictionCapLookbackPlus1(t *testing.T) {
	r := newPriceRing(3)
	for i, s := range []string{"1", "2", "3", "4", "5"} {
		r.append(domain.MustPrice(s), s)
		_ = i
	}
	if r.len() != 3 {
		t.Fatalf("ring len %d want 3", r.len())
	}
	if r.strs[0] != "3" || r.strs[2] != "5" {
		t.Fatalf("ring eviction wrong: %v", r.strs)
	}
}

func TestSharedSymbolOneHistory(t *testing.T) {
	// Two pairs sharing KO must share ONE history ring and ONE leg slot
	// (set-if-absent; spec §4.3, I-2).
	cfg := baseCfg()
	cfg.Pairs = []Pair{{LongLeg: "KO", ShortLeg: "PEP"}, {LongLeg: "KO", ShortLeg: "MO"}}
	g, _ := New(cfg)
	rKOPEP := g.history["KO"]
	if rKOPEP == nil {
		t.Fatal("KO has no history")
	}
	// Append via one pair's identity; the other sees the same buffer.
	g.OnDomainBar(bar("KO", "2020-01-02", "100"))
	if g.history["KO"].len() != 1 {
		t.Fatalf("shared KO history len %d", g.history["KO"].len())
	}
}

func TestIntentGenerationMonotonic(t *testing.T) {
	g, _ := New(baseCfg())
	ts := time.Now().UTC()
	first := g.EvaluateIntent(ts)
	if first[0].Generation != 1 {
		t.Fatalf("first generation %d want 1", first[0].Generation)
	}
	second := g.EvaluateIntent(ts)
	if second[0].Generation != 2 {
		t.Fatalf("second generation %d want 2", second[0].Generation)
	}
	// 2N intents for N pairs.
	if len(first) != 2*len(g.cfg.Pairs) {
		t.Fatalf("intent count %d want %d", len(first), 2*len(g.cfg.Pairs))
	}
}
