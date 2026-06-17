package orb

// pydec_test.go locks the exact decimal arithmetic and rendering that the ORB
// reason / state strings depend on. Expected values are the pinned golden
// decimal outputs.

import "testing"

func TestPydecScalePropagation(t *testing.T) {
	cases := []struct {
		name string
		got  func() pydec
		want string
	}{
		{"mul scale sum 102.0*0.99", func() pydec {
			return mustDec("102.0").mul(decFromInt(1).sub(mustDec("1.0").divInt(100)))
		}, "100.980"},
		{"sub keeps larger scale 102.0-100.980", func() pydec {
			return mustDec("102.0").sub(mustDec("100.980"))
		}, "1.020"},
		{"target 102.0+1.020*2.0", func() pydec {
			sd := mustDec("102.0").sub(mustDec("100.980"))
			return mustDec("102.0").add(sd.mul(mustDec("2.0")))
		}, "104.0400"},
		{"div exact 1.0/100", func() pydec { return mustDec("1.0").divInt(100) }, "0.01"},
		{"div exact 5.0/100", func() pydec { return mustDec("5.0").divInt(100) }, "0.05"},
		{"hard5 102.0*0.95", func() pydec {
			return mustDec("102.0").mul(decFromInt(1).sub(mustDec("5.0").divInt(100)))
		}, "96.900"},
		{"range_low wins -> 99.0 stays", func() pydec { return mustDec("99.0") }, "99.0"},
		{"session3 stop 51.8*0.99", func() pydec {
			return mustDec("51.8").mul(decFromInt(1).sub(mustDec("1.0").divInt(100)))
		}, "51.282"},
		{"session2 stop 202.0*0.99", func() pydec {
			return mustDec("202.0").mul(decFromInt(1).sub(mustDec("1.0").divInt(100)))
		}, "199.980"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got().String(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPydecCompare(t *testing.T) {
	// Decimal("100.980") == Decimal("100.98") by value.
	if mustDec("100.980").cmp(mustDec("100.98")) != 0 {
		t.Fatal("100.980 != 100.98 by value")
	}
	if mustDec("102.0").cmp(mustDec("101.0")) <= 0 {
		t.Fatal("102.0 not > 101.0")
	}
	if mustDec("-0.2").cmp(mustDec("0")) >= 0 {
		t.Fatal("-0.2 not < 0")
	}
}

func TestPydecFloat64(t *testing.T) {
	if f := mustDec("100.980").float64(); f != 100.98 {
		t.Fatalf("float64(100.980) = %v", f)
	}
	if f := mustDec("1.020").float64(); f != 1.02 {
		t.Fatalf("float64(1.020) = %v", f)
	}
}

func TestPyFloatRepr(t *testing.T) {
	cases := map[float64]string{
		1.5: "1.5", 2.0: "2.0", 1.0: "1.0", 3.0: "3.0", 0.5: "0.5",
		100000.0: "100000.0", 0.0001: "0.0001",
	}
	for in, want := range cases {
		if got := pyFloatRepr(in); got != want {
			t.Errorf("pyFloatRepr(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPyFmt0(t *testing.T) {
	cases := map[float64]string{
		1000000.0: "1000000", 100000.0: "100000", 0.0: "0",
		// round-half-even at .5: 2.5 -> "2", 3.5 -> "4"
		2.5: "2", 3.5: "4",
	}
	for in, want := range cases {
		if got := pyFmt0(in); got != want {
			t.Errorf("pyFmt0(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDivIntProximityPaths(t *testing.T) {
	// BUY proximity: (500.50-500)/500*100 == 0.1
	last := mustDec("500.50")
	oh := mustDec("500")
	prox := last.sub(oh).div(oh).mul(decFromInt(100)).float64()
	if prox != 0.1 {
		t.Fatalf("buy proximity = %v, want 0.1", prox)
	}
	// FORMING strength: (499-498)/(500-498)*100 == 50.0
	last = mustDec("499")
	ol := mustDec("498")
	width := oh.sub(ol)
	str := last.sub(ol).div(width).mul(decFromInt(100)).float64()
	if str != 50.0 {
		t.Fatalf("forming strength = %v, want 50.0", str)
	}
	prox = last.sub(oh).div(oh).mul(decFromInt(100)).float64()
	if prox != -0.2 {
		t.Fatalf("forming proximity = %v, want -0.2", prox)
	}
}
