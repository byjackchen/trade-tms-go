package sectorrotation

import (
	"math"
	"strconv"
	"strings"
)

// pyFloatRepr reproduces CPython's repr(float) / str(float), which the
// reference uses to serialize history/last_close closes:
//
//	state_dict stores str(Decimal) where the Decimal came from Decimal(str(f)),
//	so the stored string is exactly str(f) — Python's float repr.
//
// CPython float_repr uses "shortest string that round-trips" (PyOS_double_to_
// string with format 'r'), with these surface rules that differ from Go's
// strconv.FormatFloat:
//   - integral values carry a trailing ".0" (Go's 'g' drops it): 142 -> "142.0".
//   - exponential form uses a lowercase 'e' with an explicit sign and a
//     minimum-2-digit exponent ("1e-05" not "1e-5"); Go uses the same 'e' code
//     but a 2-digit exponent already, and switches to exponential at the same
//     magnitudes (|x| >= 1e16 or 0 < |x| < 1e-4). We normalise the few
//     remaining differences below.
//
// For this strategy's price domain (positive, ~10..10000, 4 decimals) only the
// trailing-".0" rule is ever exercised; the exponential handling is included
// for completeness/robustness.
func pyFloatRepr(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}

	// CPython 'r' format: shortest round-trip. Go's 'g' with precision -1 is the
	// same shortest-round-trip digit selection. We then reconcile the decimal-
	// point / exponent surface rules.
	s := strconv.FormatFloat(f, 'g', -1, 64)

	// Exponent present: normalise to CPython's "<mantissa>e<sign><2+ digits>".
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant := s[:i]
		exp := s[i+1:]
		sign := "+"
		if exp[0] == '+' || exp[0] == '-' {
			if exp[0] == '-' {
				sign = "-"
			}
			exp = exp[1:]
		}
		for len(exp) < 2 {
			exp = "0" + exp
		}
		// CPython keeps a bare integral mantissa (e.g. "1e+16", not "1.0e+16").
		return mant + "e" + sign + exp
	}

	// No exponent: ensure a decimal point so integral values read like "142.0".
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
