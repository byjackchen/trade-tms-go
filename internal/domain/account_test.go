package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func snapshotFixture() PortfolioSnapshot {
	return NewPortfolioSnapshot(
		MustMoney("100000"), MustMoney("100000"), 0, 0,
		map[StrategySymbol]Qty{
			{"SEPARunner-000", "AAPL"}:  150,
			{"SEPARunner-000", "MSFT"}:  -40, // shorts count gross via |qty|
			{"SEPARunner-000", "NOPX"}:  10,  // no last_close → invisible to budget
			{"SEPARunner-000", "FLAT0"}: 0,   // zero entry skipped
			{"PairsRunner-000", "AAPL"}: -60,
			{"PairsRunner-000", "KO"}:   200,
		},
		map[string]Price{
			"AAPL":  MustPrice("101.50"),
			"MSFT":  MustPrice("400.00"),
			"KO":    MustPrice("60.25"),
			"FLAT0": MustPrice("1.00"),
		},
	)
}

func TestPortfolioSnapshotDerived(t *testing.T) {
	a := snapshotFixture()

	// strategy_position: lookup (sid, sym), missing -> 0.
	if got := a.StrategyPosition("SEPARunner-000", "AAPL"); got != 150 {
		t.Errorf("StrategyPosition = %d", got)
	}
	if got := a.StrategyPosition("SEPARunner-000", "TSLA"); got != 0 {
		t.Errorf("missing position must be 0, got %d", got)
	}
	if got := a.StrategyPosition("Ghost-999", "AAPL"); got != 0 {
		t.Errorf("missing strategy must be 0, got %d", got)
	}

	// net_position_across_strategies: sum over all strategies.
	if net, err := a.NetPositionAcrossStrategies("AAPL"); err != nil || net != 90 {
		t.Errorf("net AAPL = %d, %v; want 150 + (-60) = 90", net, err)
	}
	if net, err := a.NetPositionAcrossStrategies("KO"); err != nil || net != 200 {
		t.Errorf("net KO = %d, %v", net, err)
	}
	if net, err := a.NetPositionAcrossStrategies("UNKNOWN"); err != nil || net != 0 {
		t.Errorf("net UNKNOWN = %d, %v", net, err)
	}

	// gross_exposure_for_strategy: Σ |qty| * last_close.get(sym, 0):
	// 150*101.50 + 40*400.00 + (NOPX: no price → 0) + (FLAT0:
	// qty 0 → skipped) = 15225 + 16000 = 31225.
	if g, err := a.GrossExposureForStrategy("SEPARunner-000"); err != nil || g != MustMoney("31225") {
		t.Errorf("gross SEPA = %s, %v; want 31225", g, err)
	}
	// Pairs: 60*101.50 + 200*60.25 = 6090 + 12050 = 18140 (gross, not net).
	if g, err := a.GrossExposureForStrategy("PairsRunner-000"); err != nil || g != MustMoney("18140") {
		t.Errorf("gross Pairs = %s, %v; want 18140", g, err)
	}
	if g, err := a.GrossExposureForStrategy("Ghost-999"); err != nil || g != 0 {
		t.Errorf("gross unknown strategy = %s, %v; want 0", g, err)
	}

	// total_pnl_today = realized + unrealized.
	b := a
	b.RealizedPnLToday = MustMoney("-120.50")
	b.UnrealizedPnLToday = MustMoney("45.25")
	if pnl, err := b.TotalPnLToday(); err != nil || pnl != MustMoney("-75.25") {
		t.Errorf("TotalPnLToday = %s, %v", pnl, err)
	}
}

func TestPortfolioSnapshotOverflow(t *testing.T) {
	a := NewPortfolioSnapshot(0, 0, math.MaxInt64, math.MaxInt64, nil, nil)
	if _, err := a.TotalPnLToday(); !errors.Is(err, ErrOverflow) {
		t.Error("TotalPnLToday overflow not detected")
	}

	b := NewPortfolioSnapshot(0, 0, 0, 0,
		map[StrategySymbol]Qty{
			{"S", "X"}:  math.MaxInt64,
			{"S2", "X"}: 1,
		},
		map[string]Price{"X": MustPrice("2")},
	)
	if _, err := b.NetPositionAcrossStrategies("X"); !errors.Is(err, ErrOverflow) {
		t.Error("NetPositionAcrossStrategies overflow not detected")
	}
	if _, err := b.GrossExposureForStrategy("S"); !errors.Is(err, ErrOverflow) {
		t.Error("GrossExposureForStrategy overflow not detected")
	}

	c := NewPortfolioSnapshot(0, 0, 0, 0,
		map[StrategySymbol]Qty{{"S", "X"}: math.MinInt64},
		map[string]Price{"X": MustPrice("1")},
	)
	if _, err := c.GrossExposureForStrategy("S"); !errors.Is(err, ErrOverflow) {
		t.Error("GrossExposureForStrategy |MinInt64| overflow not detected")
	}
}

func TestPortfolioSnapshotImmutability(t *testing.T) {
	src := map[StrategySymbol]Qty{{"S", "AAPL"}: 100}
	lc := map[string]Price{"AAPL": MustPrice("100")}
	a := NewPortfolioSnapshot(1, 1, 0, 0, src, lc)

	// Mutating the source maps must not affect the snapshot (the constructor
	// deep-copies both maps).
	src[StrategySymbol{"S", "AAPL"}] = 999
	lc["AAPL"] = MustPrice("1")
	if a.StrategyPosition("S", "AAPL") != 100 || a.LastClose["AAPL"] != MustPrice("100") {
		t.Error("snapshot shares storage with its inputs")
	}

	// Clone must be independent too.
	cl := a.Clone()
	cl.Positions[StrategySymbol{"S", "AAPL"}] = -5
	cl.LastClose["AAPL"] = 0
	if a.StrategyPosition("S", "AAPL") != 100 || a.LastClose["AAPL"] != MustPrice("100") {
		t.Error("Clone shares storage with the original")
	}

	// Nil maps become empty maps.
	empty := NewPortfolioSnapshot(0, 0, 0, 0, nil, nil)
	if empty.Positions == nil || empty.LastClose == nil {
		t.Error("nil maps must become empty maps")
	}
}

func TestPortfolioSnapshotJSON(t *testing.T) {
	a := snapshotFixture()
	a.RealizedPnLToday = MustMoney("-12.5")

	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PortfolioSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.NAV != a.NAV || back.Cash != a.Cash || back.RealizedPnLToday != a.RealizedPnLToday {
		t.Errorf("scalar fields lost: %+v", back)
	}
	if len(back.Positions) != len(a.Positions) {
		t.Fatalf("positions count = %d, want %d", len(back.Positions), len(a.Positions))
	}
	for k, v := range a.Positions {
		if back.Positions[k] != v {
			t.Errorf("position %v = %d, want %d", k, back.Positions[k], v)
		}
	}
	for k, v := range a.LastClose {
		if back.LastClose[k] != v {
			t.Errorf("last_close %s = %s, want %s", k, back.LastClose[k], v)
		}
	}

	// Marshal is deterministic (sorted positions array).
	raw2, _ := json.Marshal(a)
	if string(raw) != string(raw2) {
		t.Error("marshal must be deterministic")
	}

	// Duplicate position entries are rejected.
	dup := []byte(`{"nav":"0","cash":"0","realized_pnl_today":"0","unrealized_pnl_today":"0",` +
		`"positions":[{"strategy_id":"S","symbol":"A","qty":1},{"strategy_id":"S","symbol":"A","qty":2}],` +
		`"last_close":{}}`)
	var d PortfolioSnapshot
	if err := json.Unmarshal(dup, &d); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("duplicate entries error = %v, want ErrInvalidArgument", err)
	}

	// null is a no-op.
	keep := snapshotFixture()
	if err := json.Unmarshal([]byte("null"), &keep); err != nil || keep.NAV != MustMoney("100000") {
		t.Errorf("null handling: %v", err)
	}
}
