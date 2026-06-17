package domain

// fixed.go implements the shared int64 fixed-point engine (scale 1e-4) that
// backs Price and Money, plus the correctly-rounded float rounding helpers.
//
// Numeric model (see docs/spec/domain-types-money.md §1):
//
//   - Prices and monetary amounts are int64 counts of 1e-4 units
//     ("ten-thousandths of a dollar"). Equity prices from the data layer are
//     2-decimal exact (price_precision=2), so the 4-decimal scale holds them
//     exactly with headroom for adapter fill prices, which are formatted at
//     4 decimals (f"{last_px:.4f}").
//   - Representable range: ±922,337,203,685,477.5807 (and the single extra
//     negative value -922,337,203,685,477.5808 = MinInt64). All arithmetic is
//     overflow-checked; helpers return ErrOverflow instead of wrapping.
//   - String parsing is EXACT: a literal whose value is not representable at
//     1e-4 scale yields ErrInexact (never silent rounding). Conversions from
//     float64 are the only place rounding happens, and they round
//     HALF-TO-EVEN on the decimal digits of the float's shortest
//     representation — the decimal-string bridge quantized to 0.0001 with
//     ROUND_HALF_EVEN, this library's default rounding mode.
//     This is deliberately NOT half-up: 0.00005 -> 0.0000, 0.00015 -> 0.0002.
//   - PyRound performs correctly-rounded half-to-even on the EXACT binary value
//     (so round(2.675, 2) == 2.67, because the double 2.675 is really
//     2.67499999...). Note this can differ from the decimal-string bridge above
//     on values whose shortest repr is a decimal tie; both behaviors are
//     supported, each by its dedicated helper.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

const (
	// FixedScale is the fixed-point denominator: 1 Price/Money unit == 1e-4.
	FixedScale = 10_000
	// fixedFracDigits is the number of decimal digits carried by the scale.
	fixedFracDigits = 4
)

// Sentinel errors for the numeric layer. All returned errors wrap one of
// these, so callers can match with errors.Is.
var (
	// ErrOverflow reports that a result does not fit in the int64 fixed-point
	// (or integer) range.
	ErrOverflow = errors.New("domain: numeric overflow")
	// ErrInvalidNumber reports a malformed numeric literal.
	ErrInvalidNumber = errors.New("domain: invalid numeric literal")
	// ErrInexact reports a literal that is well-formed but not exactly
	// representable at 1e-4 scale (exact parsing never rounds).
	ErrInexact = errors.New("domain: value not representable at 1e-4 scale")
	// ErrNotFinite reports a NaN or ±Inf float input.
	ErrNotFinite = errors.New("domain: not a finite number")
	// ErrInvalidArgument reports an out-of-domain argument (e.g. negative
	// ndigits for PyRound).
	ErrInvalidArgument = errors.New("domain: invalid argument")
)

var pow10tab = [...]int64{
	1, 10, 100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000, 100_000_000,
	1_000_000_000, 10_000_000_000, 100_000_000_000, 1_000_000_000_000,
	10_000_000_000_000, 100_000_000_000_000, 1_000_000_000_000_000,
	10_000_000_000_000_000, 100_000_000_000_000_000, 1_000_000_000_000_000_000,
}

// ---------------------------------------------------------------------------
// Overflow-checked int64 primitives
// ---------------------------------------------------------------------------

func checkedAdd64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSub64(a, b int64) (int64, bool) {
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return 0, false
	}
	return a - b, true
}

func checkedMul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	// MinInt64 * -1 (either order) overflows and would also make the
	// division check below panic, so reject it explicitly.
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	c := a * b
	if c/b != a {
		return 0, false
	}
	return c, true
}

func checkedNeg64(a int64) (int64, bool) {
	if a == math.MinInt64 {
		return 0, false
	}
	return -a, true
}

func checkedAbs64(a int64) (int64, bool) {
	if a == math.MinInt64 {
		return 0, false
	}
	if a < 0 {
		return -a, true
	}
	return a, true
}

// ---------------------------------------------------------------------------
// Decimal-literal parsing into 1e-4 fixed point
// ---------------------------------------------------------------------------

type roundMode uint8

const (
	// roundExact errors (ErrInexact) when any precision would be lost.
	roundExact roundMode = iota
	// roundHalfEven rounds half-to-even at the 4th decimal digit
	// (ROUND_HALF_EVEN, this library's default rounding mode).
	roundHalfEven
)

// magLimit is 2^63 as uint64: the magnitude of MinInt64. Magnitudes are
// accumulated unsigned so that the full asymmetric int64 range round-trips.
const magLimit = uint64(1) << 63

func anyNonzeroDigit(b []byte) bool {
	for _, c := range b {
		if c != '0' {
			return true
		}
	}
	return false
}

// parseFixed4 parses a decimal literal — optional sign, digits with optional
// fraction, optional e/E integer exponent — into int64 units of 1e-4.
// It never goes through float64, so parsing is exact by construction; mode
// controls what happens when the value has digits below 1e-4.
func parseFixed4(s string, mode roundMode) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidNumber)
	}
	i := 0
	neg := false
	if s[i] == '+' || s[i] == '-' {
		neg = s[i] == '-'
		i++
	}

	// Mantissa: collect significant digits (leading zeros stripped) and the
	// count of fractional digits.
	var buf []byte
	fracLen := 0
	seenDigit := false
	seenDot := false
	seenNonzero := false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			seenDigit = true
			if seenDot {
				fracLen++
			}
			if c == '0' && !seenNonzero {
				continue // skip leading zeros (fracLen above still counts them)
			}
			seenNonzero = true
			buf = append(buf, c)
		case c == '.':
			if seenDot {
				return 0, fmt.Errorf("%w: %q has two decimal points", ErrInvalidNumber, s)
			}
			seenDot = true
		default:
			goto mantissaDone
		}
	}
mantissaDone:
	if !seenDigit {
		return 0, fmt.Errorf("%w: %q has no digits", ErrInvalidNumber, s)
	}

	// Optional exponent.
	exp := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		eneg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			eneg = s[i] == '-'
			i++
		}
		start := i
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			if exp < 1_000_000_000 { // clamp; |exp| beyond this is overflow/zero anyway
				exp = exp*10 + int(s[i]-'0')
			}
		}
		if i == start {
			return 0, fmt.Errorf("%w: %q has an empty exponent", ErrInvalidNumber, s)
		}
		if eneg {
			exp = -exp
		}
	}
	if i != len(s) {
		return 0, fmt.Errorf("%w: trailing characters in %q", ErrInvalidNumber, s)
	}

	if len(buf) == 0 {
		return 0, nil // ±0 in any spelling
	}

	// value * 1e4 = D * 10^shift, where D is the integer formed by buf.
	shift := exp - fracLen + fixedFracDigits

	var mag uint64
	if shift >= 0 {
		m, err := parseMagnitude(buf, s)
		if err != nil {
			return 0, err
		}
		mag = m
		for k := 0; k < shift; k++ {
			if mag > magLimit/10 {
				return 0, fmt.Errorf("%w: %q exceeds the 1e-4 fixed-point range", ErrOverflow, s)
			}
			mag *= 10
		}
	} else {
		k := -shift
		keepLen := len(buf) - k
		var kept []byte
		var first byte
		var sticky bool
		switch {
		case keepLen > 0:
			kept = buf[:keepLen]
			first = buf[keepLen]
			sticky = anyNonzeroDigit(buf[keepLen+1:])
		case keepLen == 0:
			first = buf[0]
			sticky = anyNonzeroDigit(buf[1:])
		default: // value's magnitude is below 10^-(4+gap)
			first = '0'
			sticky = anyNonzeroDigit(buf)
		}
		if mode == roundExact && (first != '0' || sticky) {
			return 0, fmt.Errorf("%w: %q", ErrInexact, s)
		}
		m, err := parseMagnitude(kept, s)
		if err != nil {
			return 0, err
		}
		mag = m
		if mode == roundHalfEven {
			roundUp := first > '5' || (first == '5' && (sticky || mag%2 == 1))
			if roundUp {
				mag++ // mag <= magLimit still holds (checked below)
			}
		}
	}

	if mag > magLimit || (!neg && mag == magLimit) {
		return 0, fmt.Errorf("%w: %q exceeds the 1e-4 fixed-point range", ErrOverflow, s)
	}
	if neg {
		if mag == magLimit {
			return math.MinInt64, nil
		}
		return -int64(mag), nil
	}
	return int64(mag), nil
}

// parseMagnitude parses a digit slice into a uint64 magnitude, capped at
// magLimit (2^63).
func parseMagnitude(digits []byte, orig string) (uint64, error) {
	var mag uint64
	for _, c := range digits {
		d := uint64(c - '0')
		if mag > (math.MaxUint64-d)/10 {
			return 0, fmt.Errorf("%w: %q exceeds the 1e-4 fixed-point range", ErrOverflow, orig)
		}
		mag = mag*10 + d
		if mag > magLimit {
			return 0, fmt.Errorf("%w: %q exceeds the 1e-4 fixed-point range", ErrOverflow, orig)
		}
	}
	return mag, nil
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// formatFixed4 renders v (1e-4 units) canonically: minus sign for negatives,
// no leading zeros, fractional part trimmed of trailing zeros, no decimal
// point for whole values. parseFixed4(formatFixed4(v)) == v for every int64.
func formatFixed4(v int64) string {
	var u uint64
	neg := v < 0
	if neg {
		u = uint64(-(v + 1)) + 1 // safe for MinInt64
	} else {
		u = uint64(v)
	}
	ip := u / FixedScale
	fp := u % FixedScale

	var b []byte
	if neg {
		b = append(b, '-')
	}
	b = strconv.AppendUint(b, ip, 10)
	if fp != 0 {
		frac := [fixedFracDigits]byte{}
		for k := fixedFracDigits - 1; k >= 0; k-- {
			frac[k] = byte('0' + fp%10)
			fp /= 10
		}
		end := fixedFracDigits
		for end > 0 && frac[end-1] == '0' {
			end--
		}
		b = append(b, '.')
		b = append(b, frac[:end]...)
	}
	return string(b)
}

// roundHalfEvenDiv divides v by d (a positive even power of ten) rounding
// half-to-even. Cannot overflow for d > 1.
func roundHalfEvenDiv(v, d int64) int64 {
	q := v / d
	r := v % d
	if r == 0 {
		return q
	}
	ar := r
	if ar < 0 {
		ar = -ar
	}
	half := d / 2
	if ar > half || (ar == half && q%2 != 0) {
		if v < 0 {
			q--
		} else {
			q++
		}
	}
	return q
}

// formatFixedDP renders v (1e-4 units) with exactly dp decimal places.
// dp < 4 rounds half-to-even; dp > 4 pads with zeros; dp < 0 is treated as 0.
func formatFixedDP(v int64, dp int) string {
	if dp < 0 {
		dp = 0
	}
	if dp >= fixedFracDigits {
		s := formatFixedExact(v)
		// formatFixedExact always emits exactly 4 decimals; pad if needed.
		for k := fixedFracDigits; k < dp; k++ {
			s += "0"
		}
		return s
	}
	scaled := roundHalfEvenDiv(v, pow10tab[fixedFracDigits-dp])
	var u uint64
	neg := scaled < 0
	if neg {
		u = uint64(-(scaled + 1)) + 1
	} else {
		u = uint64(scaled)
	}
	div := uint64(pow10tab[dp])
	ip := u / div
	fp := u % div

	var b []byte
	if neg {
		b = append(b, '-')
	}
	b = strconv.AppendUint(b, ip, 10)
	if dp > 0 {
		b = append(b, '.')
		for k := dp - 1; k >= 0; k-- {
			b = append(b, byte('0'+(fp/uint64(pow10tab[k]))%10))
		}
	}
	return string(b)
}

// formatFixedExact renders v with all 4 decimal places, no trimming.
func formatFixedExact(v int64) string {
	var u uint64
	neg := v < 0
	if neg {
		u = uint64(-(v + 1)) + 1
	} else {
		u = uint64(v)
	}
	ip := u / FixedScale
	fp := u % FixedScale
	var b []byte
	if neg {
		b = append(b, '-')
	}
	b = strconv.AppendUint(b, ip, 10)
	b = append(b, '.')
	for k := fixedFracDigits - 1; k >= 0; k-- {
		b = append(b, byte('0'+(fp/uint64(pow10tab[k]))%10))
	}
	return string(b)
}

// fixed4FromFloat converts a float64 to 1e-4 fixed point via the
// decimal-string bridge (§1.2): take the float's shortest round-trip decimal
// representation, then quantize to 4 decimals rounding HALF-TO-EVEN
// (ROUND_HALF_EVEN).
func fixed4FromFloat(f float64) (int64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: %v", ErrNotFinite, f)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	n, err := parseFixed4(s, roundHalfEven)
	if err != nil {
		return 0, fmt.Errorf("converting float64 %v: %w", f, err)
	}
	return n, nil
}

// fixed4ToFloat converts 1e-4 fixed point to the nearest float64 with a
// single correctly-rounded step (via the exact decimal string), correct for
// every representable value, including those beyond 2^53 raw units where a
// naive float64(v)/1e4 would double-round.
func fixed4ToFloat(v int64) float64 {
	f, err := strconv.ParseFloat(formatFixed4(v), 64)
	if err != nil {
		// Unreachable: formatFixed4 always yields a valid decimal literal.
		return float64(v) / FixedScale
	}
	return f
}

// ---------------------------------------------------------------------------
// round(float, ndigits)
// ---------------------------------------------------------------------------

// PyRound rounds x to ndigits decimal places for ndigits >= 0:
// correctly-rounded, half-to-even on the EXACT binary value of x (Go's strconv
// fixed-precision formatting uses the correctly-rounded ties-to-even
// algorithm). NaN and ±Inf are returned unchanged. Negative ndigits is not
// used anywhere and returns ErrInvalidArgument.
//
// Pinned by golden vectors (see testdata/pyround_golden.txt).
func PyRound(x float64, ndigits int) (float64, error) {
	if ndigits < 0 {
		return 0, fmt.Errorf("%w: PyRound ndigits must be >= 0, got %d", ErrInvalidArgument, ndigits)
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x, nil
	}
	s := strconv.FormatFloat(x, 'f', ndigits, 64)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Unreachable: FormatFloat output is always parseable.
		return 0, fmt.Errorf("%w: %q", ErrInvalidNumber, s)
	}
	return v, nil
}

// PyRound4 is the spec-mandated pyround4 helper (§1.3): round(x, 4), used by
// SEPA stop computation. Equivalent to PyRound(x, 4).
func PyRound4(x float64) float64 {
	v, _ := PyRound(x, 4) // ndigits is constant and valid; no error path
	return v
}
