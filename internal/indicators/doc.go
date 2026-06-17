// Package indicators is the shared numerical foundation for the four
// strategies (SEPA, Pairs, Sector Rotation, Intraday ORB). Every primitive
// has well-defined warm-up/NaN handling, rolling-window min_periods == window,
// ddof conventions, and round-half-even output rounding. Golden tests
// (golden_test.go + testdata/golden.json) pin these numerical outputs within
// 1e-9.
//
// Surface:
//
//   - rolling.go      SMA, RollingSum, RollingStd(ddof), RollingMax/Min,
//     scalar Max/Min/Mean (skipna).
//   - incremental.go  O(1)/O(window) streaming counterparts (RollingSMA,
//     RollingStd, RollingMax/Min) — bit-identical to batch for
//     min/max/std; within tol for the running-sum SMA.
//   - highlow.go      RollingHigh/Low, FiftyTwoWeek{High,Low} (252-bar with the
//     SEPA full-history fall-back), PctReturn, WindowReturn.
//   - atr.go          TrueRange, ATRWilder, ATRSimple.
//   - stats.go        FMean, PStdev, Stdev, ZScore, RollingZScore, OLS{Slope},
//     Correlation, Spread (Pairs hedge-ratio/z primitives).
//   - ma.go           MA, MASlopePct, MAUptrendDays, FractionAbove.
//   - swing.go        FindSwingPoints (numpy argmax/argmin leftmost-tie center).
//   - vcp.go          DetectVCP + contraction/tail/quality helpers.
//   - trend_template.go EvaluateTrendTemplate (8 rules) + ClassifyStage.
//   - volume.go       VolumeBaselineExcludingCurrent / BreakoutVolumeOK
//     (look-ahead guard).
//   - round.go        RoundHalfEven (banker's rounding).
//
// Rules: pure functions over float64 slices; no I/O; NaN signals warmup/
// undefined (callers test math.IsNaN). The gonum pin in deps.go is retained to
// avoid go.mod/go.sum churn across parallel build phases even though the
// implementations here are stdlib-only.
package indicators
