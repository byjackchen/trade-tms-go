package indicators

// SwingKind classifies a detected swing point.
type SwingKind int

const (
	// SwingHigh is a local maximum (strict, leftmost-tie center).
	SwingHigh SwingKind = iota
	// SwingLow is a local minimum.
	SwingLow
)

func (k SwingKind) String() string {
	if k == SwingHigh {
		return "high"
	}
	return "low"
}

// Swing is a detected local extremum. Idx is the position in the input slices.
type Swing struct {
	Idx   int
	Price float64
	Kind  SwingKind
}

// FindSwingPoints detects local swing extrema.
//
// For each center index i in [lookback, n-lookback): consider the
// (2*lookback+1)-bar window centered on i.
//
//   - swing HIGH when highs[i] == window_high.max() AND argmax(window_high) ==
//     lookback. argmax returns the LEFTMOST index of the maximum, so a high at
//     the center is accepted only if no EARLIER bar in the window shares that
//     maximum.
//   - swing LOW is symmetric with lows and argmin.
//
// Both a high and a low can be emitted at the same i (checked independently).
// Output is sorted by Idx ascending, with high emitted before low at the same
// Idx.
//
// The first and last `lookback` bars never produce a swing (post-confirmation
// requirement). NaN is not expected in OHLC; if present it participates in the
// comparisons as IEEE NaN (every comparison false), so it never becomes an
// extremum.
func FindSwingPoints(high, low []float64, lookback int) []Swing {
	if lookback < 1 {
		panic("indicators: FindSwingPoints lookback must be >= 1")
	}
	n := len(high)
	if len(low) != n {
		panic("indicators: FindSwingPoints requires equal-length high/low")
	}
	out := make([]Swing, 0)
	for i := lookback; i < n-lookback; i++ {
		lo := i - lookback
		hi := i + lookback + 1 // exclusive

		// High: argmax over window == lookback (i.e. leftmost max is the center)
		// and highs[i] equals that max.
		maxVal := high[lo]
		maxArg := 0
		for j := lo + 1; j < hi; j++ {
			if high[j] > maxVal { // strict so leftmost wins on ties
				maxVal = high[j]
				maxArg = j - lo
			}
		}
		if high[i] == maxVal && maxArg == lookback {
			out = append(out, Swing{Idx: i, Price: high[i], Kind: SwingHigh})
		}

		// Low: argmin over window == lookback.
		minVal := low[lo]
		minArg := 0
		for j := lo + 1; j < hi; j++ {
			if low[j] < minVal {
				minVal = low[j]
				minArg = j - lo
			}
		}
		if low[i] == minVal && minArg == lookback {
			out = append(out, Swing{Idx: i, Price: low[i], Kind: SwingLow})
		}
	}
	// Swings are ordered by idx, with high-before-low at equal idx. Our build
	// order already yields ascending idx with high before low, so the output is
	// already sorted.
	return out
}
