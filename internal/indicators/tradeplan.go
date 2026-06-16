package indicators

// tradeplan.go is a TMS ENHANCEMENT (not present in the Python SEPA reference):
// the actionable trade-plan primitives that let a SEPA forming signal carry a
// reliable, non-null buy point / stop / proximity / risk so it is rankable and
// tradeable from the watchlist. The Python oracle's SEPA forming signal carries
// only strength=100 (8/8 trend template) — these helpers deliberately diverge to
// add the missing trade plan.
//
// All helpers are pure functions over OHLCV tails so they are unit-testable in
// isolation; the SEPA generator composes them in EvaluateIntent.

import "math"

// SwingPlanLookback is the number of COMPLETED daily bars the swing-high/low
// pivot+stop fallback scans when no VCP base is detected. TMS enhancement.
const SwingPlanLookback = 10

// VolumeSMALookback is the volume baseline window for vol_ratio (today's volume
// vs the trailing 50-bar volume SMA). TMS enhancement.
const VolumeSMALookback = 50

// SwingHighPivot returns the highest HIGH of the last `lookback` COMPLETED daily
// bars — the swing-high pivot fallback used when DetectVCP fails. `high` is the
// full high history (oldest first); `completedExclusive` is true when the most
// recent bar should be excluded as "today's forming bar" (the entry chain's
// look-ahead guard), false to include it. Returns 0 when there is no history.
//
// TMS enhancement — not in the Python SEPA reference.
func SwingHighPivot(high []float64, lookback int, completedExclusive bool) float64 {
	seg := completedTail(high, lookback, completedExclusive)
	if len(seg) == 0 {
		return 0
	}
	return Max(seg)
}

// SwingLowStop returns the lowest LOW of the last `lookback` COMPLETED daily bars
// — the stop fallback paired with SwingHighPivot. Returns 0 when there is no
// history. TMS enhancement.
func SwingLowStop(low []float64, lookback int, completedExclusive bool) float64 {
	seg := completedTail(low, lookback, completedExclusive)
	if len(seg) == 0 {
		return 0
	}
	return Min(seg)
}

// completedTail slices the trailing `lookback` elements of x, optionally dropping
// the final (most-recent) element first when completedExclusive is true.
func completedTail(x []float64, lookback int, completedExclusive bool) []float64 {
	n := len(x)
	if completedExclusive {
		n-- // drop the forming bar
	}
	if n <= 0 || lookback <= 0 {
		return nil
	}
	lo := n - lookback
	if lo < 0 {
		lo = 0
	}
	return x[lo:n]
}

// VolumeRatio is today's volume divided by the trailing `lookback`-bar volume SMA
// (the full window including today, matching a classic "relative volume"). Returns
// 0 when the window is not yet full or the average is non-positive. TMS enhancement.
func VolumeRatio(volume []float64, lookback int) float64 {
	n := len(volume)
	if n < lookback || lookback <= 0 {
		return 0
	}
	avg := Mean(volume[n-lookback:])
	if avg <= 0 {
		return 0
	}
	return volume[n-1] / avg
}

// ProximityToTriggerPct is the SIGNED distance from the current close up to the
// pivot, as a percent of close: (pivot - close)/close*100. Positive => price is
// still below the pivot (approaching the buy point); negative => price is already
// above the pivot (extended / triggered). TMS enhancement.
func ProximityToTriggerPct(pivot, close float64) float64 {
	if close <= 0 {
		return 0
	}
	return (pivot - close) / close * 100.0
}

// RiskPct is the trade's stop risk as a percent of the pivot (buy point):
// (pivot - stop)/pivot*100. Returns 0 when pivot is non-positive. TMS enhancement.
func RiskPct(pivot, stop float64) float64 {
	if pivot <= 0 {
		return 0
	}
	return (pivot - stop) / pivot * 100.0
}

// PctOff52wkHigh is the distance below the 52-week high as a percent
// (<= 0; 0 == at a new high): (close - high52wk)/high52wk*100. TMS enhancement.
func PctOff52wkHigh(close, high52wk float64) float64 {
	if high52wk <= 0 {
		return 0
	}
	return (close - high52wk) / high52wk * 100.0
}

// BuyReadinessInputs are the normalized trade-plan facts the readiness composite
// scores. TMS enhancement.
type BuyReadinessInputs struct {
	// ProximityPct is the SIGNED proximity to the pivot (see ProximityToTriggerPct):
	// positive = below pivot, negative = extended above it.
	ProximityPct float64
	// RSRank is the cross-sectional RS percentile in [1,99]; <=0 means "unknown".
	RSRank int
	// HasVCP is true when a real VCP base (not the swing fallback) backs the plan.
	HasVCP bool
	// BaseDepthPct is the VCP base depth (last contraction %); used only when HasVCP.
	BaseDepthPct float64
	// RiskPct is the stop risk as a percent of the pivot (see RiskPct).
	RiskPct float64
}

// BuyReadiness is a transparent 0..100 composite ranking "ready to buy" =
// near-but-not-above the pivot, leading on RS, tight base, acceptable risk.
// TMS enhancement — not in the Python SEPA reference.
//
// Weighted blend (weights sum to 1.0):
//
//	0.40 * proximity   — peaks in the 0..5% "approaching the pivot" sweet spot;
//	                     0 when far below; decays when extended above the pivot.
//	0.30 * rs          — RSRank/99 (0 when unknown).
//	0.20 * tightness   — 1.0 with a VCP base shallower than ~8% (tight), scaling
//	                     down to 0 by ~25% depth; 0.3 floor when no VCP (swing
//	                     fallback is a looser, lower-conviction base).
//	0.10 * riskQuality — 1.0 at <=3% risk, linearly down to 0 by 15% risk.
//
// The proximity sub-score is the heart of "actionability": a name sitting right
// under its pivot (small positive proximity) is the most actionable; a name far
// below has no near-term trigger, and a name already extended above the pivot has
// (mostly) already triggered, so both score low.
func BuyReadiness(in BuyReadinessInputs) float64 {
	const (
		wProximity = 0.40
		wRS        = 0.30
		wTight     = 0.20
		wRisk      = 0.10

		sweetSpotPct = 5.0  // proximity peak window below the pivot
		extendedTol  = 3.0  // how far above the pivot before proximity score hits 0
		farPct       = 25.0 // proximity that scores ~0 when far below the pivot

		tightDepthPct = 8.0  // VCP depth at/under which tightness is 1.0
		looseDepthPct = 25.0 // VCP depth at/over which tightness is 0
		noVCPFloor    = 0.3  // swing-fallback base tightness floor

		riskFloorPct = 3.0  // risk at/under which risk quality is 1.0
		riskCapPct   = 15.0 // risk at/over which risk quality is 0
	)

	// --- proximity sub-score (0..1) ---
	var prox float64
	switch p := in.ProximityPct; {
	case p >= 0 && p <= sweetSpotPct:
		// In the sweet spot: 1.0 right at the pivot, easing to ~0.6 at the edge.
		prox = 1.0 - 0.4*(p/sweetSpotPct)
	case p > sweetSpotPct:
		// Below the pivot but outside the sweet spot: linear decay to 0 at farPct.
		prox = clamp01(0.6 * (farPct - p) / (farPct - sweetSpotPct))
	default: // p < 0: extended above the pivot — decays from 1.0 AT the pivot to 0 by
		// extendedTol. Peaking at 1.0 (not 0.6) makes the score CONTINUOUS at the pivot
		// crossing (the sweet-spot branch is also 1.0 at p=0), so a name a hair above its
		// pivot is not penalised ~16 readiness points vs one exactly at it — a just-broken-
		// out name is still highly actionable, only a clearly-extended one is not.
		prox = clamp01((extendedTol + p) / extendedTol)
	}

	// --- RS sub-score (0..1) ---
	var rs float64
	if in.RSRank > 0 {
		rs = clamp01(float64(in.RSRank) / float64(RSRankMax))
	}

	// --- base-tightness sub-score (0..1) ---
	var tight float64
	if in.HasVCP {
		switch d := in.BaseDepthPct; {
		case d <= tightDepthPct:
			tight = 1.0
		case d >= looseDepthPct:
			tight = 0.0
		default:
			tight = (looseDepthPct - d) / (looseDepthPct - tightDepthPct)
		}
	} else {
		tight = noVCPFloor
	}

	// --- risk-quality sub-score (0..1) ---
	var riskQ float64
	switch rk := in.RiskPct; {
	case rk <= riskFloorPct:
		riskQ = 1.0
	case rk >= riskCapPct:
		riskQ = 0.0
	default:
		riskQ = (riskCapPct - rk) / (riskCapPct - riskFloorPct)
	}

	score := 100.0 * (wProximity*prox + wRS*rs + wTight*tight + wRisk*riskQ)
	return math.Round(score*100) / 100 // 2dp, stable for JSON
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
