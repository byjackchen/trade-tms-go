package runs

import "testing"

func TestFormatPyFloat(t *testing.T) {
	// Golden pairs pinning this repo's float surface form.
	cases := []struct {
		in   float64
		want string
	}{
		{100000.0, "100000.0"},
		{5247.309999999998, "5247.309999999998"},
		{2.8e12, "2800000000000.0"},
		{1e16, "1e+16"},
		{1e-4, "0.0001"},
		{0.0001, "0.0001"},
		{123.0, "123.0"},
		{0.0, "0.0"},
		{1.5, "1.5"},
		{0.1, "0.1"},
		{100.25, "100.25"},
		{9999999999999998.0, "9999999999999998.0"},
		{1e-5, "1e-05"},
		{3.14159, "3.14159"},
		{-2000.0, "-2000.0"},
		{105247.31, "105247.31"},
		{1e21, "1e+21"},
		{1.23456789e-7, "1.23456789e-07"},
		{12345678901234.5, "12345678901234.5"},
	}
	for _, tc := range cases {
		if got := FormatPyFloat(tc.in); got != tc.want {
			t.Errorf("FormatPyFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatPyFloatNegZero(t *testing.T) {
	if got := FormatPyFloat(negZero()); got != "-0.0" {
		t.Errorf("negative zero = %q, want -0.0", got)
	}
}

func negZero() float64 {
	z := 0.0
	return -z
}

func TestMarshalObjectOrdered(t *testing.T) {
	o := NewObj().
		Set("version", Int(1)).
		Set("ts", Str("2026-05-13_16-10-49")).
		Set("starting_balance_usd", PyFloat(100000.0)).
		Set("total_pnl_usd", PyFloat(5247.309999999998)).
		Set("strategies", NewArr().Append(Str("SEPA-000")).Append(Str("Pairs-002")))
	got := string(Marshal(o))
	want := `{
  "version": 1,
  "ts": "2026-05-13_16-10-49",
  "starting_balance_usd": 100000.0,
  "total_pnl_usd": 5247.309999999998,
  "strategies": [
    "SEPA-000",
    "Pairs-002"
  ]
}`
	if got != want {
		t.Errorf("Marshal mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarshalEmptyContainers(t *testing.T) {
	if got := string(Marshal(NewObj())); got != "{}" {
		t.Errorf("empty obj = %q", got)
	}
	if got := string(Marshal(NewArr())); got != "[]" {
		t.Errorf("empty arr = %q", got)
	}
}

func TestEncodePyStringEscapes(t *testing.T) {
	o := NewObj().Set("k", Str("a\"b\\c\nd"))
	got := string(Marshal(o))
	want := "{\n  \"k\": \"a\\\"b\\\\c\\nd\"\n}"
	if got != want {
		t.Errorf("string escape:\n got: %q\nwant: %q", got, want)
	}
}
