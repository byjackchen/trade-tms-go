package indicators

// VCP (Volatility Contraction Pattern) detection primitives, ported from
// sepa/vcp.py with the exact geometry/volume semantics documented in
// docs/spec/strategy-sepa.md §6 [MUST-MATCH]. These are pure functions over
// OHLCV slices; the SEPA strategy builder composes them with stage / trend
// template / regime context.

// VCP canonical thresholds (vcp.py:29-33). Exported so call sites and tests
// reference the single source of truth.
const (
	VCPDryupBaselineThreshold = 0.7 // final-contraction vol < 70% of baseline
	VCPBaselineLookback       = 50
	VCPDefaultBaseMinDays     = 25  // ~5 weeks (bar count)
	VCPDefaultBaseMaxDays     = 150 // ~30 weeks (bar count)
)

// Contraction is one (high → low) leg of the base, with its depth percentage
// and the positional indices of the low and the preceding high.
type Contraction struct {
	DepthPct float64
	LowPrice float64
	LowIdx   int
	HighIdx  int
}

// ContractionDepthPct is the VCP contraction depth: (high - low) / high * 100
// (vcp.py:82). Pure helper exposed for testing and reuse.
func ContractionDepthPct(high, low float64) float64 {
	return (high - low) / high * 100.0
}

// VCPSnapshot mirrors the Python VCPSnapshot dataclass (vcp.py:37-51). Bar-count
// fields (BaseLengthDays, FinalContractionDurationDays) keep the reference names
// per the [IMPROVE] note's compatibility guidance; they are bar positional
// differences, not calendar days.
type VCPSnapshot struct {
	Code                         string
	Contractions                 []float64 // depths %, oldest -> newest, round(2)
	LastContractionPct           float64   // round(2)
	PivotPrice                   float64   // not rounded
	BaseLengthDays               int
	VolumeDryup                  bool
	QualityScore                 float64 // round(2)
	VolDryupRatio                float64 // round(3)
	FinalContractionDurationDays int

	// BaseLowPrice is the lowest LOW across the detected base window (base start
	// .. trailing low inclusive). TMS ENHANCEMENT (not in the Python SEPA
	// reference's VCPSnapshot): the actionable trade-plan uses it as the VCP stop
	// reference. Not rounded.
	BaseLowPrice float64
}

// DetectVCPParams carries the tunable detector knobs (vcp.py:54-63 defaults).
type DetectVCPParams struct {
	Code               string
	Lookback           int     // swing lookback (config.vcp_lookback); default 5
	MinContractions    int     // default 2
	MaxLastContraction float64 // default 10.0
	BaseLengthMin      int     // default 25
	BaseLengthMax      int     // default 150
}

// DefaultVCPParams returns the reference defaults (vcp.py:54-63).
func DefaultVCPParams() DetectVCPParams {
	return DetectVCPParams{
		Lookback:           5,
		MinContractions:    2,
		MaxLastContraction: 10.0,
		BaseLengthMin:      VCPDefaultBaseMinDays,
		BaseLengthMax:      VCPDefaultBaseMaxDays,
	}
}

// DetectVCP ports sepa/vcp.py detect_vcp EXACTLY. high/low/volume must be equal
// length and date-ordered (oldest first). Returns (snapshot, true) on a valid
// base, or (_, false) when no convergent tail meets the Minervini geometry.
//
// All numeric handling mirrors the reference: float64 math, round-half-even on
// the output fields, the exact pairing/convergent-tail rules, the pivot taken
// from highs AFTER the trailing low, and the volume-dryup logic (50-bar baseline
// + per-contraction strictly-decreasing 6-bar average volume).
func DetectVCP(high, low, volume []float64, p DetectVCPParams) (VCPSnapshot, bool) {
	n := len(high)
	if len(low) != n || len(volume) != n {
		panic("indicators: DetectVCP requires equal-length high/low/volume")
	}
	if n < 30 {
		return VCPSnapshot{}, false
	}
	if p.MinContractions <= 0 {
		p.MinContractions = 2
	}

	swings := FindSwingPoints(high, low, p.Lookback)

	// Pair (high, low) into contractions. A later high overwrites an earlier
	// unpaired high; a low consumes the pending high even if depth is out of
	// range (vcp.py:75-86).
	var contractions []Contraction
	haveHigh := false
	var lastHigh float64
	var lastHighIdx int
	for _, s := range swings {
		switch s.Kind {
		case SwingHigh:
			lastHigh = s.Price
			lastHighIdx = s.Idx
			haveHigh = true
		case SwingLow:
			if haveHigh {
				depth := ContractionDepthPct(lastHigh, s.Price)
				if depth > 0.5 && depth < 50 {
					contractions = append(contractions, Contraction{
						DepthPct: depth,
						LowPrice: s.Price,
						LowIdx:   s.Idx,
						HighIdx:  lastHighIdx,
					})
				}
				haveHigh = false
			}
		}
	}

	if len(contractions) < p.MinContractions {
		return VCPSnapshot{}, false
	}

	// Convergent tail: walk backwards; include while each older depth >= the
	// last-added depth (>=, allows equal). Stop at first violation, then
	// reverse to chronological order (vcp.py:93-101).
	tail := []Contraction{contractions[len(contractions)-1]}
	for i := len(contractions) - 2; i >= 0; i-- {
		if contractions[i].DepthPct >= tail[len(tail)-1].DepthPct {
			tail = append(tail, contractions[i])
		} else {
			break
		}
	}
	// reverse tail in place
	for l, r := 0, len(tail)-1; l < r; l, r = l+1, r-1 {
		tail[l], tail[r] = tail[r], tail[l]
	}
	if len(tail) < p.MinContractions {
		return VCPSnapshot{}, false
	}

	lastDepth := tail[len(tail)-1].DepthPct
	if lastDepth > p.MaxLastContraction {
		return VCPSnapshot{}, false
	}

	lastLowIdx := tail[len(tail)-1].LowIdx
	lastHighIdxInTail := tail[len(tail)-1].HighIdx

	// Pivot = max high AFTER the trailing low (excluding the low's own bar).
	var pivot float64
	if lastLowIdx+1 < n {
		pivot = Max(high[lastLowIdx+1:])
	} else {
		pivot = high[n-1]
	}

	baseStartIdx := tail[0].LowIdx
	baseLen := n - 1 - baseStartIdx
	if !(p.BaseLengthMin <= baseLen && baseLen <= p.BaseLengthMax) {
		return VCPSnapshot{}, false
	}

	// baseLow is the lowest LOW across the base window [baseStartIdx, lastLowIdx].
	// TMS ENHANCEMENT: the actionable trade-plan stop reference. Guarded indices.
	baseLow := low[baseStartIdx]
	if lastLowIdx+1 <= n && baseStartIdx <= lastLowIdx {
		baseLow = Min(low[baseStartIdx : lastLowIdx+1])
	}

	finalDuration := lastLowIdx - lastHighIdxInTail
	if finalDuration < 1 {
		finalDuration = 1
	}

	// 50-bar baseline volume preceding the trailing low (vcp.py:126-131).
	lo := lastLowIdx - VCPBaselineLookback
	if lo < 0 {
		lo = 0
	}
	hiBase := lastLowIdx
	if hiBase < 1 {
		hiBase = 1
	}
	baselineAvg := 0.0
	if lo < hiBase {
		baselineAvg = Mean(volume[lo:hiBase])
	}

	// Final-contraction volume: trailing high .. trailing low inclusive.
	finalAvg := 0.0
	if lastHighIdxInTail <= lastLowIdx {
		finalAvg = Mean(volume[lastHighIdxInTail : lastLowIdx+1])
	}
	volDryupRatio := 1.0
	if baselineAvg > 0 {
		volDryupRatio = finalAvg / baselineAvg
	}

	// Per-contraction relative dryup: each tail leg's 6-bar avg volume strictly
	// less than the previous (vcp.py:139-147).
	relativeDryup := true
	havePrev := false
	var prevVol float64
	for _, c := range tail {
		segLo := c.LowIdx - 5
		if segLo < 0 {
			segLo = 0
		}
		avg := Mean(volume[segLo : c.LowIdx+1])
		if havePrev && avg >= prevVol {
			relativeDryup = false
			break
		}
		prevVol = avg
		havePrev = true
	}
	volumeDryup := relativeDryup && volDryupRatio < VCPDryupBaselineThreshold

	score := 0.25*float64(len(tail)) + 0.4*(1-lastDepth/10)
	if volumeDryup {
		score += 0.2
	}
	if score > 1.0 {
		score = 1.0
	}

	depths := make([]float64, len(tail))
	for i, c := range tail {
		depths[i] = RoundHalfEven(c.DepthPct, 2)
	}

	return VCPSnapshot{
		Code:                         p.Code,
		Contractions:                 depths,
		LastContractionPct:           RoundHalfEven(lastDepth, 2),
		PivotPrice:                   pivot,
		BaseLengthDays:               baseLen,
		VolumeDryup:                  volumeDryup,
		QualityScore:                 RoundHalfEven(score, 2),
		VolDryupRatio:                RoundHalfEven(volDryupRatio, 3),
		FinalContractionDurationDays: finalDuration,
		BaseLowPrice:                 baseLow,
	}, true
}
