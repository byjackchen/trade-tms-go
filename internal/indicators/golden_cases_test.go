package indicators

import (
	"math"
	"testing"
)

func TestSMAGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.SMA.Cases {
		assertSeries(t, "SMA w="+itoa(c.Window), SMA(g.SMA.X, c.Window), c.Out)
	}
	for _, c := range g.SMANaN.Cases {
		assertSeries(t, "SMA_NaN w="+itoa(c.Window), SMA(g.SMANaN.xs(), c.Window), c.Out)
	}
	for _, c := range g.SMABig.Cases {
		assertSeries(t, "SMA_big w="+itoa(c.Window), SMA(g.SMABig.X, c.Window), c.Out)
	}
}

func TestRollingSumGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.RollingSum.Cases {
		assertSeries(t, "RollingSum w="+itoa(c.Window), RollingSum(g.RollingSum.X, c.Window), c.Out)
	}
}

func TestRollingStdGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.RollingStd.Cases {
		assertSeries(t, "RollingStd w="+itoa(c.Window)+" ddof="+itoa(c.Ddof),
			RollingStd(g.RollingStd.X, c.Window, c.Ddof), c.Out)
	}
	for _, c := range g.RollingStdBig.Cases {
		assertSeries(t, "RollingStd_big w="+itoa(c.Window)+" ddof="+itoa(c.Ddof),
			RollingStd(g.RollingStdBig.X, c.Window, c.Ddof), c.Out)
	}
}

func TestRollingMaxMinGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.RollingMax.Cases {
		assertSeries(t, "RollingMax w="+itoa(c.Window), RollingMax(g.RollingMax.X, c.Window), c.Out)
	}
	for _, c := range g.RollingMin.Cases {
		assertSeries(t, "RollingMin w="+itoa(c.Window), RollingMin(g.RollingMin.X, c.Window), c.Out)
	}
	for _, c := range g.RollingMaxBig.Cases {
		assertSeries(t, "RollingMax_big w="+itoa(c.Window), RollingMax(g.RollingMaxBig.X, c.Window), c.Out)
	}
	for _, c := range g.RollingMinBig.Cases {
		assertSeries(t, "RollingMin_big w="+itoa(c.Window), RollingMin(g.RollingMinBig.X, c.Window), c.Out)
	}
}

func TestPctReturnGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.PctReturn.Cases {
		assertSeries(t, "PctReturn w="+itoa(c.Window), PctReturn(g.PctReturn.X, c.Window), c.Out)
	}
	for _, c := range g.PctReturnBig.Cases {
		assertSeries(t, "PctReturn_big w="+itoa(c.Window), PctReturn(g.PctReturnBig.X, c.Window), c.Out)
	}
	assertClose(t, "WindowReturn", WindowReturn(g.WindowReturn.Deque), g.WindowReturn.Out.f())
}

func TestATRGolden(t *testing.T) {
	g := loadGolden(t)
	tr := TrueRange(g.ATR.High, g.ATR.Low, g.ATR.Close)
	assertSeries(t, "TrueRange", tr, g.ATR.TrueRange)
	assertSeries(t, "ATRWilder14", ATRWilder(g.ATR.High, g.ATR.Low, g.ATR.Close, 14), g.ATR.Wilder14)
	assertSeries(t, "ATRSimple14", ATRSimple(g.ATR.High, g.ATR.Low, g.ATR.Close, 14), g.ATR.Simple14)
}

func TestStatsGolden(t *testing.T) {
	g := loadGolden(t)
	assertClose(t, "FMean", FMean(g.Stats.Spread), g.Stats.FMean)
	assertClose(t, "PStdev", PStdev(g.Stats.Spread), g.Stats.PStdev)
	assertClose(t, "Stdev", Stdev(g.Stats.Spread), g.Stats.Stdev)
	assertClose(t, "ZScore", ZScore(g.Stats.Spread), g.Stats.ZScore)
}

func TestRollingZScoreGolden(t *testing.T) {
	g := loadGolden(t)
	assertSeries(t, "RollingZScore", RollingZScore(g.RollingZScore.X, g.RollingZScore.Window), g.RollingZScore.Out)
}

func TestOLSGolden(t *testing.T) {
	g := loadGolden(t)
	slope, ok := OLSSlope(g.OLS.X, g.OLS.Y)
	if !ok {
		t.Fatal("OLSSlope unexpectedly degenerate")
	}
	assertClose(t, "OLSSlope", slope, g.OLS.Slope)

	res := OLS(g.OLS.X, g.OLS.Y)
	if !res.OK {
		t.Fatal("OLS unexpectedly degenerate")
	}
	assertClose(t, "OLS.Slope", res.Slope, g.OLS.Slope)
	assertClose(t, "OLS.Intercept", res.Intercept, g.OLS.Intercept)

	perfect, ok := OLSSlope([]float64{1, 2, 3, 4}, []float64{2, 4, 6, 8})
	if !ok {
		t.Fatal("perfect line OLS degenerate")
	}
	assertClose(t, "OLS.PerfectLine", perfect, g.OLS.PerfectLineSlope)

	if _, ok := OLSSlope([]float64{5, 5, 5}, []float64{1, 2, 3}); ok {
		t.Error("degenerate constant-x must return OK=false")
	}
	if g.OLS.DegenerateSlope != nil {
		t.Fatal("reference degenerate slope should be null")
	}

	assertClose(t, "Correlation", Correlation(g.OLS.X, g.OLS.Y), g.OLS.Correlation)
}

func TestMAHelpersGolden(t *testing.T) {
	g := loadGolden(t)
	assertClose(t, "MASlopePct", MASlopePct(g.MAHelpers.Close, 200, 20), g.MAHelpers.MASlopePct)
	if got := MAUptrendDays(g.MAHelpers.Close, 200); got != g.MAHelpers.MAUptrendDays {
		t.Errorf("MAUptrendDays got %d want %d", got, g.MAHelpers.MAUptrendDays)
	}
	// Reconstruct high/low used by the harness (close ±1.5/-1.2) for the
	// 52-week extremum check is unnecessary: the harness emits the rolling
	// values directly. We re-derive high/low the same way.
	high := make([]float64, len(g.MAHelpers.Close))
	low := make([]float64, len(g.MAHelpers.Close))
	for i, c := range g.MAHelpers.Close {
		high[i] = math.Round((c+1.5)*1e4) / 1e4
		low[i] = math.Round((c-1.2)*1e4) / 1e4
	}
	assertClose(t, "RollingHigh252", FiftyTwoWeekHigh(high, 252), g.MAHelpers.RollingHigh252.f())
	assertClose(t, "RollingLow252", FiftyTwoWeekLow(low, 252), g.MAHelpers.RollingLow252.f())
}

func TestSwingGolden(t *testing.T) {
	g := loadGolden(t)
	got := FindSwingPoints(g.Swing.High, g.Swing.Low, g.Swing.Lookback)
	if len(got) != len(g.Swing.Swings) {
		t.Fatalf("swing count got %d want %d (%+v)", len(got), len(g.Swing.Swings), got)
	}
	for i, w := range g.Swing.Swings {
		if got[i].Idx != w.Idx || got[i].Kind.String() != w.Kind {
			t.Errorf("swing[%d] got {%d %s} want {%d %s}", i, got[i].Idx, got[i].Kind, w.Idx, w.Kind)
		}
		assertClose(t, "swing price["+itoa(i)+"]", got[i].Price, w.Price)
	}
}

func TestRoundHalfEvenGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.RoundHalfEven {
		assertClose(t, "round2("+ftoa(c.X)+")", RoundHalfEven(c.X, 2), c.D2)
		assertClose(t, "round3("+ftoa(c.X)+")", RoundHalfEven(c.X, 3), c.D3)
		assertClose(t, "round0("+ftoa(c.X)+")", RoundHalfEven(c.X, 0), c.D0)
	}
}

func TestTrendTemplateGolden(t *testing.T) {
	g := loadGolden(t)
	lin := g.TrendTemplateLinear
	res := EvaluateTrendTemplate(lin.Close, lin.High, lin.Low, lin.MarketCapUSD, lin.MarketCapMinUSD)
	if res.Passed() != lin.Passed {
		t.Errorf("TT passed got %v want %v", res.Passed(), lin.Passed)
	}
	if res.PassingRules() != lin.PassingRules {
		t.Errorf("TT passing rules got %d want %d", res.PassingRules(), lin.PassingRules)
	}
	rules := []bool{
		res.Rule1CloseGtMA50, res.Rule2CloseGtMA150, res.Rule3CloseGtMA200,
		res.Rule4MA50GtMA150, res.Rule5MA150GtMA200, res.Rule6Within25PctHigh,
		res.Rule7Above30PctLow, res.Rule8MarketCapAboveMin,
	}
	for i, want := range lin.Rules {
		if rules[i] != want {
			t.Errorf("TT rule %d got %v want %v", i+1, rules[i], want)
		}
	}
	assertClose(t, "TT.close", res.Close, lin.CloseOut)
	assertClose(t, "TT.ma50", res.MA50, lin.MA50)
	assertClose(t, "TT.ma150", res.MA150, lin.MA150)
	assertClose(t, "TT.ma200", res.MA200, lin.MA200)
	assertClose(t, "TT.high52w", res.High52w, lin.High52w)
	assertClose(t, "TT.low52w", res.Low52w, lin.Low52w)
	if res.MA200UptrendDays != lin.MA200Uptrend {
		t.Errorf("TT ma200 uptrend got %d want %d", res.MA200UptrendDays, lin.MA200Uptrend)
	}

	// Short history (n < 200): rules 1-7 False, only rule 8 evaluated.
	sh := g.TrendTemplateShort
	rs := EvaluateTrendTemplate(sh.Close, sh.Close, sh.Close, 1e9, 5e8)
	if rs.Passed() != sh.Passed || rs.PassingRules() != sh.PassingRules {
		t.Errorf("TT short got passed=%v rules=%d want passed=%v rules=%d",
			rs.Passed(), rs.PassingRules(), sh.Passed, sh.PassingRules)
	}
	assertClose(t, "TT short close", rs.Close, sh.CloseOut)
	if rs.Rule8MarketCapAboveMin != sh.Rule8 {
		t.Errorf("TT short rule8 got %v want %v", rs.Rule8MarketCapAboveMin, sh.Rule8)
	}
}

func TestStageGolden(t *testing.T) {
	g := loadGolden(t)
	for name, c := range g.Stage {
		if got := ClassifyStage(c.Close); got != c.Stage {
			t.Errorf("Stage[%s] got %q want %q", name, got, c.Stage)
		}
	}
}

func TestVCPGolden(t *testing.T) {
	g := loadGolden(t)
	p := DefaultVCPParams()
	p.Lookback = g.VCP.Lookback
	p.Code = "TEST"
	snap, ok := DetectVCP(g.VCP.High, g.VCP.Low, g.VCP.Volume, p)
	if ok != g.VCP.Detected {
		t.Fatalf("VCP detected got %v want %v", ok, g.VCP.Detected)
	}
	if !ok {
		return
	}
	if len(snap.Contractions) != len(g.VCP.Contractions) {
		t.Fatalf("VCP contractions len got %d want %d", len(snap.Contractions), len(g.VCP.Contractions))
	}
	for i := range snap.Contractions {
		assertClose(t, "VCP contraction["+itoa(i)+"]", snap.Contractions[i], g.VCP.Contractions[i])
	}
	assertClose(t, "VCP.last_contraction_pct", snap.LastContractionPct, g.VCP.LastContractionPct)
	assertClose(t, "VCP.pivot_price", snap.PivotPrice, g.VCP.PivotPrice)
	if snap.BaseLengthDays != g.VCP.BaseLengthDays {
		t.Errorf("VCP base_length got %d want %d", snap.BaseLengthDays, g.VCP.BaseLengthDays)
	}
	if snap.VolumeDryup != g.VCP.VolumeDryup {
		t.Errorf("VCP volume_dryup got %v want %v", snap.VolumeDryup, g.VCP.VolumeDryup)
	}
	assertClose(t, "VCP.quality_score", snap.QualityScore, g.VCP.QualityScore)
	assertClose(t, "VCP.vol_dryup_ratio", snap.VolDryupRatio, g.VCP.VolDryupRatio)
	if snap.FinalContractionDurationDays != g.VCP.FinalContractionDurationDays {
		t.Errorf("VCP final_duration got %d want %d", snap.FinalContractionDurationDays, g.VCP.FinalContractionDurationDays)
	}

	// Pure linear ramp must NOT produce a VCP.
	pl := DefaultVCPParams()
	pl.Lookback = g.VCPLinear.Lookback
	if _, ok := DetectVCP(g.VCPLinear.High, g.VCPLinear.Low, g.VCPLinear.Volume, pl); ok != g.VCPLinear.Detected {
		t.Errorf("VCP linear detected got %v want %v", ok, g.VCPLinear.Detected)
	}
}

func TestBreakoutVolumeGolden(t *testing.T) {
	g := loadGolden(t)
	bv := g.BreakoutVolume
	baseline, ok := VolumeBaselineExcludingCurrent(bv.Volume, bv.Base)
	if !ok {
		t.Fatal("VolumeBaselineExcludingCurrent not ok")
	}
	assertClose(t, "baseline_excl_current", baseline, bv.BaselineExclCurrent)
	if got := BreakoutVolumeOK(bv.Volume, bv.Base, bv.Multiple); got != bv.OK {
		t.Errorf("BreakoutVolumeOK got %v want %v", got, bv.OK)
	}
}

func ftoa(f float64) string {
	// minimal float->string for test context labels
	i := int(f)
	if float64(i) == f {
		return itoa(i)
	}
	return itoa(i) + ".xxx"
}
