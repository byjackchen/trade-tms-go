package orb

// pydec.go implements a Python-`decimal.Decimal`-faithful fixed-point type
// used ONLY by the ORB strategy, so that on_bar reasons, state_summary and
// state_dict render byte-identically to the Python reference.
//
// Why not domain.Price (1e-4 fixed point)? The Python ORB SignalGenerator
// carries scale through arithmetic the way CPython's Decimal does:
//
//   - construction from str preserves the literal's scale
//     (Decimal("102.0") has exponent -1, str() == "102.0");
//   - multiply: result exponent = sum of operand exponents
//     (102.0 [exp -1] * 0.99 [exp -2] -> 100.980 [exp -3]);
//   - add/sub:  result exponent = min(operand exponents) i.e. the larger
//     fractional scale, with the coefficient aligned (102.0 - 100.980 ->
//     1.020 [exp -3]);
//   - true division uses the 28-significant-digit context and strips
//     trailing zeros down to the "ideal exponent" (1.0/100 -> 0.01).
//
// domain.Price.String() trims trailing zeros ("100.980" -> "100.98") and is
// capped at 4 decimals, so it cannot reproduce these strings. pydec keeps an
// explicit (coefficient, exponent) pair exactly like CPython Decimal.
//
// Only the operations ORB actually performs are implemented: parse-from-str,
// compare, add, sub, mul, scalar-divide (x/100 in the hard-stop path and the
// proximity/strength paths), Float64, and the str() renderer. Values stay
// well within int64/float64 ranges for equity-strategy prices; the
// coefficient is a math/big.Int to be safe against scale growth.

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// decContextPrec mirrors CPython's default decimal context precision
// (getcontext().prec == 28). Division rounds to this many significant digits.
const decContextPrec = 28

// pydec is coefficient * 10**exp, matching CPython Decimal's internal form.
// A nil receiver is never produced; the zero value is unusable — always build
// via mustDec / parseDec / decFromInt.
type pydec struct {
	coef *big.Int // signed coefficient (significand), no leading-sign separation
	exp  int      // power of ten; value == coef * 10**exp
}

// parseDec parses a plain decimal literal (the only forms ORB ever sees:
// "102.0", "99", "1.5", "-0.0001"). No exponent notation. Returns ok=false on
// malformed input.
func parseDec(s string) (pydec, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return pydec{}, false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return pydec{}, false
	}
	intPart := s
	fracPart := ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	}
	digits := intPart + fracPart
	if digits == "" {
		return pydec{}, false
	}
	coef, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return pydec{}, false
	}
	if neg {
		coef.Neg(coef)
	}
	return pydec{coef: coef, exp: -len(fracPart)}, true
}

// mustDec parses s or panics (for in-package constants/tests).
func mustDec(s string) pydec {
	d, ok := parseDec(s)
	if !ok {
		panic("orb: bad decimal literal " + s)
	}
	return d
}

// decFromInt builds an integer-valued pydec with exponent 0 (matches
// Python Decimal(int)).
func decFromInt(v int64) pydec {
	return pydec{coef: big.NewInt(v), exp: 0}
}

// decFromPyFloatStr builds Decimal(str(f)) — the float's shortest repr parsed
// exactly, exactly as the Python SG does for config knobs (hard_stop_pct,
// profit_target_r). Go's strconv 'g'/-1 yields the same shortest repr CPython
// uses for str(float).
func decFromPyFloatStr(f float64) pydec {
	d, ok := parseDec(pyFloatRepr(f))
	if !ok {
		return decFromInt(0)
	}
	return d
}

func (d pydec) isZero() bool { return d.coef == nil || d.coef.Sign() == 0 }

// align returns both coefficients scaled to the common (smaller/more
// negative) exponent, plus that exponent.
func align(a, b pydec) (ca, cb *big.Int, exp int) {
	if a.exp == b.exp {
		return new(big.Int).Set(a.coef), new(big.Int).Set(b.coef), a.exp
	}
	exp = a.exp
	if b.exp < exp {
		exp = b.exp
	}
	ca = scaleCoef(a, exp)
	cb = scaleCoef(b, exp)
	return ca, cb, exp
}

// scaleCoef returns d's coefficient expressed at the target (lower) exponent.
func scaleCoef(d pydec, target int) *big.Int {
	out := new(big.Int).Set(d.coef)
	if d.exp == target {
		return out
	}
	// d.exp > target: multiply coef by 10**(d.exp-target).
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.exp-target)), nil)
	return out.Mul(out, pow)
}

// cmp returns -1, 0, +1 comparing the *values* (scale-independent), exactly
// like Python Decimal comparison.
func (d pydec) cmp(o pydec) int {
	ca, cb, _ := align(d, o)
	return ca.Cmp(cb)
}

// add returns d + o with exponent = min(d.exp, o.exp) (Python Decimal rule).
func (d pydec) add(o pydec) pydec {
	ca, cb, exp := align(d, o)
	return pydec{coef: ca.Add(ca, cb), exp: exp}
}

// sub returns d - o with exponent = min(d.exp, o.exp).
func (d pydec) sub(o pydec) pydec {
	ca, cb, exp := align(d, o)
	return pydec{coef: ca.Sub(ca, cb), exp: exp}
}

// mul returns d * o; exponent = d.exp + o.exp (Python Decimal rule, no
// trailing-zero stripping for multiply).
func (d pydec) mul(o pydec) pydec {
	return pydec{coef: new(big.Int).Mul(d.coef, o.coef), exp: d.exp + o.exp}
}

// divInt returns d / n where n is a small positive integer, replicating
// CPython true-division: compute to decContextPrec significant digits, then
// strip trailing zeros toward the ideal exponent. For ORB the only division
// is x/100, which is always exact and short (e.g. 1.0/100 -> 0.01).
func (d pydec) divInt(n int64) pydec {
	return d.div(decFromInt(n))
}

// div implements CPython Decimal true division for the operand magnitudes ORB
// uses. It produces up to decContextPrec significant digits and then removes
// trailing zeros down to (but not past) the ideal exponent = d.exp - o.exp.
func (d pydec) div(o pydec) pydec {
	if o.isZero() {
		// ORB never divides by zero on any live path; return zero defensively.
		return decFromInt(0)
	}
	if d.isZero() {
		return pydec{coef: big.NewInt(0), exp: d.exp - o.exp}
	}
	idealExp := d.exp - o.exp

	num := new(big.Int).Abs(d.coef)
	den := new(big.Int).Abs(o.coef)
	neg := (d.coef.Sign() < 0) != (o.coef.Sign() < 0)

	// We want a quotient coefficient with exactly `prec` significant digits
	// (or fewer if it terminates earlier and we can reach the ideal exponent).
	// Strategy: scale numerator up until the integer quotient has >= prec
	// digits, then round half-even, tracking the resulting exponent.
	shift := 0
	q := new(big.Int)
	r := new(big.Int)
	ten := big.NewInt(10)

	// Bring numerator >= denominator so the quotient has at least 1 digit.
	for num.Cmp(den) < 0 {
		num.Mul(num, ten)
		shift++
	}
	q.DivMod(num, den, r)
	// q now has the leading digits; extend until prec significant digits.
	for numDigits(q) < decContextPrec && r.Sign() != 0 {
		num.Mul(r, ten) // continue long division from remainder
		q.Mul(q, ten)
		qd := new(big.Int)
		qd.DivMod(num, den, r)
		q.Add(q, qd)
		shift++
	}
	// resultExp = d.exp - o.exp - shift  (we multiplied numerator by 10**shift)
	resultExp := idealExp - shift

	// Round to prec significant digits half-even if we overshot.
	if numDigits(q) > decContextPrec {
		drop := numDigits(q) - decContextPrec
		q, resultExp = roundHalfEven(q, resultExp, drop)
	} else if r.Sign() != 0 {
		// Remainder still nonzero at prec digits: round last digit half-even
		// using the remainder vs denominator.
		q, resultExp = roundRemainderHalfEven(q, resultExp, r, den)
	}

	if neg {
		q = new(big.Int).Neg(q)
	}
	res := pydec{coef: q, exp: resultExp}
	// Strip trailing zeros down to the ideal exponent (Python's behaviour for
	// exact divisions: 1.0/100 -> 0.01, not 0.0100000...).
	res = res.stripTrailingZerosTo(idealExp)
	return res
}

// roundHalfEven drops `drop` least-significant digits from q, rounding
// half-to-even, and raises the exponent accordingly.
func roundHalfEven(q *big.Int, exp, drop int) (*big.Int, int) {
	if drop <= 0 {
		return q, exp
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(drop)), nil)
	quo := new(big.Int)
	rem := new(big.Int)
	quo.DivMod(new(big.Int).Abs(q), pow, rem)
	half := new(big.Int).Rsh(pow, 1) // pow/2 (pow is a power of ten, even)
	cmp := rem.Cmp(half)
	roundUp := false
	if cmp > 0 {
		roundUp = true
	} else if cmp == 0 {
		// exactly half: round to even
		if quo.Bit(0) == 1 {
			roundUp = true
		}
	}
	if roundUp {
		quo.Add(quo, big.NewInt(1))
	}
	if q.Sign() < 0 {
		quo.Neg(quo)
	}
	return quo, exp + drop
}

// roundRemainderHalfEven applies a final half-even rounding decision based on
// the leftover remainder r over denominator den (2*r vs den).
func roundRemainderHalfEven(q *big.Int, exp int, r, den *big.Int) (*big.Int, int) {
	twice := new(big.Int).Lsh(r, 1) // 2*r
	cmp := twice.Cmp(den)
	roundUp := false
	if cmp > 0 {
		roundUp = true
	} else if cmp == 0 {
		if q.Bit(0) == 1 {
			roundUp = true
		}
	}
	if roundUp {
		if q.Sign() < 0 {
			q = new(big.Int).Sub(q, big.NewInt(1))
		} else {
			q = new(big.Int).Add(q, big.NewInt(1))
		}
	}
	return q, exp
}

// stripTrailingZerosTo removes factors of ten from the coefficient, raising
// the exponent, but never past `floorExp` (Python keeps the ideal exponent
// for exact quotients).
func (d pydec) stripTrailingZerosTo(floorExp int) pydec {
	if d.coef.Sign() == 0 {
		if d.exp < floorExp {
			return pydec{coef: big.NewInt(0), exp: floorExp}
		}
		return d
	}
	coef := new(big.Int).Set(d.coef)
	exp := d.exp
	ten := big.NewInt(10)
	q := new(big.Int)
	r := new(big.Int)
	for exp < floorExp {
		q.DivMod(coef, ten, r)
		if r.Sign() != 0 {
			break
		}
		coef.Set(q)
		exp++
	}
	return pydec{coef: coef, exp: exp}
}

// numDigits returns the number of decimal digits in |n| (0 -> 1).
func numDigits(n *big.Int) int {
	if n.Sign() == 0 {
		return 1
	}
	return len(new(big.Int).Abs(n).Text(10))
}

// float64 converts to the nearest float64 the same way Python float(Decimal)
// does: build the exact decimal string and parse it (strconv.ParseFloat is
// correctly rounded, matching CPython's float parse).
func (d pydec) float64() float64 {
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// pyFloatRepr renders f exactly as CPython's repr(float)/str(float) does.
// Used for the {vol_multiple} field in the breakout reason and (via
// Decimal(str(f))) for the hard_stop_pct / profit_target_r knobs. Mirrors the
// SEPA package's implementation (kept local to avoid cross-package coupling of
// an unexported helper).
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
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	es := strconv.FormatFloat(f, 'e', -1, 64)
	neg := false
	if es[0] == '-' {
		neg = true
		es = es[1:]
	}
	eIdx := strings.IndexByte(es, 'e')
	mant := es[:eIdx]
	exp, _ := strconv.Atoi(es[eIdx+1:])
	var digits string
	if dot := strings.IndexByte(mant, '.'); dot >= 0 {
		digits = mant[:dot] + mant[dot+1:]
	} else {
		digits = mant
	}
	out := formatPyDigits(digits, exp)
	if neg {
		return "-" + out
	}
	return out
}

// formatPyDigits renders significant digits per CPython's repr threshold:
// scientific when exp < -4 or exp >= 16, else positional with a trailing ".0"
// for integral positional results.
func formatPyDigits(digits string, exp int) string {
	n := len(digits)
	if exp < -4 || exp >= 16 {
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
		esd := strconv.Itoa(ea)
		if len(esd) < 2 {
			esd = "0" + esd
		}
		sb.WriteString(esd)
		return sb.String()
	}
	if exp >= 0 {
		if exp+1 >= n {
			return digits + strings.Repeat("0", exp+1-n) + ".0"
		}
		return digits[:exp+1] + "." + digits[exp+1:]
	}
	return "0." + strings.Repeat("0", -exp-1) + digits
}

// pyFmt0 renders f like Python f"{f:.0f}" (round-half-even to an integer
// string). Used for the {avg_volume:.0f} field in the breakout reason.
func pyFmt0(f float64) string {
	return strconv.FormatFloat(f, 'f', 0, 64)
}

// String renders exactly like CPython str(Decimal) for the non-exponential
// magnitudes ORB uses (always |exp| small). Negative exponent -> fixed-point
// with that many fractional digits; non-negative exponent -> integer padded
// with trailing zeros (ORB never hits exp>0 on a rendered value, but handle
// it for completeness).
func (d pydec) String() string {
	if d.coef == nil {
		return "0"
	}
	neg := d.coef.Sign() < 0
	digits := new(big.Int).Abs(d.coef).Text(10)

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	switch {
	case d.exp == 0:
		b.WriteString(digits)
	case d.exp > 0:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", d.exp))
	default: // d.exp < 0
		frac := -d.exp
		if len(digits) <= frac {
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", frac-len(digits)))
			b.WriteString(digits)
		} else {
			split := len(digits) - frac
			b.WriteString(digits[:split])
			b.WriteByte('.')
			b.WriteString(digits[split:])
		}
	}
	return b.String()
}
