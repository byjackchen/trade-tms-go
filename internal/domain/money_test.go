package domain

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"testing"
)

func TestPriceParseAndString(t *testing.T) {
	tests := []struct {
		in   string
		want Price
		str  string
	}{
		{"123.45", 1234500, "123.45"},
		{"123.40", 1234000, "123.4"},
		{"-0.0001", -1, "-0.0001"},
		{"0", 0, "0"},
		{"70.5", 705000, "70.5"},
		{"922337203685477.5807", math.MaxInt64, "922337203685477.5807"},
		{"-922337203685477.5808", math.MinInt64, "-922337203685477.5808"},
	}
	for _, tt := range tests {
		got, err := ParsePrice(tt.in)
		if err != nil {
			t.Errorf("ParsePrice(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParsePrice(%q) = %d, want %d", tt.in, got, tt.want)
		}
		if got.String() != tt.str {
			t.Errorf("Price(%d).String() = %q, want %q", got, got.String(), tt.str)
		}
	}
	if _, err := ParsePrice("1.23456"); !errors.Is(err, ErrInexact) {
		t.Error("ParsePrice must be exact")
	}
	if _, err := ParseMoney("not-a-number"); !errors.Is(err, ErrInvalidNumber) {
		t.Error("ParseMoney must reject junk")
	}
	if MustPrice("123.45") != 1234500 {
		t.Error("MustPrice failed")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("MustPrice on invalid input must panic")
			}
		}()
		MustPrice("junk")
	}()
}

func TestPriceArithmetic(t *testing.T) {
	a, b := MustPrice("10.50"), MustPrice("0.25")

	if v, err := a.Add(b); err != nil || v != MustPrice("10.75") {
		t.Errorf("Add = %v, %v", v, err)
	}
	if v, err := a.Sub(b); err != nil || v != MustPrice("10.25") {
		t.Errorf("Sub = %v, %v", v, err)
	}
	if v, err := a.Neg(); err != nil || v != MustPrice("-10.50") {
		t.Errorf("Neg = %v, %v", v, err)
	}
	if v, err := MustPrice("-3.5").Abs(); err != nil || v != MustPrice("3.5") {
		t.Errorf("Abs = %v, %v", v, err)
	}
	if v, err := a.MulQty(100); err != nil || v != MustMoney("1050") {
		t.Errorf("MulQty = %v, %v", v, err)
	}
	if v, err := a.MulQty(-100); err != nil || v != MustMoney("-1050") {
		t.Errorf("MulQty negative = %v, %v", v, err)
	}
	if a.AsMoney() != Money(a) {
		t.Error("AsMoney must be a lossless reinterpretation")
	}
	if a.Cmp(b) != 1 || b.Cmp(a) != -1 || a.Cmp(a) != 0 {
		t.Error("Cmp ordering wrong")
	}
	if a.Sign() != 1 || MustPrice("-1").Sign() != -1 || Price(0).Sign() != 0 {
		t.Error("Sign wrong")
	}
	if !Price(0).IsZero() || !a.IsPositive() || !MustPrice("-1").IsNegative() {
		t.Error("predicates wrong")
	}

	// Overflow paths.
	if _, err := Price(math.MaxInt64).Add(1); !errors.Is(err, ErrOverflow) {
		t.Error("Add overflow not detected")
	}
	if _, err := Price(math.MinInt64).Sub(1); !errors.Is(err, ErrOverflow) {
		t.Error("Sub overflow not detected")
	}
	if _, err := Price(math.MinInt64).Neg(); !errors.Is(err, ErrOverflow) {
		t.Error("Neg overflow not detected")
	}
	if _, err := Price(math.MinInt64).Abs(); !errors.Is(err, ErrOverflow) {
		t.Error("Abs overflow not detected")
	}
	if _, err := Price(math.MaxInt64).MulQty(2); !errors.Is(err, ErrOverflow) {
		t.Error("MulQty overflow not detected")
	}
	if _, err := PriceFromInt(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Error("PriceFromInt overflow not detected")
	}
	if v, err := PriceFromInt(123); err != nil || v != MustPrice("123") {
		t.Errorf("PriceFromInt(123) = %v, %v", v, err)
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a, b := MustMoney("100000"), MustMoney("0.0001")

	if v, err := a.Add(b); err != nil || v != MustMoney("100000.0001") {
		t.Errorf("Add = %v, %v", v, err)
	}
	if v, err := a.Sub(b); err != nil || v != MustMoney("99999.9999") {
		t.Errorf("Sub = %v, %v", v, err)
	}
	if v, err := a.MulInt64(3); err != nil || v != MustMoney("300000") {
		t.Errorf("MulInt64 = %v, %v", v, err)
	}
	if v, err := MustMoney("-12.5").Abs(); err != nil || v != MustMoney("12.5") {
		t.Errorf("Abs = %v, %v", v, err)
	}
	if v, err := MustMoney("12.5").Neg(); err != nil || v != MustMoney("-12.5") {
		t.Errorf("Neg = %v, %v", v, err)
	}
	if _, err := Money(math.MaxInt64).MulInt64(2); !errors.Is(err, ErrOverflow) {
		t.Error("MulInt64 overflow not detected")
	}
	if v, err := MoneyFromInt(100000); err != nil || v != MustMoney("100000") {
		t.Errorf("MoneyFromInt = %v, %v", v, err)
	}
	if a.StringFixed(2) != "100000.00" {
		t.Errorf("StringFixed = %q", a.StringFixed(2))
	}
	if got := MustMoney("123.4").StringFixed(2); got != "123.40" {
		t.Errorf("StringFixed(2) of 123.4 = %q, want fixed-2dp %q", got, "123.40")
	}
}

func TestQty(t *testing.T) {
	if v, err := ParseQty("-150"); err != nil || v != -150 {
		t.Errorf("ParseQty = %v, %v", v, err)
	}
	if _, err := ParseQty("1.5"); !errors.Is(err, ErrInvalidNumber) {
		t.Error("ParseQty must reject fractions")
	}
	if _, err := ParseQty("1e2"); !errors.Is(err, ErrInvalidNumber) {
		t.Error("ParseQty must reject exponents")
	}
	if v, err := Qty(5).Add(3); err != nil || v != 8 {
		t.Errorf("Add = %v, %v", v, err)
	}
	if v, err := Qty(5).Sub(8); err != nil || v != -3 {
		t.Errorf("Sub = %v, %v", v, err)
	}
	if v, err := Qty(-7).Abs(); err != nil || v != 7 {
		t.Errorf("Abs = %v, %v", v, err)
	}
	if v, err := Qty(7).Neg(); err != nil || v != -7 {
		t.Errorf("Neg = %v, %v", v, err)
	}
	if _, err := Qty(math.MaxInt64).Add(1); !errors.Is(err, ErrOverflow) {
		t.Error("Qty.Add overflow not detected")
	}
	if _, err := Qty(math.MinInt64).Abs(); !errors.Is(err, ErrOverflow) {
		t.Error("Qty.Abs overflow not detected")
	}
	if Qty(3).Cmp(5) != -1 || Qty(3).Sign() != 1 || !Qty(0).IsZero() {
		t.Error("Qty predicates wrong")
	}
	if Qty(-150).String() != "-150" || Qty(-150).Int64() != -150 || Qty(2).Float64() != 2.0 {
		t.Error("Qty conversions wrong")
	}
}

func TestQtyFromFloat64Trunc(t *testing.T) {
	// Truncation toward zero — including for negatives.
	tests := []struct {
		in   float64
		want Qty
	}{
		{0, 0}, {1.0, 1}, {1.9, 1}, {-1.9, -1}, {-0.5, 0}, {0.999999, 0},
		{100.0, 100}, {-100.7, -100}, {2.5, 2}, {-2.5, -2},
		{9.007199254740992e15, 9007199254740992},
		{-9223372036854775808.0, math.MinInt64}, // MinInt64 is exactly representable
	}
	for _, tt := range tests {
		got, err := QtyFromFloat64Trunc(tt.in)
		if err != nil {
			t.Errorf("QtyFromFloat64Trunc(%v) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("QtyFromFloat64Trunc(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := QtyFromFloat64Trunc(x); !errors.Is(err, ErrNotFinite) {
			t.Errorf("QtyFromFloat64Trunc(%v) error = %v, want ErrNotFinite", x, err)
		}
	}
	for _, x := range []float64{9.3e18, -9.3e18, 1e300} {
		if _, err := QtyFromFloat64Trunc(x); !errors.Is(err, ErrOverflow) {
			t.Errorf("QtyFromFloat64Trunc(%v) error = %v, want ErrOverflow", x, err)
		}
	}
}

func TestFixedJSONRoundTrip(t *testing.T) {
	// Marshal → Unmarshal must reproduce the value bit-for-bit, for the full
	// int64 range.
	values := []int64{0, 1, -1, 1234500, -1234000, math.MaxInt64, math.MinInt64}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 5000; i++ {
		values = append(values, rng.Int63()-rng.Int63())
	}
	for _, v := range values {
		p := Price(v)
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal Price(%d): %v", v, err)
		}
		var back Price
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("unmarshal Price %s: %v", b, err)
		}
		if back != p {
			t.Fatalf("Price JSON round trip %d → %s → %d", v, b, back)
		}

		m := Money(v)
		b, err = json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal Money(%d): %v", v, err)
		}
		var backM Money
		if err := json.Unmarshal(b, &backM); err != nil {
			t.Fatalf("unmarshal Money %s: %v", b, err)
		}
		if backM != m {
			t.Fatalf("Money JSON round trip %d → %s → %d", v, b, backM)
		}

		q := Qty(v)
		b, err = json.Marshal(q)
		if err != nil {
			t.Fatalf("marshal Qty(%d): %v", v, err)
		}
		var backQ Qty
		if err := json.Unmarshal(b, &backQ); err != nil {
			t.Fatalf("unmarshal Qty %s: %v", b, err)
		}
		if backQ != q {
			t.Fatalf("Qty JSON round trip %d → %s → %d", v, b, backQ)
		}
	}
}

func TestFixedJSONForms(t *testing.T) {
	// String form (the canonical encoding, default=str).
	if b, _ := json.Marshal(MustPrice("123.45")); string(b) != `"123.45"` {
		t.Errorf("Price marshals as %s, want \"123.45\"", b)
	}
	// Qty marshals as a number.
	if b, _ := json.Marshal(Qty(-150)); string(b) != `-150` {
		t.Errorf("Qty marshals as %s, want -150", b)
	}

	// Unmarshal accepts quoted strings, raw number tokens (parsed exactly,
	// never through float64) and integer-valued exponents.
	var p Price
	for in, want := range map[string]Price{
		`"123.45"`: 1234500,
		`123.45`:   1234500,
		`-0.0001`:  -1,
		`1.2345e2`: 1234500,
		`"70.5"`:   705000,
		`0`:        0,
		`"+2"`:     20000,
	} {
		p = 0
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Errorf("unmarshal %s: %v", in, err)
			continue
		}
		if p != want {
			t.Errorf("unmarshal %s = %d, want %d", in, p, want)
		}
	}

	// null leaves the value unchanged (encoding/json convention).
	p = MustPrice("9.99")
	if err := json.Unmarshal([]byte(`null`), &p); err != nil || p != MustPrice("9.99") {
		t.Errorf("null handling: %v, %v", p, err)
	}
	var q Qty = 7
	if err := json.Unmarshal([]byte(`null`), &q); err != nil || q != 7 {
		t.Errorf("Qty null handling: %v, %v", q, err)
	}

	// Inexact / invalid tokens are rejected — silent precision loss is not.
	for _, in := range []string{`"1.23456"`, `1.23456`, `"abc"`, `true`, `{"x":1}`, `"1e15"`} {
		var pp Price
		if err := json.Unmarshal([]byte(in), &pp); err == nil {
			t.Errorf("unmarshal %s should fail", in)
		}
	}
	var qq Qty
	if err := json.Unmarshal([]byte(`1.5`), &qq); err == nil {
		t.Error("Qty must reject fractional numbers")
	}
	if err := json.Unmarshal([]byte(`"200"`), &qq); err != nil || qq != 200 {
		t.Errorf("Qty quoted integer: %v, %v", qq, err)
	}
}

func TestFixedTextMarshaling(t *testing.T) {
	// TextMarshaler enables use as JSON map keys.
	m := map[Price]string{MustPrice("1.5"): "x"}
	b, err := json.Marshal(m)
	if err != nil || string(b) != `{"1.5":"x"}` {
		t.Errorf("map key marshal = %s, %v", b, err)
	}
	var back map[Price]string
	if err := json.Unmarshal(b, &back); err != nil || back[MustPrice("1.5")] != "x" {
		t.Errorf("map key unmarshal = %v, %v", back, err)
	}

	var p Price
	if err := p.UnmarshalText([]byte("70.5")); err != nil || p != MustPrice("70.5") {
		t.Errorf("UnmarshalText: %v, %v", p, err)
	}
	if err := p.UnmarshalText([]byte("junk")); err == nil {
		t.Error("UnmarshalText must reject junk")
	}
	var mm Money
	if err := mm.UnmarshalText([]byte("-12.5")); err != nil || mm != MustMoney("-12.5") {
		t.Errorf("Money UnmarshalText: %v, %v", mm, err)
	}
	if txt, _ := MustMoney("-12.5").MarshalText(); string(txt) != "-12.5" {
		t.Errorf("Money MarshalText = %s", txt)
	}
}
