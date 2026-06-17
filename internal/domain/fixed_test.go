package domain

import (
	"bufio"
	"errors"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseFixed4Exact(t *testing.T) {
	valid := []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"0.0", 0}, {"-0", 0}, {"-0.0000", 0}, {"+0", 0},
		{"1", 10000}, {"-1", -10000}, {"+1", 10000},
		{"123.45", 1234500}, {"123.40", 1234000},
		{"0.0001", 1}, {"-0.0001", -1},
		{"1.23450000", 12345},
		{"000123.45", 1234500}, {"0.0500", 500},
		{".5", 5000}, {"5.", 50000},
		{"1e2", 1000000}, {"1.5e+3", 15000000}, {"1e-4", 1},
		{"1.2345e2", 1234500}, {"12345e-4", 12345}, {"1.2345E2", 1234500},
		{"922337203685477.5807", math.MaxInt64},
		{"-922337203685477.5808", math.MinInt64},
		{"-900000000000000", -9000000000000000000},
		{"70.5", 705000},
	}
	for _, tt := range valid {
		got, err := parseFixed4(tt.in, roundExact)
		if err != nil {
			t.Errorf("parseFixed4(%q, exact) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseFixed4(%q, exact) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseFixed4Errors(t *testing.T) {
	tests := []struct {
		in      string
		wantErr error
	}{
		{"", ErrInvalidNumber},
		{"abc", ErrInvalidNumber},
		{".", ErrInvalidNumber},
		{"-", ErrInvalidNumber},
		{"+", ErrInvalidNumber},
		{"--1", ErrInvalidNumber},
		{"1..2", ErrInvalidNumber},
		{"1.2.3", ErrInvalidNumber},
		{"1e", ErrInvalidNumber},
		{"1e+", ErrInvalidNumber},
		{"1e1.5", ErrInvalidNumber},
		{"1 ", ErrInvalidNumber},
		{" 1", ErrInvalidNumber},
		{"1_000", ErrInvalidNumber},
		{"NaN", ErrInvalidNumber},
		{"Inf", ErrInvalidNumber},
		{"0x10", ErrInvalidNumber},
		// inexact at 1e-4 scale (exact mode)
		{"0.00001", ErrInexact},
		{"5e-5", ErrInexact},
		{"1.23456", ErrInexact},
		{"0.99999", ErrInexact},
		{"1e-1000000000", ErrInexact},
		// out of range
		{"922337203685477.5808", ErrOverflow},
		{"-922337203685477.5809", ErrOverflow},
		{"1e15", ErrOverflow},
		{"-1e15", ErrOverflow},
		{"99999999999999999999999999", ErrOverflow},
		{"1e1000000000", ErrOverflow},
	}
	for _, tt := range tests {
		_, err := parseFixed4(tt.in, roundExact)
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("parseFixed4(%q, exact) error = %v, want errors.Is %v", tt.in, err, tt.wantErr)
		}
	}
}

func TestParseFixed4HalfEven(t *testing.T) {
	// Half-to-even (ROUND_HALF_EVEN), NOT half-up: ties go to the even
	// neighbor. The half-up answers are noted where they differ.
	tests := []struct {
		in   string
		want int64
	}{
		{"0.00005", 0},     // tie → even 0 (half-up would give 1)
		{"0.00015", 2},     // tie → even 2
		{"0.00025", 2},     // tie → even 2 (half-up would give 3)
		{"0.00035", 4},     // tie → even 4
		{"-0.00005", 0},    // symmetric
		{"-0.00015", -2},   // symmetric
		{"-0.00025", -2},   // symmetric (half-up away-from-zero would give -3)
		{"0.000050001", 1}, // above the tie → up
		{"0.000049999", 0}, // below the tie → down
		{"0.00009", 1},     // round up from zero kept digits
		{"1.23456", 12346}, // 6 > 5 → up
		{"1.23454", 12345}, // 4 < 5 → down
		{"1.23455", 12346}, // tie, kept digit 5 odd → up
		{"1.23445", 12344}, // tie, kept digit 4 even → down
		{"70.49999999999999", 705000},
		{"1e-7", 0},
		{"-1e-7", 0},
		{"1e-1000000000", 0},
		{"123.45", 1234500}, // exact stays exact
	}
	for _, tt := range tests {
		got, err := parseFixed4(tt.in, roundHalfEven)
		if err != nil {
			t.Errorf("parseFixed4(%q, halfEven) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseFixed4(%q, halfEven) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestFormatFixed4Canonical(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "0.0001"},
		{-1, "-0.0001"},
		{10000, "1"},
		{-10000, "-1"},
		{1234500, "123.45"},
		{1234000, "123.4"}, // canonical trims trailing zeros (use StringFixed(2) for fixed-decimal display)
		{705000, "70.5"},
		{12345, "1.2345"},
		{math.MaxInt64, "922337203685477.5807"},
		{math.MinInt64, "-922337203685477.5808"},
		{5000, "0.5"},
		{-5000, "-0.5"},
	}
	for _, tt := range tests {
		if got := formatFixed4(tt.in); got != tt.want {
			t.Errorf("formatFixed4(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	// parseFixed4(formatFixed4(v)) == v for every int64, including extremes.
	fixed := []int64{0, 1, -1, 9999, -9999, 10000, -10000, math.MaxInt64, math.MinInt64,
		math.MaxInt64 - 1, math.MinInt64 + 1, 1234500, -1234500}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 10000; i++ {
		fixed = append(fixed, rng.Int63()-rng.Int63())
	}
	for _, v := range fixed {
		s := formatFixed4(v)
		got, err := parseFixed4(s, roundExact)
		if err != nil {
			t.Fatalf("round trip of %d via %q: %v", v, s, err)
		}
		if got != v {
			t.Fatalf("round trip of %d via %q = %d", v, s, got)
		}
	}
}

func TestFormatFixedDP(t *testing.T) {
	tests := []struct {
		in   int64
		dp   int
		want string
	}{
		{1234500, 2, "123.45"},
		{1234000, 2, "123.40"}, // fixed 2dp decimal for prices
		{1234500, 4, "123.4500"},
		{1234500, 6, "123.450000"},
		{1234500, 0, "123"},
		{1235000, 1, "123.5"},
		{1234550, 2, "123.46"}, // 123.455 → tie, kept 5 odd → up
		{1234450, 2, "123.44"}, // 123.445 → tie, kept 4 even → down
		{-1234550, 2, "-123.46"},
		{-1234450, 2, "-123.44"},
		{5000, 0, "0"},  // 0.5 → tie → even 0
		{15000, 0, "2"}, // 1.5 → tie → even 2
		{25000, 0, "2"}, // 2.5 → tie → even 2
		{-5000, 0, "0"}, // -0.5 → tie → even 0 (sign dropped with the digits)
		{-15000, 0, "-2"},
		{0, 2, "0.00"},
		{1234500, -3, "123"}, // negative dp treated as 0
	}
	for _, tt := range tests {
		if got := formatFixedDP(tt.in, tt.dp); got != tt.want {
			t.Errorf("formatFixedDP(%d, %d) = %q, want %q", tt.in, tt.dp, got, tt.want)
		}
	}
}

func TestRoundHalfEvenDiv(t *testing.T) {
	tests := []struct {
		v, d, want int64
	}{
		{5, 10, 0}, {15, 10, 2}, {25, 10, 2}, {35, 10, 4},
		{-5, 10, 0}, {-15, 10, -2}, {-25, 10, -2}, {-35, 10, -4},
		{6, 10, 1}, {-6, 10, -1}, {4, 10, 0}, {-4, 10, 0},
		{10, 10, 1}, {-10, 10, -1}, {0, 10, 0},
		{math.MinInt64, 10000, -922337203685478}, // -922337203685477.5808 → tie? .5808 > .5 → away
		{math.MaxInt64, 10000, 922337203685478},  // .5807 > .5 → away
	}
	for _, tt := range tests {
		if got := roundHalfEvenDiv(tt.v, tt.d); got != tt.want {
			t.Errorf("roundHalfEvenDiv(%d, %d) = %d, want %d", tt.v, tt.d, got, tt.want)
		}
	}
}

func TestCheckedArithmetic(t *testing.T) {
	type op struct {
		a, b   int64
		want   int64
		ok     bool
		fn     func(int64, int64) (int64, bool)
		fnName string
	}
	add, sub, mul := checkedAdd64, checkedSub64, checkedMul64
	tests := []op{
		{1, 2, 3, true, add, "add"},
		{math.MaxInt64, 1, 0, false, add, "add"},
		{math.MaxInt64, 0, math.MaxInt64, true, add, "add"},
		{math.MinInt64, -1, 0, false, add, "add"},
		{math.MinInt64, math.MaxInt64, -1, true, add, "add"},
		{-5, 5, 0, true, add, "add"},

		{1, 2, -1, true, sub, "sub"},
		{math.MinInt64, 1, 0, false, sub, "sub"},
		{math.MaxInt64, -1, 0, false, sub, "sub"},
		{math.MaxInt64, math.MaxInt64, 0, true, sub, "sub"},
		{0, math.MinInt64, 0, false, sub, "sub"}, // -(MinInt64) overflows

		{3, 4, 12, true, mul, "mul"},
		{-3, 4, -12, true, mul, "mul"},
		{0, math.MinInt64, 0, true, mul, "mul"},
		{math.MinInt64, 0, 0, true, mul, "mul"},
		{math.MinInt64, -1, 0, false, mul, "mul"}, // would panic with naive div check
		{-1, math.MinInt64, 0, false, mul, "mul"},
		{math.MinInt64, 1, math.MinInt64, true, mul, "mul"},
		{math.MaxInt64, 2, 0, false, mul, "mul"},
		{3037000500, 3037000500, 0, false, mul, "mul"}, // sqrt(MaxInt64)+1 squared
		{3037000499, 3037000499, 9223372030926249001, true, mul, "mul"},
	}
	for _, tt := range tests {
		got, ok := tt.fn(tt.a, tt.b)
		if ok != tt.ok {
			t.Errorf("checked %s(%d, %d) ok = %v, want %v", tt.fnName, tt.a, tt.b, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("checked %s(%d, %d) = %d, want %d", tt.fnName, tt.a, tt.b, got, tt.want)
		}
	}

	if _, ok := checkedNeg64(math.MinInt64); ok {
		t.Error("checkedNeg64(MinInt64) must overflow")
	}
	if v, ok := checkedNeg64(5); !ok || v != -5 {
		t.Errorf("checkedNeg64(5) = %d, %v", v, ok)
	}
	if _, ok := checkedAbs64(math.MinInt64); ok {
		t.Error("checkedAbs64(MinInt64) must overflow")
	}
	if v, ok := checkedAbs64(-5); !ok || v != 5 {
		t.Errorf("checkedAbs64(-5) = %d, %v", v, ok)
	}
}

func TestPyRoundHandcrafted(t *testing.T) {
	// PyRound(float, ndigits): correctly-rounded half-to-even on the
	// EXACT binary value. round(2.675, 2) == 2.67 because the double 2.675
	// is 2.67499999...; a naive decimal half-even on "2.675" would give 2.68
	// and naive half-up (math.Round style) would also give 2.68.
	tests := []struct {
		x    float64
		n    int
		want float64
	}{
		{2.675, 2, 2.67},
		{0.5, 0, 0.0},
		{1.5, 0, 2.0},
		{2.5, 0, 2.0},
		{3.5, 0, 4.0},
		{-2.5, 0, -2.0},
		{0.125, 2, 0.12}, // exact binary tie → even
		{0.375, 2, 0.38}, // exact binary tie → even (8 is even)
		{1.0, 4, 1.0},
		{0.0, 4, 0.0},
		{1e300, 4, 1e300},
		{70.49999999999999, 4, 70.5},
	}
	for _, tt := range tests {
		got, err := PyRound(tt.x, tt.n)
		if err != nil {
			t.Errorf("PyRound(%v, %d) error: %v", tt.x, tt.n, err)
			continue
		}
		if math.Float64bits(got) != math.Float64bits(tt.want) {
			t.Errorf("PyRound(%v, %d) = %v, want %v", tt.x, tt.n, got, tt.want)
		}
	}

	if v, err := PyRound(math.NaN(), 4); err != nil || !math.IsNaN(v) {
		t.Errorf("PyRound(NaN, 4) = %v, %v; want NaN passthrough", v, err)
	}
	if v, err := PyRound(math.Inf(1), 4); err != nil || !math.IsInf(v, 1) {
		t.Errorf("PyRound(+Inf, 4) = %v, %v; want +Inf passthrough", v, err)
	}
	if _, err := PyRound(1.5, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("PyRound(1.5, -1) error = %v, want ErrInvalidArgument", err)
	}
	if got := PyRound4(2.67455); got != PyRound4(2.67455) || math.Float64bits(PyRound4(0.5)) != math.Float64bits(0.5) {
		t.Errorf("PyRound4 basic sanity failed: %v", got)
	}
}

// TestGoldenRounding validates PyRound (ndigits 0, 2, 4) and the
// string-decimal half-even quantization path of *FromFloat64 against the
// pinned golden vectors (testdata/pyround_golden.txt, regenerated by
// testdata/gen_golden.py).
func TestGoldenRounding(t *testing.T) {
	f, err := os.Open("testdata/pyround_golden.txt")
	if err != nil {
		t.Fatalf("opening golden file: %v", err)
	}
	defer f.Close()

	parseHex := func(s string, line int) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("line %d: bad hex float %q: %v", line, s, err)
		}
		return v
	}

	sc := bufio.NewScanner(f)
	lineNo, checked := 0, 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("line %d: want 5 fields, got %d", lineNo, len(fields))
		}
		x := parseHex(fields[0], lineNo)
		for i, n := range []int{0, 2, 4} {
			want := parseHex(fields[1+i], lineNo)
			got, err := PyRound(x, n)
			if err != nil {
				t.Fatalf("line %d: PyRound(%v, %d) error: %v", lineNo, x, n, err)
			}
			if math.Float64bits(got) != math.Float64bits(want) {
				t.Errorf("line %d: PyRound(%v, %d) = %v (%x), golden round = %v (%x)",
					lineNo, x, n, got, math.Float64bits(got), want, math.Float64bits(want))
			}
		}
		// quant4 column: Decimal(str(x)).quantize(1e-4, ROUND_HALF_EVEN)
		if fields[4] == "OOR" {
			if _, err := PriceFromFloat64(x); !errors.Is(err, ErrOverflow) {
				t.Errorf("line %d: PriceFromFloat64(%v) error = %v, want ErrOverflow", lineNo, x, err)
			}
		} else {
			want, err := parseFixed4(fields[4], roundExact)
			if err != nil {
				t.Fatalf("line %d: bad quant4 %q: %v", lineNo, fields[4], err)
			}
			gotP, err := PriceFromFloat64(x)
			if err != nil {
				t.Fatalf("line %d: PriceFromFloat64(%v) error: %v", lineNo, x, err)
			}
			if int64(gotP) != want {
				t.Errorf("line %d: PriceFromFloat64(%v) = %d, golden quantize = %d",
					lineNo, x, int64(gotP), want)
			}
			gotM, err := MoneyFromFloat64(x)
			if err != nil {
				t.Fatalf("line %d: MoneyFromFloat64(%v) error: %v", lineNo, x, err)
			}
			if int64(gotM) != want {
				t.Errorf("line %d: MoneyFromFloat64(%v) = %d, want %d", lineNo, x, int64(gotM), want)
			}
		}
		checked++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if checked < 5000 {
		t.Fatalf("golden corpus suspiciously small: %d vectors", checked)
	}
	t.Logf("validated %d golden vectors", checked)
}

func TestFromFloatErrors(t *testing.T) {
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := PriceFromFloat64(x); !errors.Is(err, ErrNotFinite) {
			t.Errorf("PriceFromFloat64(%v) error = %v, want ErrNotFinite", x, err)
		}
		if _, err := MoneyFromFloat64(x); !errors.Is(err, ErrNotFinite) {
			t.Errorf("MoneyFromFloat64(%v) error = %v, want ErrNotFinite", x, err)
		}
	}
	for _, x := range []float64{1e18, -1e18, 1e300} {
		if _, err := PriceFromFloat64(x); !errors.Is(err, ErrOverflow) {
			t.Errorf("PriceFromFloat64(%v) error = %v, want ErrOverflow", x, err)
		}
	}
}

func TestFixed4ToFloatSingleRounding(t *testing.T) {
	// Beyond 2^53 raw units a naive float64(v)/1e4 double-rounds; the string
	// path must stay correctly rounded (exact decimal -> float64).
	cases := []int64{0, 1, -1, 1234500, math.MaxInt64, math.MinInt64, 1 << 53, (1 << 53) + 1}
	for _, v := range cases {
		got := fixed4ToFloat(v)
		want, err := strconv.ParseFloat(formatFixed4(v), 64)
		if err != nil {
			t.Fatalf("parse of own format failed for %d: %v", v, err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Errorf("fixed4ToFloat(%d) = %v, want %v", v, got, want)
		}
	}
	if Price(1234500).Float64() != 123.45 {
		t.Errorf("Price(1234500).Float64() = %v", Price(1234500).Float64())
	}
}
