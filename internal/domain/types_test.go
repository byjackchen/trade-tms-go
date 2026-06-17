package domain

// types_test.go covers Bar, Signal, Grade/SetupInputs/GradeSetup,
// the Signal family, Order/Fill/Position and Fundamentals.

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var testTS = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

func validBar() Bar {
	return Bar{
		Symbol: "AAPL", TS: testTS,
		Open: MustPrice("100.00"), High: MustPrice("101.50"),
		Low: MustPrice("99.50"), Close: MustPrice("101.00"),
		Volume: 1_000_000,
	}
}

func TestBarValidate(t *testing.T) {
	if err := validBar().Validate(); err != nil {
		t.Fatalf("valid bar rejected: %v", err)
	}
	mut := func(f func(*Bar)) Bar { b := validBar(); f(&b); return b }
	tests := []struct {
		name string
		bar  Bar
	}{
		{"empty symbol", mut(func(b *Bar) { b.Symbol = "" })},
		{"zero ts", mut(func(b *Bar) { b.TS = time.Time{} })},
		{"non-utc ts", mut(func(b *Bar) {
			b.TS = time.Date(2024, 1, 2, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
		})},
		{"high < low", mut(func(b *Bar) { b.High = MustPrice("99.00") })},
		{"open above high", mut(func(b *Bar) { b.Open = MustPrice("200") })},
		{"close below low", mut(func(b *Bar) { b.Close = MustPrice("1") })},
		{"negative volume", mut(func(b *Bar) { b.Volume = -1 })},
	}
	for _, tt := range tests {
		if err := tt.bar.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidArgument", tt.name, err)
		}
	}
}

func TestBarJSONRoundTrip(t *testing.T) {
	b := validBar()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Prices appear as exact decimal strings, never floats.
	if !strings.Contains(string(raw), `"open":"100"`) || !strings.Contains(string(raw), `"high":"101.5"`) {
		t.Errorf("unexpected bar JSON: %s", raw)
	}
	var back Bar
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Symbol != b.Symbol || !back.TS.Equal(b.TS) || back.Open != b.Open ||
		back.High != b.High || back.Low != b.Low || back.Close != b.Close || back.Volume != b.Volume {
		t.Errorf("round trip mismatch: %+v vs %+v", back, b)
	}
}

func TestNewSignalDefaults(t *testing.T) {
	s := NewSignal("AAPL", testTS, SideLong, 150, "SEPA A+ :: ...")
	if s.Confidence != 1.0 {
		t.Errorf("default confidence = %v, want 1.0", s.Confidence)
	}
	if s.Grade != nil || s.StopPrice != nil {
		t.Error("Grade/StopPrice must default to nil")
	}
	if err := s.Validate(); err != nil {
		t.Errorf("valid signal rejected: %v", err)
	}

	// ORB edge case (§2.3): FLAT may carry the held qty, not 0.
	flat := NewSignal("TSLA", testTS, SideFlat, 80, "EOD exit at 15:55")
	if err := flat.Validate(); err != nil {
		t.Errorf("ORB-style FLAT with held qty rejected: %v", err)
	}
	// Negative target (short by convention) is legal.
	short := NewSignal("KO", testTS, SideShort, -50, "Pairs ...")
	if err := short.Validate(); err != nil {
		t.Errorf("short signal rejected: %v", err)
	}
}

func TestSignalValidateErrors(t *testing.T) {
	base := NewSignal("AAPL", testTS, SideLong, 1, "r")
	tests := []struct {
		name string
		f    func(Signal) Signal
	}{
		{"empty symbol", func(s Signal) Signal { s.Symbol = ""; return s }},
		{"zero ts", func(s Signal) Signal { s.TS = time.Time{}; return s }},
		{"bad side", func(s Signal) Signal { s.Side = "UP"; return s }},
		{"bad grade", func(s Signal) Signal { g := Grade("Z"); s.Grade = &g; return s }},
	}
	for _, tt := range tests {
		if err := tt.f(base).Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: want ErrInvalidArgument", tt.name)
		}
	}
}

func TestSignalJSONRoundTrip(t *testing.T) {
	stop := MustPrice("93.46")
	s := NewSignal("AAPL", testTS, SideLong, 150, "SEPA A+ :: pivot $100.00")
	s.Grade = GradePtr(GradeAPlus)
	s.StopPrice = &stop

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"grade":"A+"`) || !strings.Contains(string(raw), `"stop_price":"93.46"`) {
		t.Errorf("unexpected signal JSON: %s", raw)
	}
	var back Signal
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Grade == nil || *back.Grade != GradeAPlus || back.StopPrice == nil || *back.StopPrice != stop {
		t.Errorf("optional fields lost: %+v", back)
	}

	// nil optionals serialize as null and round-trip to nil.
	s2 := NewSignal("KO", testTS, SideFlat, 0, "close")
	raw2, _ := json.Marshal(s2)
	if !strings.Contains(string(raw2), `"grade":null`) || !strings.Contains(string(raw2), `"stop_price":null`) {
		t.Errorf("nil optionals must encode as null: %s", raw2)
	}
	var back2 Signal
	if err := json.Unmarshal(raw2, &back2); err != nil || back2.Grade != nil || back2.StopPrice != nil {
		t.Errorf("null round trip: %+v, %v", back2, err)
	}
}

func TestGradeSetup(t *testing.T) {
	base := SetupInputs{
		TrendTemplatePass: true, EarningsPass: true, Stage: "2",
		Catalyst: true, VCPContractionCount: 3, Regime: "bull",
	}
	mut := func(f func(*SetupInputs)) SetupInputs { in := base; f(&in); return in }
	tests := []struct {
		name string
		in   SetupInputs
		want Grade
	}{
		// Rule 1: bear regime or stage != "2" → skip (checked first).
		{"bear regime", mut(func(i *SetupInputs) { i.Regime = "bear" }), GradeSkip},
		{"stage 1", mut(func(i *SetupInputs) { i.Stage = "1" }), GradeSkip},
		{"stage spelled differently", mut(func(i *SetupInputs) { i.Stage = "2 " }), GradeSkip},
		{"bear overrides everything", SetupInputs{TrendTemplatePass: true, EarningsPass: true, Stage: "2", Catalyst: true, VCPContractionCount: 5, Regime: "bear"}, GradeSkip},
		// Rule 2: TT and earnings must both pass.
		{"tt fail", mut(func(i *SetupInputs) { i.TrendTemplatePass = false }), GradeSkip},
		{"earnings fail", mut(func(i *SetupInputs) { i.EarningsPass = false }), GradeSkip},
		// Rule 3: < 2 contractions → skip.
		{"one contraction", mut(func(i *SetupInputs) { i.VCPContractionCount = 1 }), GradeSkip},
		{"zero contractions", mut(func(i *SetupInputs) { i.VCPContractionCount = 0 }), GradeSkip},
		// Rule 4: catalyst + >=3 contractions + bull → A+.
		{"a plus", base, GradeAPlus},
		{"a plus more contractions", mut(func(i *SetupInputs) { i.VCPContractionCount = 5 }), GradeAPlus},
		// Rule 5: otherwise B.
		{"no catalyst", mut(func(i *SetupInputs) { i.Catalyst = false }), GradeB},
		{"only two contractions", mut(func(i *SetupInputs) { i.VCPContractionCount = 2 }), GradeB},
		{"neutral regime", mut(func(i *SetupInputs) { i.Regime = "neutral" }), GradeB},
		{"warning regime", mut(func(i *SetupInputs) { i.Regime = "warning" }), GradeB},
	}
	for _, tt := range tests {
		if got := GradeSetup(tt.in); got != tt.want {
			t.Errorf("%s: GradeSetup = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestIntentDefaults(t *testing.T) {
	sepa := NewSEPASignal()
	if sepa.StrategyID != "sepa" || sepa.Grade != 0 || sepa.PivotPrice != nil || sepa.RSRank != nil {
		t.Errorf("SEPA defaults wrong: %+v", sepa)
	}
	pairs := NewPairsSignal()
	if pairs.StrategyID != "pairs" || pairs.LegRole != LegLong ||
		pairs.ZEntryThreshold != 2.0 || pairs.ZExitThreshold != 0.5 ||
		pairs.HedgeRatio != 1.0 || pairs.PairID != "" {
		t.Errorf("Pairs defaults wrong: %+v", pairs)
	}
	rot := NewSectorRotationSignal()
	if rot.StrategyID != "sector_rotation" || rot.Rank != 0 || rot.TargetWeight != 0 {
		t.Errorf("Rotation defaults wrong: %+v", rot)
	}
	orb := NewIntradayBreakoutSignal()
	if orb.StrategyID != "intraday_breakout" || orb.ORBHigh != nil || orb.EntryWindowEnd != nil {
		t.Errorf("ORB defaults wrong: %+v", orb)
	}
}

func TestSignalJSONShape(t *testing.T) {
	prox := 1.25
	intent := NewSEPASignal()
	intent.Symbol = "AAPL"
	intent.State = StateBuy
	intent.Strength = 87.5
	intent.ProximityToTriggerPct = &prox
	intent.UpdatedAt = testTS
	intent.Generation = 42
	intent.Grade = 87
	intent.TrendTemplatePass = true
	pivot := MustPrice("100.00")
	intent.PivotPrice = &pivot

	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Shared fields come first (embedded struct), strategy fields follow —
	// the declaration order of the struct.
	s := string(raw)
	for _, frag := range []string{
		`"symbol":"AAPL"`, `"state":"buy"`, `"strength":87.5`,
		`"proximity_to_trigger_pct":1.25`, `"generation":42`,
		`"strategy_id":"sepa"`, `"grade":87`, `"trend_template_pass":true`,
		`"pivot_price":"100"`, `"stop_price":null`, `"rs_rank":null`,
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("intent JSON missing %s: %s", frag, s)
		}
	}
	if strings.Index(s, `"symbol"`) > strings.Index(s, `"grade"`) {
		t.Error("shared fields must precede strategy-specific fields")
	}
	var back SEPASignal
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Symbol != "AAPL" || back.PivotPrice == nil || *back.PivotPrice != pivot ||
		back.ProximityToTriggerPct == nil || *back.ProximityToTriggerPct != prox {
		t.Errorf("round trip mismatch: %+v", back)
	}
}

func TestStrengthFromZ(t *testing.T) {
	tests := []struct {
		z, want float64
	}{
		{0, 0}, {1.5, 50}, {3.0, 100}, {4.5, 100}, {-1.5, 50}, {-6, 100},
		{0.3, 10.0}, // verified bit-exact (cross-platform deterministic)
	}
	for _, tt := range tests {
		if got := StrengthFromZ(tt.z); got != tt.want {
			t.Errorf("StrengthFromZ(%v) = %v, want %v", tt.z, got, tt.want)
		}
	}
}

func TestStrengthFromRank(t *testing.T) {
	tests := []struct {
		rank, total int
		want        float64
	}{
		{1, 11, 100.0},
		{0, 11, 0.0},  // unranked
		{1, 1, 100.0}, // total<=1, rank==1
		{2, 1, 0.0},   // total<=1, rank!=1
		{0, 0, 0.0},
		{11, 11, 0.0}, // rank >= total
		{12, 11, 0.0},
		{2, 11, 90.0}, // 100 - 1/10*100; verified bit-exact (cross-platform)
		{6, 11, 50.0},
		{10, 11, 10.0},
		{2, 3, 50.0},
	}
	for _, tt := range tests {
		if got := StrengthFromRank(tt.rank, tt.total); got != tt.want {
			t.Errorf("StrengthFromRank(%d, %d) = %v, want %v", tt.rank, tt.total, got, tt.want)
		}
	}
}

func TestOrderLifecycle(t *testing.T) {
	o := NewMarketOrder("O-20240102-001", "SEPARunner-000", "AAPL", OrderSideBuy, 150, "SEPA A+ :: ...", testTS)
	if o.Type != OrderTypeMarket || o.TIF != TIFGTC || o.Status != OrderStatusSubmitted {
		t.Errorf("NewMarketOrder defaults wrong: %+v", o)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}

	mut := func(f func(*Order)) Order { x := o; f(&x); return x }
	bad := []struct {
		name string
		o    Order
	}{
		{"empty id", mut(func(x *Order) { x.ClientOrderID = "" })},
		{"empty strategy", mut(func(x *Order) { x.StrategyID = "" })},
		{"empty symbol", mut(func(x *Order) { x.Symbol = "" })},
		{"bad side", mut(func(x *Order) { x.Side = "HOLD" })},
		{"bad type", mut(func(x *Order) { x.Type = "ICEBERG" })},
		{"bad tif", mut(func(x *Order) { x.TIF = "GTD" })},
		{"bad status", mut(func(x *Order) { x.Status = "LOST" })},
		{"zero qty", mut(func(x *Order) { x.Qty = 0 })},
		{"negative qty", mut(func(x *Order) { x.Qty = -10 })},
		{"zero ts", mut(func(x *Order) { x.TS = time.Time{} })},
		{"limit without price", mut(func(x *Order) { x.Type = OrderTypeLimit })},
		{"stop without price", mut(func(x *Order) { x.Type = OrderTypeStopMarket })},
	}
	for _, tt := range bad {
		if err := tt.o.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidArgument", tt.name, err)
		}
	}

	limitPx := MustPrice("99.95")
	lim := mut(func(x *Order) { x.Type = OrderTypeLimit; x.LimitPrice = &limitPx })
	if err := lim.Validate(); err != nil {
		t.Errorf("valid limit order rejected: %v", err)
	}
	stopPx := MustPrice("95.00")
	stp := mut(func(x *Order) { x.Type = OrderTypeStopLimit; x.LimitPrice = &limitPx; x.StopPrice = &stopPx })
	if err := stp.Validate(); err != nil {
		t.Errorf("valid stop-limit order rejected: %v", err)
	}
}

func TestFill(t *testing.T) {
	f := Fill{
		TradeID:       "VENUE123-1704153600000000000", // "{venue_order_id}-{ts_ns}" reference format
		ClientOrderID: "O-001",
		VenueOrderID:  "VENUE123",
		StrategyID:    "SEPARunner-000",
		Symbol:        "AAPL",
		Side:          OrderSideBuy,
		Qty:           150,
		Price:         MustPrice("101.2345"), // 4-dp adapter price held exactly
		Commission:    0,                     // zero in backtest (§7.1)
		TS:            testTS,
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid fill rejected: %v", err)
	}
	if n, err := f.Notional(); err != nil || n != MustMoney("15185.175") {
		t.Errorf("Notional = %v, %v; want 15185.175", n, err)
	}

	mut := func(fn func(*Fill)) Fill { x := f; fn(&x); return x }
	bad := []struct {
		name string
		f    Fill
	}{
		{"empty trade id", mut(func(x *Fill) { x.TradeID = "" })},
		{"empty order id", mut(func(x *Fill) { x.ClientOrderID = "" })},
		{"empty strategy id", mut(func(x *Fill) { x.StrategyID = "" })},
		{"empty symbol", mut(func(x *Fill) { x.Symbol = "" })},
		{"bad side", mut(func(x *Fill) { x.Side = "X" })},
		{"zero qty (duplicate push)", mut(func(x *Fill) { x.Qty = 0 })},
		{"negative qty (regressed push)", mut(func(x *Fill) { x.Qty = -5 })},
		{"zero price", mut(func(x *Fill) { x.Price = 0 })},
		{"negative price", mut(func(x *Fill) { x.Price = -1 })},
		{"negative commission", mut(func(x *Fill) { x.Commission = -1 })},
		{"zero ts", mut(func(x *Fill) { x.TS = time.Time{} })},
	}
	for _, tt := range bad {
		if err := tt.f.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidArgument", tt.name, err)
		}
	}

	huge := mut(func(x *Fill) { x.Qty = math.MaxInt64; x.Price = MustPrice("2") })
	if _, err := huge.Notional(); !errors.Is(err, ErrOverflow) {
		t.Error("Notional overflow not detected")
	}
}

func TestPosition(t *testing.T) {
	p := Position{
		StrategyID: "SEPARunner-000", Symbol: "AAPL",
		SignedQty: 150, AvgPx: MustPrice("100.00"), RealizedPnL: 0, UpdatedAt: testTS,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid position rejected: %v", err)
	}
	if !p.IsLong() || p.IsShort() || p.IsFlat() {
		t.Error("long predicates wrong")
	}
	short := p
	short.SignedQty = -50
	if !short.IsShort() || short.IsLong() || short.IsFlat() {
		t.Error("short predicates wrong")
	}
	flat := p
	flat.SignedQty = 0
	if !flat.IsFlat() {
		t.Error("flat predicate wrong")
	}
	if v, err := p.MarketValue(MustPrice("101.50")); err != nil || v != MustMoney("15225") {
		t.Errorf("MarketValue = %v, %v", v, err)
	}
	if v, err := short.MarketValue(MustPrice("101.50")); err != nil || v != MustMoney("-5075") {
		t.Errorf("short MarketValue = %v, %v (signed)", v, err)
	}
	for _, bad := range []Position{
		{Symbol: "AAPL"},
		{StrategyID: "S"},
		{StrategyID: "S", Symbol: "AAPL", AvgPx: -1},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("bad position accepted: %+v", bad)
		}
	}
}

func TestFundamentals(t *testing.T) {
	f := Fundamentals{
		Ticker:       "AAPL",
		MarketCapUSD: MustMoney("3000000000000"), // 3T
		AsOf:         testTS,
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid fundamentals rejected: %v", err)
	}
	if f.MarketCapFloat64() != 3e12 {
		t.Errorf("MarketCapFloat64 = %v", f.MarketCapFloat64())
	}
	for _, bad := range []Fundamentals{
		{MarketCapUSD: 1, AsOf: testTS},               // empty ticker
		{Ticker: "X", MarketCapUSD: 0, AsOf: testTS},  // SF1 loader keeps only marketcap > 0
		{Ticker: "X", MarketCapUSD: -1, AsOf: testTS}, // negative
		{Ticker: "X", MarketCapUSD: 1},                // zero as_of
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("bad fundamentals accepted: %+v", bad)
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Fundamentals
	if err := json.Unmarshal(raw, &back); err != nil || back.MarketCapUSD != f.MarketCapUSD || back.Ticker != f.Ticker {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}

func TestVCPSnapshotJSON(t *testing.T) {
	v := VCPSnapshot{
		Code:                         "VCP-3T",
		Contractions:                 []float64{18.5, 11.2, 5.4},
		LastContractionPct:           5.4,
		PivotPrice:                   101.25,
		BaseLengthDays:               45,
		VolumeDryup:                  true,
		QualityScore:                 0.82,
		VolDryupRatio:                0.61,
		FinalContractionDurationDays: 9,
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, frag := range []string{`"code":"VCP-3T"`, `"contractions":[18.5,11.2,5.4]`, `"last_contraction_pct":5.4`, `"final_contraction_duration_days":9`} {
		if !strings.Contains(string(raw), frag) {
			t.Errorf("VCP JSON missing %s: %s", frag, raw)
		}
	}
	var back VCPSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Code != v.Code || len(back.Contractions) != 3 || back.PivotPrice != v.PivotPrice {
		t.Errorf("round trip: %+v", back)
	}
}
