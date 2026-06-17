package pairs

// decimalstr.go: the decimal-string bridge and tiny date helpers.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// pyDecimalStr renders str(Decimal(str(float(price)))) — the form in which
// closes are serialized by state_dict. For the <=4dp Price domain the float64
// round-trips losslessly, and the shortest decimal repr is the canonical form.
// Unlike domain.Price.String() (which renders 100.0 as "100"), this keeps the
// ".0" on integer-valued closes.
//
// Algorithm: take the shortest float repr (strconv 'g'/-1), then if it has
// neither a '.' nor an exponent, append ".0" — what str(float) does for
// integer-valued floats.
func pyDecimalStr(p domain.Price) string {
	f := p.Float64()
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func monthFromInt(m int) time.Month {
	return time.Month(m)
}

func errBadDate(s string) error {
	return fmt.Errorf("pairs: malformed ISO date %q", s)
}
