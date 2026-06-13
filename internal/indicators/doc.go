// Package indicators implements the technical indicators required by the
// strategies (SMA/EMA, ATR, RSI, rolling highs/lows, etc.), ported from the
// Python reference with bit-for-bit comparable semantics: same warm-up/NaN
// handling, same smoothing recurrences, same lookback windows. Heavy
// numerics may lean on gonum, but only where it provably matches pandas
// behaviour.
//
// Rules:
//   - Pure functions over slices/series; no I/O.
//   - Every indicator gets a golden test against reference output.
package indicators
