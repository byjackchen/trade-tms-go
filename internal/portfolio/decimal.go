package portfolio

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

func (d dec) Add(o dec) dec { return dec{r: new(big.Rat).Add(d.r, o.r)} }
func (d dec) Sub(o dec) dec { return dec{r: new(big.Rat).Sub(d.r, o.r)} }
func (d dec) Mul(o dec) dec { return dec{r: new(big.Rat).Mul(d.r, o.r)} }
func (d dec) Neg() dec      { return dec{r: new(big.Rat).Neg(d.r)} }

// Cmp returns -1, 0, +1 comparing d to o.
func (d dec) Cmp(o dec) int { return d.r.Cmp(o.r) }

// Sign returns -1, 0, +1.
func (d dec) Sign() int { return d.r.Sign() }

// Float64 returns the nearest float64 (for health snapshots/reporting only;
// never used inside a gating comparison).
func (d dec) Float64() float64 { f, _ := d.r.Float64(); return f }

// String renders the value with up to 12 fractional digits, trailing zeros
// trimmed — for reasons/logs only, never for comparison.
func (d dec) String() string {
	if d.r == nil {
		return "0"
	}
	return d.r.FloatString(2)
}
