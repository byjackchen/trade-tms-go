package riskgate

// decimal.go provides `dec`, an exact rational money value (math/big.Rat) used
// throughout the risk pipeline. It exists so the Go pipeline reproduces the
// comparison results of CPython's `decimal.Decimal` exactly.
//
// Why exact rationals and not float64 / the 1e-4 fixed-point domain.Money:
// the Python pipeline multiplies share counts by `Decimal(str(pct))` fractions
// (e.g. nav * Decimal("0.2")) and by 2-dp prices, then compares with strict
// </>. A float64 would drift; domain.Money's 4-dp quantization would truncate
// products like nav*0.05 that Python keeps exact. big.Rat keeps every
// intermediate exact, and since the Python decimal context (28 digits) never
// rounds for the magnitudes used here, the comparison results are identical.

import (
	"math/big"
	"strconv"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// dec is an exact decimal/rational value. The zero value is unusable; build via
// the constructors below (decZero, decFromInt, decFromPctFloat, DecFromMoney,
// DecFromPrice, ParseDec).
type dec struct {
	r *big.Rat
}

func decZero() dec { return dec{r: new(big.Rat)} }

// decFromInt builds an exact integer dec.
func decFromInt(v int64) dec { return dec{r: new(big.Rat).SetInt64(v)} }

// decFromPctFloat builds the exact rational of `Decimal(str(f))` — Python takes
// the SHORTEST decimal repr of the float then parses it exactly. strconv with
// 'g'/-1 yields that shortest repr; parsing it into a big.Rat is exact.
func decFromPctFloat(f float64) dec {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		// 'g'/-1 always produces a parseable decimal; fall back to the exact
		// binary value if that ever fails.
		r = new(big.Rat).SetFloat64(f)
		if r == nil {
			r = new(big.Rat)
		}
	}
	return dec{r: r}
}

// ParseDec parses a decimal string exactly (e.g. "123.45", "100000").
func ParseDec(s string) (dec, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return dec{}, false
	}
	return dec{r: r}, true
}

// MustDec is ParseDec for tests/constants; panics on malformed input.
func MustDec(s string) dec {
	d, ok := ParseDec(s)
	if !ok {
		panic("portfolio: bad decimal literal " + s)
	}
	return d
}

// DecFromMoney converts a domain.Money (1e-4 fixed point) to an exact dec.
func DecFromMoney(m domain.Money) dec {
	return dec{r: new(big.Rat).SetFrac(big.NewInt(m.Raw()), big.NewInt(domain.FixedScale))}
}

// DecFromPrice converts a domain.Price (1e-4 fixed point) to an exact dec.
func DecFromPrice(p domain.Price) dec {
	return dec{r: new(big.Rat).SetFrac(big.NewInt(p.Raw()), big.NewInt(domain.FixedScale))}
}

// rat returns the backing big.Rat, treating the nil zero-value as exact 0 so a
// default-constructed dec (e.g. an unset PortfolioSnapshot field) behaves as 0
// rather than panicking. All arithmetic and comparison flow through this.
func (d dec) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

func (d dec) Add(o dec) dec { return dec{r: new(big.Rat).Add(d.rat(), o.rat())} }
func (d dec) Sub(o dec) dec { return dec{r: new(big.Rat).Sub(d.rat(), o.rat())} }
func (d dec) Mul(o dec) dec { return dec{r: new(big.Rat).Mul(d.rat(), o.rat())} }
func (d dec) Neg() dec      { return dec{r: new(big.Rat).Neg(d.rat())} }

// decPrec mirrors CPython's default decimal context precision (28 significant
// digits). Division in the health snapshot (day_pnl/nav, headroom/nav,
// value/nav) is the only place the pipeline rounds; +, -, * stay exact. We
// round the quotient to 28 significant digits with banker's rounding
// (ROUND_HALF_EVEN, the Python decimal default) so numeric comparisons in the
// parity suite match.
const decPrec = 28

// Quo returns d / o rounded to 28 significant digits (ROUND_HALF_EVEN),
// mirroring CPython decimal.Decimal division under the default context.
// Caller guarantees o != 0 (the pipeline only divides when nav > 0).
func (d dec) Quo(o dec) dec {
	q := new(big.Rat).Quo(d.rat(), o.rat())
	if q.Sign() == 0 || q.IsInt() {
		// Exact integer (incl. zero) — no rounding needed.
		return dec{r: q}
	}
	// Determine the decimal exponent of the most significant digit so we can
	// round to 28 significant digits = (28 - 1 - msdExp) fractional digits.
	abs := new(big.Rat).Abs(q)
	// number of integer digits of the absolute value (>=1)
	intPart := new(big.Int).Quo(abs.Num(), abs.Denom())
	var msd int // position of most significant digit (0 = units place)
	if intPart.Sign() > 0 {
		msd = len(intPart.String()) - 1
	} else {
		// value in (0,1): find first non-zero fractional digit.
		// scale up by 10 until integer part is non-zero.
		msd = 0
		scaled := new(big.Rat).Set(abs)
		ten := big.NewRat(10, 1)
		for new(big.Int).Quo(scaled.Num(), scaled.Denom()).Sign() == 0 {
			scaled.Mul(scaled, ten)
			msd--
		}
	}
	frac := decPrec - 1 - msd
	if frac < 0 {
		frac = 0
	}
	return dec{r: ratRoundHalfEven(q, frac)}
}

// ratRoundHalfEven rounds r to `frac` decimal places using banker's rounding
// and returns the result as an exact big.Rat.
func ratRoundHalfEven(r *big.Rat, frac int) *big.Rat {
	// scale = 10^frac
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(frac)), nil)
	scaledRat := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	num := scaledRat.Num()
	den := scaledRat.Denom()
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, den, rem) // truncated toward zero
	if rem.Sign() != 0 {
		// twice |rem| vs den decides rounding direction
		twice := new(big.Int).Abs(rem)
		twice.Lsh(twice, 1)
		cmp := twice.Cmp(den.Abs(den))
		roundUp := false
		switch {
		case cmp > 0:
			roundUp = true
		case cmp < 0:
			roundUp = false
		default: // exactly half -> round to even
			if q.Bit(0) == 1 {
				roundUp = true
			}
		}
		if roundUp {
			if scaledRat.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	// result = q / scale
	return new(big.Rat).SetFrac(q, scale)
}

// Cmp returns -1, 0, +1 comparing d to o.
func (d dec) Cmp(o dec) int { return d.rat().Cmp(o.rat()) }

// Sign returns -1, 0, +1.
func (d dec) Sign() int { return d.rat().Sign() }

// Float64 returns the nearest float64 (for health snapshots/reporting only;
// never used inside a gating comparison).
func (d dec) Float64() float64 { f, _ := d.rat().Float64(); return f }

// String renders the value with up to 12 fractional digits, trailing zeros
// trimmed — for reasons/logs only, never for comparison.
func (d dec) String() string {
	if d.r == nil {
		return "0"
	}
	return d.r.FloatString(2)
}
