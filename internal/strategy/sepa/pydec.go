package sepa

// pydec.go renders the two string conventions the SEPA outputs use:
//
//  1. repr(float) / str(float) — used by str(Decimal(str(f))) on the
//     pivot/stop/entry/market-cap fields stored as Decimal(str(some_float)) (or,
//     for entry, as the original Decimal(str(close))). For every magnitude SEPA
//     produces, str(Decimal(str(f))) == repr(f), so we only need a faithful
//     shortest round-trip float repr.
//
//  2. The "%.2f" formatting inside the reason string (pivot/close/stop) and
//     str(round(x, 2)) for the {last_pct} field.
//
// The float repr is: take the shortest decimal string that round-trips to the
// same double, then render with scientific notation iff the decimal exponent is
// < -4 or >= 16, else fixed notation; a value with no fractional digits gets a
// trailing ".0". Go's strconv.FormatFloat(f, 'g', -1, 64) yields the same
// shortest digits but (a) drops the trailing ".0" on integral values and (b)
// switches to exponent at a different threshold. We post-process FormatFloat to
// apply the exponent threshold and the ".0" rule.

import (
	"math"
	"strconv"
	"strings"
)

// pyFloatRepr renders f as a shortest round-trip float repr (repr(float) /
// str(float)). Matches str(Decimal(str(f))) for all finite magnitudes the SEPA
// path emits.
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
	if f == 0 {
		// Preserve signed zero (-0.0 -> "-0.0").
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}

	// Shortest round-tripping mantissa + exponent via Go's 'e' format with
	// precision -1.
	// e.g. 121.0 -> "1.21e+02", 116.23 -> "1.1623e+02", 1e10 -> "1e+10".
	es := strconv.FormatFloat(f, 'e', -1, 64)
	neg := false
	if es[0] == '-' {
		neg = true
		es = es[1:]
	}
	// Split mantissa and exponent.
	eIdx := strings.IndexByte(es, 'e')
	mant := es[:eIdx]
	exp, _ := strconv.Atoi(es[eIdx+1:])

	// Collect significant digits (drop the decimal point in the mantissa).
	var digits string
	if dot := strings.IndexByte(mant, '.'); dot >= 0 {
		digits = mant[:dot] + mant[dot+1:]
	} else {
		digits = mant
	}
	// digits is now the significant-digit string with an implied decimal point
	// after the first digit; `exp` is that first digit's power of ten.
	// The "decimal exponent" used for the threshold test is `exp`
	// (the exponent of the leading significant digit): scientific iff
	// exp < -4 or exp >= 16.
	out := formatPyDigits(digits, exp)
	if neg {
		return "-" + out
	}
	return out
}

// formatPyDigits renders the significant digit string `digits` (no sign, no
// point; first digit has power-of-ten `exp`) per the repr threshold:
// scientific when exp < -4 or exp >= 16, else positional. A purely integral
// positional result gets a trailing ".0".
func formatPyDigits(digits string, exp int) string {
	n := len(digits)
	if exp < -4 || exp >= 16 {
		// Scientific: d.dddde(+/-)EE — the exponent is padded to >= 2 digits.
		var sb strings.Builder
		sb.WriteByte(digits[0])
		if n > 1 {
			sb.WriteByte('.')
			sb.WriteString(digits[1:])
		}
		sb.WriteByte('e')
		if exp >= 0 {
			sb.WriteByte('+')
		} else {
			sb.WriteByte('-')
		}
		ea := exp
		if ea < 0 {
			ea = -ea
		}
		es := strconv.Itoa(ea)
		if len(es) < 2 {
			es = "0" + es
		}
		sb.WriteString(es)
		return sb.String()
	}

	// Positional.
	if exp >= 0 {
		if exp+1 >= n {
			// All significant digits are in the integer part; pad zeros, add ".0".
			return digits + strings.Repeat("0", exp+1-n) + ".0"
		}
		// Decimal point splits the digit string.
		return digits[:exp+1] + "." + digits[exp+1:]
	}
	// exp in [-4, -1]: 0.00ddd form.
	return "0." + strings.Repeat("0", -exp-1) + digits
}

// pyRound2Str renders str(round(x, 2)) used for the {last_pct} field in the
// entry reason. round() is banker's rounding; the result is a float, then
// str(float)==repr(float). We round-half-even to 2 dp then repr.
func pyRound2Str(x float64) string {
	return pyFloatRepr(roundHalfEven(x, 2))
}

// roundHalfEven duplicates indicators.RoundHalfEven locally to keep this file
// dependency-light; identical banker's-rounding semantics.
func roundHalfEven(x float64, digits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	pow := math.Pow(10, float64(digits))
	scaled := x * pow
	floor := math.Floor(scaled)
	diff := scaled - floor
	var rounded float64
	switch {
	case diff < 0.5:
		rounded = floor
	case diff > 0.5:
		rounded = floor + 1
	default:
		if math.Mod(floor, 2) == 0 {
			rounded = floor
		} else {
			rounded = floor + 1
		}
	}
	return rounded / pow
}

// pyFixed2 renders f with exactly two decimals ("%.2f"). Go's
// strconv.FormatFloat(f, 'f', 2, 64) uses round-half-to-even (IEEE rounding) on
// the final digit.
func pyFixed2(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// parsePyFloat parses a decimal string (a stored str(Decimal)) to float64.
// Malformed input yields 0 (the SEPA load path only sees strings it itself
// produced).
func parsePyFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
