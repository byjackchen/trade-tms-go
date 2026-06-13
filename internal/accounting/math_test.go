package accounting

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestRoundHalfEvenDiv(t *testing.T) {
	// ties round to even
	assert.Equal(t, int64(2), roundHalfEvenDiv(5, 2))   // 2.5 -> 2 (even)
	assert.Equal(t, int64(4), roundHalfEvenDiv(7, 2))   // 3.5 -> 4 (even)
	assert.Equal(t, int64(2), roundHalfEvenDiv(3, 2))   // 1.5 -> 2 (even)
	assert.Equal(t, int64(3), roundHalfEvenDiv(8, 3))   // 2.66 -> 3
	assert.Equal(t, int64(-2), roundHalfEvenDiv(-5, 2)) // -2.5 -> -2 (even)
}

func TestMulDivRoundHalfEven(t *testing.T) {
	// 1000 * 1 / 3 = 333.33 -> 333
	assert.Equal(t, int64(333), mulDivRoundHalfEven(1000, 1, 3))
	// 1000 * 2 / 3 = 666.66 -> 667
	assert.Equal(t, int64(667), mulDivRoundHalfEven(1000, 2, 3))
	// exact half to even: 5 * 1 / 2 = 2.5 -> 2
	assert.Equal(t, int64(2), mulDivRoundHalfEven(5, 1, 2))
	// negative cost basis stays symmetric
	assert.Equal(t, int64(-333), mulDivRoundHalfEven(-1000, 1, 3))
	// no overflow with large notional
	big := int64(900000000000000) // 9e14 in 1e-4 = 9e10 dollars
	assert.Equal(t, big/2, mulDivRoundHalfEven(big, 1, 2))
}

func TestRoundMoneyToCents(t *testing.T) {
	// 299.8333 -> 299.83
	assert.Equal(t, domain.MustMoney("299.83"), roundMoneyToCents(domain.MustMoney("299.8333")))
	// -400.1667 -> -400.17
	assert.Equal(t, domain.MustMoney("-400.17"), roundMoneyToCents(domain.MustMoney("-400.1667")))
	// half-even tie at the mill: 1.005 (10050 in 1e-4) -> 1.00 (even)
	assert.Equal(t, domain.MustMoney("1.00"), roundMoneyToCents(domain.MustMoney("1.005")))
	// 1.015 -> 1.02 (even)
	assert.Equal(t, domain.MustMoney("1.02"), roundMoneyToCents(domain.MustMoney("1.015")))
	// already-cents value unchanged.
	assert.Equal(t, domain.MustMoney("150.00"), roundMoneyToCents(domain.MustMoney("150.00")))
}

func TestSameSign(t *testing.T) {
	assert.True(t, sameSign(domain.Qty(5), domain.Qty(3)))
	assert.True(t, sameSign(domain.Qty(-5), domain.Qty(-3)))
	assert.False(t, sameSign(domain.Qty(5), domain.Qty(-3)))
	assert.True(t, sameSign(domain.Qty(0), domain.Qty(0)))
	assert.False(t, sameSign(domain.Qty(5), domain.Qty(0)))
}
