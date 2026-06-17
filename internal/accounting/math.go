package accounting

// math.go holds the small exact-integer helpers the position arithmetic needs:
// signed-quantity sign tests and absolute values, half-to-even integer
// division, and a 128-bit-intermediate mul/div used for proportional cost-basis
// release. All rounding is ROUND_HALF_EVEN to match the project-wide money
// rounding (locked decision 1).

import (
	"math/big"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// sameSign reports whether a and b have the same sign, treating 0 as matching
// neither positive nor negative (so sameSign(x,0) is false unless x is 0).
func sameSign(a, b domain.Qty) bool {
	if a == 0 || b == 0 {
		return a == b
	}
	return (a > 0) == (b > 0)
}

// absQty returns |q| as int64. domain.Qty MinInt64 is not reachable for real
// share counts; the engine never constructs such a value.
func absQty(q domain.Qty) int64 {
	n := int64(q)
	if n < 0 {
		return -n
	}
	return n
}

// absQtyVal returns |q| as a domain.Qty.
func absQtyVal(q domain.Qty) domain.Qty {
	if q < 0 {
		return -q
	}
	return q
}

// roundMoneyToCents quantizes a 1e-4 Money to 2 decimal places (cents),
// rounding half-to-even. USD Money has cent precision, to which realized PnL
// is quantized on every fill.
func roundMoneyToCents(m domain.Money) domain.Money {
	// 1e-4 units -> cents: divide by 100 with half-even, then scale back.
	cents := roundHalfEvenDiv(int64(m), 100)
	return domain.Money(cents * 100)
}

// roundHalfEvenDiv divides v by d (d > 0) rounding the quotient to the nearest
// integer, ties to even. Used to materialize an average price (notional/qty).
func roundHalfEvenDiv(v, d int64) int64 {
	if d == 0 {
		return 0
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	q := v / d
	r := v % d
	twice := r * 2
	switch {
	case twice > d:
		q++
	case twice == d:
		if q%2 != 0 {
			q++ // round half to even
		}
	}
	if neg {
		q = -q
	}
	return q
}

// mulDivRoundHalfEven computes round_half_even(v * num / den) with den > 0,
// using a big.Int intermediate so v*num cannot overflow int64. v may be
// negative; num and den are non-negative share counts. Returns an int64 in
// 1e-4 money units.
func mulDivRoundHalfEven(v, num, den int64) int64 {
	if den == 0 || num == 0 {
		return 0
	}
	bv := big.NewInt(v)
	bn := big.NewInt(num)
	bd := big.NewInt(den)
	prod := new(big.Int).Mul(bv, bn)

	// Work on magnitude, restore sign at the end (round-half-even is symmetric).
	neg := prod.Sign() < 0
	prod.Abs(prod)

	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(prod, bd, r)

	twice := new(big.Int).Lsh(r, 1) // 2*r
	cmp := twice.Cmp(bd)
	if cmp > 0 {
		q.Add(q, big.NewInt(1))
	} else if cmp == 0 {
		// tie: round to even
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	if neg {
		q.Neg(q)
	}
	return q.Int64()
}
