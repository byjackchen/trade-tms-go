package sepa

// mathhelpers.go holds the arithmetic primitives whose CPython semantics the
// SEPA sizing path depends on bit-for-bit.

import (
	"math"
	"strconv"
)

// pyFloorDiv reproduces int(a // b) for Python floats: CPython's
// float.__floordiv__ (Objects/floatobject.c float_divmod) computes
// mod = fmod(a, b); div = (a - mod) / b; floordiv = floor(div); and nudges up
// when (div - floordiv) > 0.5 (a correction for the rounded division). The
// reference then int()-truncates the float result; for the non-negative
// equity/risk operands SEPA uses, that truncation equals the floordiv value.
// b == 0 is guarded by the caller (stop_distance <= 0 -> 0 shares).
func pyFloorDiv(a, b float64) int {
	mod := math.Mod(a, b)
	div := (a - mod) / b
	floordiv := math.Floor(div)
	if (div - floordiv) > 0.5 {
		floordiv++
	}
	// int() truncates toward zero; floordiv is already an integral float.
	return int(floordiv)
}

// itoa is strconv.Itoa, aliased for terse use in the reason builder.
func itoa(i int) string { return strconv.Itoa(i) }
