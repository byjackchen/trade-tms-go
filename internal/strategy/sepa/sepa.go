package sepa

// sepa.go is the pure Go port of SEPASignalGenerator (sepa/signal.py) — the
// stateful, streaming SEPA state machine. It composes the parity-tested
// indicator primitives in internal/indicators (Trend Template, Stage, VCP,
// swing, breakout volume, round-half-even) with the grade gate and grade-aware
// sizing, reproducing the reference's decision chain, sizing arithmetic,
// look-ahead guards, and string formats BYTE-FOR-BYTE.
//
// Layering [MUST-MATCH spec §0]: this package imports only internal/indicators
// (the numerical foundation) and the stdlib — never broker/engine/riskgate.
// External context (regime / market cap / earnings / catalyst) arrives via
// setters exactly as the reference's set_* methods; the engine adapter feeds
// them from internal/riskgate's look-ahead-safe providers.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// decState carries a price-like state field both as its float64 value (for
// arithmetic and comparisons — the reference compares the Decimal whose value
// is the shortest-repr float, so float compares are exact) and its canonical
// Python str(Decimal) string (for state_summary / state_dict / intent parity).
type decState struct {
	val float64
	str string // pyFloatRepr(val); "" iff zero-and-unset
}

func decFromFloat(f float64) decState {
	return decState{val: f, str: pyFloatRepr(f)}
}

// decZero is the reference's Decimal(0): value 0.0, str "0".
var decZero = decState{val: 0, str: "0"}

// Generator is the SEPA SignalGenerator (sepa/signal.py SEPASignalGenerator).
// Not safe for concurrent use; the engine drives one instance per symbol on a
// single goroutine (spec §15: per-symbol serialization).
type Generator struct {
	cfg Config

	// Internal kline buffer (signal.py _klines). Parallel slices mirror the
	// float/int columns of the reference DataFrame; ts mirrors the
	// DatetimeIndex. Oldest first.
	ts     []time.Time
	open   []float64
	high   []float64
	low    []float64
	close  []float64
	volume []float64 // float64 for mean() parity (pandas volume.mean() is float)

	// Position state (signal.py:142-147).
	position   int
	entryPrice decState
	stopPrice  decState
	pivotPrice decState
	grade      Grade // "" == nil

	intentGeneration int

	// Externally-supplied context (signal.py:149-153 cold-start defaults).
	regime           string
	marketCapUSD     float64
	earningsBlackout bool
	catalyst         bool

	// inc carries the per-generator incremental indicator state that replaces the
	// O(window)-per-bar batch recomputation in the flat-book entry chain. It is
	// fed once per bar in appendBar and rebuilt by WarmupFromHistory / LoadState.
	// See incstate.go / incentry.go (parity-critical, byte-identical to batch).
	inc *incState
}

// New constructs a SEPA generator, validating the config exactly as
// SEPASignalGeneratorConfig.__post_init__ (signal.py:155-164). Returns
// ErrInvalidConfig-wrapped errors on invalid risk_pct / hard_stop_pct / nil
// equity provider.
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Generator{
		cfg:        cfg,
		regime:     "unknown",
		entryPrice: decZero,
		stopPrice:  decZero,
		pivotPrice: decZero,
		inc:        newIncState(),
	}, nil
}

// ---------------------------------------------------------------------------
// External context setters (signal.py:170-180)
// ---------------------------------------------------------------------------

// SetRegime sets the market regime ("bull"/"neutral"/"warning"/"bear").
func (g *Generator) SetRegime(regime string) { g.regime = regime }

// SetMarketCap sets the market cap in USD.
func (g *Generator) SetMarketCap(marketCapUSD float64) { g.marketCapUSD = marketCapUSD }

// SetEarningsBlackout sets the earnings-blackout flag.
func (g *Generator) SetEarningsBlackout(blackout bool) { g.earningsBlackout = blackout }

// SetCatalyst sets the catalyst flag.
func (g *Generator) SetCatalyst(catalyst bool) { g.catalyst = catalyst }

// Symbol returns the configured symbol.
func (g *Generator) Symbol() string { return g.cfg.Symbol }

// Position returns the current signed position (test/inspection helper).
func (g *Generator) Position() int { return g.position }

// StopPriceFloat returns the current stop as float64 (inspection helper for
// downstream exit construction in higher layers).
func (g *Generator) StopPriceFloat() float64 { return g.stopPrice.val }

// ---------------------------------------------------------------------------
// Core: process one bar (signal.py:186-193)
// ---------------------------------------------------------------------------

// OnBar processes one bar and returns zero or more signals. A bar for a
// different symbol is ignored WITHOUT being appended to history
// (test_signal.py:162-166). Otherwise the bar is appended, then the flat book
// runs the entry chain and a held book runs the (hard-stop-only) exit check —
// never both on one bar.
func (g *Generator) OnBar(bar Bar) []Signal {
	if bar.Symbol != g.cfg.Symbol {
		return nil
	}
	g.appendBar(bar)
	if g.position == 0 {
		return g.maybeEnter(bar)
	}
	return g.maybeExit(bar)
}

// ---------------------------------------------------------------------------
// Entry logic (signal.py:199-254)
// ---------------------------------------------------------------------------

func (g *Generator) maybeEnter(bar Bar) []Signal {
	n := len(g.close)
	if n < 200 {
		return nil // not enough history for Trend Template (signal.py:200-201)
	}

	// [CORRECTNESS/PERF fix 3] Early market-cap reject. Trend-Template rule 8
	// (market_cap >= MarketCapMinUSD) is a necessary condition for entry: when it
	// fails, EvaluateTrendTemplate().Passed() is always false and the name is
	// rejected. Hoisting the SAME gate ahead of the Stage/Trend-Template/VCP chain
	// yields the IDENTICAL admit/reject set (a sub-min-cap name never traded
	// before either) while skipping the entire indicator recompute for names that
	// cannot clear the cap. Byte-identical predicate to rule 8 (trend_template.go).
	if !(g.marketCapUSD >= g.cfg.MarketCapMinUSD) {
		return nil
	}

	// 1. Stage must be "2" (on history INCLUDING the current bar). Computed from
	// the per-generator incremental state — byte-identical to
	// indicators.ClassifyStage(g.close) (incentry.go).
	stage := g.classifyStageInc()
	if stage != "2" {
		return nil
	}

	// 2. Trend Template all 8 rules (INCLUDING the current bar). Incremental,
	// byte-identical to indicators.EvaluateTrendTemplate(...).Passed().
	ttPass := g.trendTemplatePassInc()
	if !ttPass {
		return nil
	}

	// 3. VCP base on history EXCLUDING the current bar (signal.py:217-221) —
	// the look-ahead/self-reference guard so the breakout bar's own high cannot
	// redefine the pivot. prior = _klines.iloc[:-1].
	priorLen := n - 1
	if priorLen < 30 {
		return nil
	}
	vcp, ok := indicators.DetectVCP(
		g.high[:priorLen], g.low[:priorLen], g.volume[:priorLen],
		indicators.DetectVCPParams{
			Code:               g.cfg.Symbol,
			Lookback:           g.cfg.VCPLookback,
			MinContractions:    2,
			MaxLastContraction: 10.0,
			BaseLengthMin:      indicators.VCPDefaultBaseMinDays,
			BaseLengthMax:      indicators.VCPDefaultBaseMaxDays,
		},
	)
	if !ok {
		return nil
	}

	// 4. Breakout: close strictly > pivot (signal.py:230-233). The reference
	// compares float(bar.close) <= vcp.pivot_price.
	closeF := bar.Close
	if closeF <= vcp.PivotPrice {
		return nil
	}
	if !g.breakoutVolumeOK() {
		return nil
	}

	// 5. Grade — final go/no-go (signal.py:236-251).
	earningsPass := !g.earningsBlackout
	grade := gradeSetup(setupInputs{
		trendTemplatePass:   ttPass,
		earningsPass:        earningsPass,
		stage:               stage,
		catalyst:            g.catalyst,
		vcpContractionCount: len(vcp.Contractions),
		regime:              g.regime,
	})
	if grade == GradeSkip {
		return nil
	}

	// 6. Stop + size, emit (signal.py:253).
	return g.buildLongEntry(bar, vcp, grade)
}

// breakoutVolumeOK mirrors _breakout_volume_ok (signal.py:256-273): base
// lookback is hard-coded 60 (NOT 50); the denominator excludes today.
func (g *Generator) breakoutVolumeOK() bool {
	const baseLookback = 60
	return indicators.BreakoutVolumeOK(g.volume, baseLookback, g.cfg.BreakoutVolumeMultiple)
}

// buildLongEntry mirrors _build_long_entry (signal.py:275-311): compute the
// stop as max(hard_stop, pivot_stop) with round-half-even to 4 dp, derive the
// first-tranche share count, persist state, and emit the LONG signal with the
// exact reason string.
func (g *Generator) buildLongEntry(bar Bar, vcp indicators.VCPSnapshot, grade Grade) []Signal {
	entryF := bar.Close
	pivotF := vcp.PivotPrice

	hardStop := indicators.RoundHalfEven(entryF*(1-g.cfg.HardStopPct/100), 4)
	pivotStop := indicators.RoundHalfEven(pivotF*(1-g.cfg.PivotBufferPct/100), 4)
	stopF := hardStop
	if pivotStop > stopF {
		stopF = pivotStop
	}

	tranches := 2
	if grade == GradeAPlus {
		tranches = 3
	}
	shares := g.computeFirstTrancheShares(entryF, stopF, tranches)
	if shares <= 0 {
		return nil // no entry, no state (signal.py:282-283)
	}

	// Persist state (signal.py:286-291).
	g.position = shares
	g.entryPrice = decFromFloat(entryF) // == Decimal(str(close))
	g.stopPrice = decFromFloat(stopF)
	g.pivotPrice = decFromFloat(pivotF)
	g.grade = grade

	reason := "SEPA " + string(grade) + " :: stage=2, TT pass, VCP " +
		itoa(len(vcp.Contractions)) + " contractions (last " +
		pyFloatRepr(vcp.LastContractionPct) + "%), pivot $" + pyFixed2(pivotF) +
		" -> close $" + pyFixed2(entryF) + ", stop $" + pyFixed2(stopF)

	return []Signal{{
		Symbol:     bar.Symbol,
		TS:         bar.TS,
		Side:       SideLong,
		TargetQty:  shares,
		Reason:     reason,
		Confidence: 1.0,
		Grade:      grade,
		StopPrice:  g.stopPrice.str,
	}}
}

// computeFirstTrancheShares mirrors _compute_first_tranche_shares
// (signal.py:313-326): pull live equity, compute risk dollars, two floor
// divisions (risk // stop_distance, then // tranches).
func (g *Generator) computeFirstTrancheShares(entry, stop float64, tranches int) int {
	equity := g.cfg.EquityProvider()
	riskDollar := equity * (g.cfg.RiskPct / 100)
	stopDistance := entry - stop
	if stopDistance <= 0 {
		return 0
	}
	fullShares := pyFloorDiv(riskDollar, stopDistance)
	d := tranches
	if d < 1 {
		d = 1
	}
	return fullShares / d
}

// ---------------------------------------------------------------------------
// Exit logic (signal.py:332-354) — P2 hard stop only
// ---------------------------------------------------------------------------

func (g *Generator) maybeExit(bar Bar) []Signal {
	// close <= stop (Decimal compare, <=, equality triggers).
	if bar.Close <= g.stopPrice.val {
		stopAt := g.stopPrice
		grade := g.grade

		g.position = 0
		g.entryPrice = decZero
		g.stopPrice = decZero
		g.pivotPrice = decZero
		g.grade = ""

		reason := "SEPA stop hit at $" + pyFixed2(stopAt.val) +
			" :: close $" + pyFixed2(bar.Close)
		return []Signal{{
			Symbol:     bar.Symbol,
			TS:         bar.TS,
			Side:       SideFlat,
			TargetQty:  0,
			Reason:     reason,
			Confidence: 1.0,
			Grade:      grade,
			StopPrice:  stopAt.str,
		}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// History buffer (signal.py:360-376)
// ---------------------------------------------------------------------------

func (g *Generator) appendBar(bar Bar) {
	g.ts = append(g.ts, bar.TS)
	g.open = append(g.open, bar.Open)
	g.high = append(g.high, bar.High)
	g.low = append(g.low, bar.Low)
	g.close = append(g.close, bar.Close)
	g.volume = append(g.volume, float64(bar.Volume))

	// [PERF fix 2] Feed the incremental indicator state once per bar (O(1)
	// amortized) so the entry chain never recomputes O(n*window) batch MAs.
	g.inc.onAppend(bar.High, bar.Low, g.close)

	if max := g.cfg.HistoryMaxBars; max > 0 && len(g.close) > max {
		// Retain the last max rows (tail), matching DataFrame.tail(max).
		//
		// [PERF fix 2] Front-trim by RESLICING (g.x = g.x[cut:]) instead of the
		// old copy-into-new-slice trimFront. Reslicing is O(1) and allocation-free
		// — it only advances the slice header past the dropped prefix; the live
		// window stays contiguous and oldest-first (so DetectVCP / EvaluateIntent
		// keep clean slices) and len(g.close) stays exactly max (parity:
		// BarsInHistory). The wasted prefix is reclaimed by the next append's grow.
		// This removes the ~84% per-bar GC/alloc the profile flagged.
		cut := len(g.close) - max
		g.ts = g.ts[cut:]
		g.open = g.open[cut:]
		g.high = g.high[cut:]
		g.low = g.low[cut:]
		g.close = g.close[cut:]
		g.volume = g.volume[cut:]
		g.inc.trimFront(cut)
	}
}

// WarmupFromHistory primes the kline buffer from historical OHLCV WITHOUT
// evaluating any signal (signal.py:378-392). Empty input is a no-op. The latest
// HistoryMaxBars rows are retained. Bars must be date-ordered oldest-first.
func (g *Generator) WarmupFromHistory(bars []Bar) {
	if len(bars) == 0 {
		return
	}
	kept := bars
	if max := g.cfg.HistoryMaxBars; max > 0 && len(bars) > max {
		kept = bars[len(bars)-max:]
	}
	g.ts = make([]time.Time, len(kept))
	g.open = make([]float64, len(kept))
	g.high = make([]float64, len(kept))
	g.low = make([]float64, len(kept))
	g.close = make([]float64, len(kept))
	g.volume = make([]float64, len(kept))
	for i, b := range kept {
		g.ts[i] = b.TS
		g.open[i] = b.Open
		g.high[i] = b.High
		g.low[i] = b.Low
		g.close[i] = b.Close
		g.volume[i] = float64(b.Volume)
	}
	// Rebuild the incremental indicator state over the warmed buffer; produces
	// state byte-identical to feeding the bars one-by-one via appendBar.
	g.inc.rebuild(g.high, g.low, g.close)
}
