package domain

// money.go defines the three numeric value types used across the system:
//
//   Price — per-share price, int64 fixed point at 1e-4 (4 decimal places).
//   Money — monetary amount (USD), int64 fixed point at 1e-4.
//   Qty   — whole signed share count (positive long, negative short, 0 flat).
//
// All three are immutable value types (methods take value receivers and
// return new values). Arithmetic is overflow-checked and returns errors —
// never panics, never silent wraparound.
//
// JSON encoding round-trips exactly:
//   - Price/Money marshal as canonical decimal strings ("123.45").
//     Unmarshal accepts either a JSON string or a raw JSON number token; the
//     token text is parsed exactly as a decimal (never through float64).
//   - Qty marshals as a JSON number (shares are integers).

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
)

// ---------------------------------------------------------------------------
// Price
// ---------------------------------------------------------------------------

// Price is a per-share price in 1e-4 currency units (e.g. Price(1234500)
// is 123.45). The zero value is 0.0000.
type Price int64

// ParsePrice parses a decimal literal exactly; values not representable at
// 1e-4 scale yield ErrInexact, malformed input ErrInvalidNumber, and
// out-of-range input ErrOverflow.
func ParsePrice(s string) (Price, error) {
	n, err := parseFixed4(s, roundExact)
	if err != nil {
		return 0, fmt.Errorf("parsing price: %w", err)
	}
	return Price(n), nil
}

// MustPrice is ParsePrice for compile-time constants and tests; it panics on
// error and must not be used on runtime inputs.
func MustPrice(s string) Price {
	p, err := ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return p
}

// PriceFromFloat64 converts a float64 via the decimal-string bridge:
// shortest-repr decimal digits, then quantized to 4 decimals rounding
// half-to-even (ROUND_HALF_EVEN; see fixed.go).
func PriceFromFloat64(f float64) (Price, error) {
	n, err := fixed4FromFloat(f)
	if err != nil {
		return 0, fmt.Errorf("price: %w", err)
	}
	return Price(n), nil
}

// PriceFromInt converts a whole currency amount (e.g. dollars) to a Price.
func PriceFromInt(units int64) (Price, error) {
	n, ok := checkedMul64(units, FixedScale)
	if !ok {
		return 0, fmt.Errorf("price from %d: %w", units, ErrOverflow)
	}
	return Price(n), nil
}

// Raw returns the underlying 1e-4 unit count.
func (p Price) Raw() int64 { return int64(p) }

// Float64 returns the nearest float64, correctly rounded in a single step.
func (p Price) Float64() float64 { return fixed4ToFloat(int64(p)) }

// String renders the canonical decimal form: trailing zeros trimmed, no
// decimal point for whole values ("123.45", "70.5", "100", "-0.0001").
// ParsePrice(p.String()) == p for every value.
func (p Price) String() string { return formatFixed4(int64(p)) }

// StringFixed renders with exactly dp decimal places; dp < 4 rounds
// half-to-even, dp > 4 pads zeros, dp < 0 is treated as 0.
func (p Price) StringFixed(dp int) string { return formatFixedDP(int64(p), dp) }

// Add returns p + o, or ErrOverflow.
func (p Price) Add(o Price) (Price, error) {
	n, ok := checkedAdd64(int64(p), int64(o))
	if !ok {
		return 0, fmt.Errorf("price %s + %s: %w", p, o, ErrOverflow)
	}
	return Price(n), nil
}

// Sub returns p - o, or ErrOverflow.
func (p Price) Sub(o Price) (Price, error) {
	n, ok := checkedSub64(int64(p), int64(o))
	if !ok {
		return 0, fmt.Errorf("price %s - %s: %w", p, o, ErrOverflow)
	}
	return Price(n), nil
}

// Neg returns -p, or ErrOverflow for the MinInt64 value.
func (p Price) Neg() (Price, error) {
	n, ok := checkedNeg64(int64(p))
	if !ok {
		return 0, fmt.Errorf("negating price %s: %w", p, ErrOverflow)
	}
	return Price(n), nil
}

// Abs returns |p|, or ErrOverflow for the MinInt64 value.
func (p Price) Abs() (Price, error) {
	n, ok := checkedAbs64(int64(p))
	if !ok {
		return 0, fmt.Errorf("abs of price %s: %w", p, ErrOverflow)
	}
	return Price(n), nil
}

// MulQty returns the notional p × q as Money, or ErrOverflow.
func (p Price) MulQty(q Qty) (Money, error) {
	n, ok := checkedMul64(int64(p), int64(q))
	if !ok {
		return 0, fmt.Errorf("price %s * qty %d: %w", p, q, ErrOverflow)
	}
	return Money(n), nil
}

// AsMoney reinterprets the per-share price as a monetary amount (the
// notional of exactly one share). Lossless.
func (p Price) AsMoney() Money { return Money(p) }

// Cmp returns -1, 0 or +1 comparing p to o.
func (p Price) Cmp(o Price) int { return cmpInt64(int64(p), int64(o)) }

// Sign returns -1, 0 or +1.
func (p Price) Sign() int { return cmpInt64(int64(p), 0) }

// IsZero reports p == 0.
func (p Price) IsZero() bool { return p == 0 }

// IsPositive reports p > 0.
func (p Price) IsPositive() bool { return p > 0 }

// IsNegative reports p < 0.
func (p Price) IsNegative() bool { return p < 0 }

// MarshalText implements encoding.TextMarshaler (canonical decimal string).
func (p Price) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler (exact parse).
func (p *Price) UnmarshalText(b []byte) error {
	v, err := ParsePrice(string(b))
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// MarshalJSON encodes the canonical decimal string ("123.45").
func (p Price) MarshalJSON() ([]byte, error) { return marshalFixedJSON(int64(p)) }

// UnmarshalJSON accepts a JSON string or raw number token, parsed exactly.
// JSON null leaves the value unchanged (encoding/json convention).
func (p *Price) UnmarshalJSON(b []byte) error {
	v, changed, err := unmarshalFixedJSON(b, "price")
	if err != nil {
		return err
	}
	if changed {
		*p = Price(v)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

// Money is a monetary amount in 1e-4 currency units. The zero value is
// 0.0000. Range: ±922,337,203,685,477.5807.
type Money int64

// ParseMoney parses a decimal literal exactly (see ParsePrice).
func ParseMoney(s string) (Money, error) {
	n, err := parseFixed4(s, roundExact)
	if err != nil {
		return 0, fmt.Errorf("parsing money: %w", err)
	}
	return Money(n), nil
}

// MustMoney is ParseMoney for compile-time constants and tests; it panics on
// error and must not be used on runtime inputs.
func MustMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// MoneyFromFloat64 converts a float64 with Decimal(str(x)) bridge semantics
// (shortest repr, half-to-even quantize at 1e-4; see PriceFromFloat64).
func MoneyFromFloat64(f float64) (Money, error) {
	n, err := fixed4FromFloat(f)
	if err != nil {
		return 0, fmt.Errorf("money: %w", err)
	}
	return Money(n), nil
}

// MoneyFromInt converts a whole currency amount (e.g. dollars) to Money.
func MoneyFromInt(units int64) (Money, error) {
	n, ok := checkedMul64(units, FixedScale)
	if !ok {
		return 0, fmt.Errorf("money from %d: %w", units, ErrOverflow)
	}
	return Money(n), nil
}

// Raw returns the underlying 1e-4 unit count.
func (m Money) Raw() int64 { return int64(m) }

// Float64 returns the nearest float64, correctly rounded in a single step.
func (m Money) Float64() float64 { return fixed4ToFloat(int64(m)) }

// String renders the canonical decimal form (see Price.String).
func (m Money) String() string { return formatFixed4(int64(m)) }

// StringFixed renders with exactly dp decimal places (see Price.StringFixed).
func (m Money) StringFixed(dp int) string { return formatFixedDP(int64(m), dp) }

// Add returns m + o, or ErrOverflow.
func (m Money) Add(o Money) (Money, error) {
	n, ok := checkedAdd64(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("money %s + %s: %w", m, o, ErrOverflow)
	}
	return Money(n), nil
}

// Sub returns m - o, or ErrOverflow.
func (m Money) Sub(o Money) (Money, error) {
	n, ok := checkedSub64(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("money %s - %s: %w", m, o, ErrOverflow)
	}
	return Money(n), nil
}

// Neg returns -m, or ErrOverflow for the MinInt64 value.
func (m Money) Neg() (Money, error) {
	n, ok := checkedNeg64(int64(m))
	if !ok {
		return 0, fmt.Errorf("negating money %s: %w", m, ErrOverflow)
	}
	return Money(n), nil
}

// Abs returns |m|, or ErrOverflow for the MinInt64 value.
func (m Money) Abs() (Money, error) {
	n, ok := checkedAbs64(int64(m))
	if !ok {
		return 0, fmt.Errorf("abs of money %s: %w", m, ErrOverflow)
	}
	return Money(n), nil
}

// MulInt64 returns m × n, or ErrOverflow.
func (m Money) MulInt64(n int64) (Money, error) {
	v, ok := checkedMul64(int64(m), n)
	if !ok {
		return 0, fmt.Errorf("money %s * %d: %w", m, n, ErrOverflow)
	}
	return Money(v), nil
}

// Cmp returns -1, 0 or +1 comparing m to o.
func (m Money) Cmp(o Money) int { return cmpInt64(int64(m), int64(o)) }

// Sign returns -1, 0 or +1.
func (m Money) Sign() int { return cmpInt64(int64(m), 0) }

// IsZero reports m == 0.
func (m Money) IsZero() bool { return m == 0 }

// IsPositive reports m > 0.
func (m Money) IsPositive() bool { return m > 0 }

// IsNegative reports m < 0.
func (m Money) IsNegative() bool { return m < 0 }

// MarshalText implements encoding.TextMarshaler (canonical decimal string).
func (m Money) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler (exact parse).
func (m *Money) UnmarshalText(b []byte) error {
	v, err := ParseMoney(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// MarshalJSON encodes the canonical decimal string.
func (m Money) MarshalJSON() ([]byte, error) { return marshalFixedJSON(int64(m)) }

// UnmarshalJSON accepts a JSON string or raw number token, parsed exactly.
// JSON null leaves the value unchanged.
func (m *Money) UnmarshalJSON(b []byte) error {
	v, changed, err := unmarshalFixedJSON(b, "money")
	if err != nil {
		return err
	}
	if changed {
		*m = Money(v)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Qty
// ---------------------------------------------------------------------------

// Qty is a whole, signed share count: positive = long, negative = short,
// 0 = flat.
type Qty int64

// ParseQty parses a base-10 integer share count.
func ParseQty(s string) (Qty, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: parsing qty %q", ErrInvalidNumber, s)
	}
	return Qty(n), nil
}

// QtyFromFloat64Trunc converts a float64 share count by TRUNCATING toward
// zero, as applied to Position.signed_qty and all share-sizing paths (§1.3).
// NaN, ±Inf and out-of-range values error.
func QtyFromFloat64Trunc(f float64) (Qty, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("qty: %w: %v", ErrNotFinite, f)
	}
	t := math.Trunc(f)
	// 2^63 as float64 is exact; MinInt64 (-2^63) is representable, +2^63 is not.
	if t >= 9223372036854775808.0 || t < -9223372036854775808.0 {
		return 0, fmt.Errorf("qty from %v: %w", f, ErrOverflow)
	}
	return Qty(t), nil
}

// Int64 returns the share count as int64.
func (q Qty) Int64() int64 { return int64(q) }

// Float64 returns the share count as float64 (exact below 2^53 shares).
func (q Qty) Float64() float64 { return float64(q) }

// String renders the base-10 integer.
func (q Qty) String() string { return strconv.FormatInt(int64(q), 10) }

// Add returns q + o, or ErrOverflow.
func (q Qty) Add(o Qty) (Qty, error) {
	n, ok := checkedAdd64(int64(q), int64(o))
	if !ok {
		return 0, fmt.Errorf("qty %d + %d: %w", q, o, ErrOverflow)
	}
	return Qty(n), nil
}

// Sub returns q - o, or ErrOverflow.
func (q Qty) Sub(o Qty) (Qty, error) {
	n, ok := checkedSub64(int64(q), int64(o))
	if !ok {
		return 0, fmt.Errorf("qty %d - %d: %w", q, o, ErrOverflow)
	}
	return Qty(n), nil
}

// Neg returns -q, or ErrOverflow for the MinInt64 value.
func (q Qty) Neg() (Qty, error) {
	n, ok := checkedNeg64(int64(q))
	if !ok {
		return 0, fmt.Errorf("negating qty %d: %w", q, ErrOverflow)
	}
	return Qty(n), nil
}

// Abs returns |q|, or ErrOverflow for the MinInt64 value.
func (q Qty) Abs() (Qty, error) {
	n, ok := checkedAbs64(int64(q))
	if !ok {
		return 0, fmt.Errorf("abs of qty %d: %w", q, ErrOverflow)
	}
	return Qty(n), nil
}

// Cmp returns -1, 0 or +1 comparing q to o.
func (q Qty) Cmp(o Qty) int { return cmpInt64(int64(q), int64(o)) }

// Sign returns -1, 0 or +1.
func (q Qty) Sign() int { return cmpInt64(int64(q), 0) }

// IsZero reports q == 0 (flat).
func (q Qty) IsZero() bool { return q == 0 }

// MarshalJSON encodes a JSON number.
func (q Qty) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(q), 10)), nil
}

// UnmarshalJSON accepts a JSON integer number or a quoted integer string.
// JSON null leaves the value unchanged.
func (q *Qty) UnmarshalJSON(b []byte) error {
	tok := bytes.TrimSpace(b)
	if string(tok) == "null" {
		return nil
	}
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		tok = tok[1 : len(tok)-1]
	}
	v, err := ParseQty(string(tok))
	if err != nil {
		return err
	}
	*q = v
	return nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func marshalFixedJSON(v int64) ([]byte, error) {
	s := formatFixed4(v)
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	b = append(b, s...)
	b = append(b, '"')
	return b, nil
}

// unmarshalFixedJSON parses a JSON token (quoted string or raw number) into
// 1e-4 fixed point exactly. Returns changed=false for JSON null.
func unmarshalFixedJSON(b []byte, kind string) (int64, bool, error) {
	tok := bytes.TrimSpace(b)
	if string(tok) == "null" {
		return 0, false, nil
	}
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		var s string
		if err := unquoteJSONString(tok, &s); err != nil {
			return 0, false, fmt.Errorf("%w: %s token %s", ErrInvalidNumber, kind, tok)
		}
		n, err := parseFixed4(s, roundExact)
		if err != nil {
			return 0, false, fmt.Errorf("parsing %s: %w", kind, err)
		}
		return n, true, nil
	}
	// Raw number token: parse the literal text exactly (never via float64).
	n, err := parseFixed4(string(tok), roundExact)
	if err != nil {
		return 0, false, fmt.Errorf("parsing %s: %w", kind, err)
	}
	return n, true, nil
}

// unquoteJSONString unquotes a JSON string token. Numeric literals contain
// no escapes, so the fast path suffices; escaped content is rejected as it
// can never be a valid number.
func unquoteJSONString(tok []byte, out *string) error {
	inner := tok[1 : len(tok)-1]
	if bytes.IndexByte(inner, '\\') >= 0 {
		return fmt.Errorf("%w: escaped characters in numeric string", ErrInvalidNumber)
	}
	*out = string(inner)
	return nil
}
