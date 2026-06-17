package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTimeFromUnixNanosExact(t *testing.T) {
	tests := []struct {
		ns   int64
		want time.Time
	}{
		{0, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		{1704153600000000000, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{1704153600000000001, time.Date(2024, 1, 2, 0, 0, 0, 1, time.UTC)},
		{-1000000000, time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC)},
		// > 2^53: a float division (ns/1e9) cannot represent this exactly;
		// the integer path is exact [IMPROVE, documented].
		{9007199254740993, time.Date(1970, 4, 15, 5, 59, 59, 254740993, time.UTC)},
	}
	for _, tt := range tests {
		got := TimeFromUnixNanos(tt.ns)
		if !got.Equal(tt.want) {
			t.Errorf("TimeFromUnixNanos(%d) = %v, want %v", tt.ns, got, tt.want)
		}
		if got.Location() != time.UTC {
			t.Errorf("TimeFromUnixNanos(%d) location = %v, want UTC", tt.ns, got.Location())
		}
		if UnixNanos(got) != tt.ns {
			t.Errorf("UnixNanos round trip of %d = %d", tt.ns, UnixNanos(got))
		}
	}
}

func TestFormatPyDatetime(t *testing.T) {
	// Datetime string encoding (json.dumps default=str form): space separator,
	// +00:00 offset, .%06d only when micro != 0.
	tests := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "2024-01-02 00:00:00+00:00"},
		{time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC), "2024-01-02 15:04:05+00:00"},
		{time.Date(2024, 1, 2, 15, 4, 5, 123000, time.UTC), "2024-01-02 15:04:05.000123+00:00"},
		{time.Date(2024, 1, 2, 15, 4, 5, 999999000, time.UTC), "2024-01-02 15:04:05.999999+00:00"},
		// sub-microsecond ns truncate (µs resolution)
		{time.Date(2024, 1, 2, 15, 4, 5, 999, time.UTC), "2024-01-02 15:04:05+00:00"},
		// non-UTC inputs are normalized to UTC first
		{time.Date(2024, 1, 2, 10, 0, 0, 0, time.FixedZone("EST", -5*3600)), "2024-01-02 15:00:00+00:00"},
	}
	for _, tt := range tests {
		if got := FormatPyDatetime(tt.in); got != tt.want {
			t.Errorf("FormatPyDatetime(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := FormatPyISO(time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)); got != "2024-01-02T15:04:05+00:00" {
		t.Errorf("FormatPyISO = %q", got)
	}
	if got := FormatPyISO(time.Date(2024, 1, 2, 15, 4, 5, 250000000, time.UTC)); got != "2024-01-02T15:04:05.250000+00:00" {
		t.Errorf("FormatPyISO with micros = %q", got)
	}
}

func TestParsePyDatetime(t *testing.T) {
	want := time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)
	wantMicro := time.Date(2024, 1, 2, 15, 4, 5, 123000, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"2024-01-02 15:04:05+00:00", want}, // str(datetime)
		{"2024-01-02T15:04:05+00:00", want}, // isoformat()
		{"2024-01-02 15:04:05.000123+00:00", wantMicro},
		{"2024-01-02T15:04:05.000123+00:00", wantMicro},
		{"2024-01-02 15:04:05", want},                               // naive → assumed UTC
		{"2024-01-02T15:04:05Z", want},                              // RFC3339 Z
		{"2024-01-02", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)}, // date.isoformat()
		{"2024-01-02 10:04:05-05:00", want},                         // offset normalized to UTC
		{" 2024-01-02 15:04:05+00:00 ", want},                       // surrounding whitespace tolerated
	}
	for _, tt := range tests {
		got, err := ParsePyDatetime(tt.in)
		if err != nil {
			t.Errorf("ParsePyDatetime(%q) error: %v", tt.in, err)
			continue
		}
		if !got.Equal(tt.want) {
			t.Errorf("ParsePyDatetime(%q) = %v, want %v", tt.in, got, tt.want)
		}
		if _, off := got.Zone(); off != 0 {
			t.Errorf("ParsePyDatetime(%q) not UTC-normalized", tt.in)
		}
	}
	for _, in := range []string{"", "  ", "not-a-date", "2024-13-40", "02/01/2024"} {
		if _, err := ParsePyDatetime(in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ParsePyDatetime(%q) error = %v, want ErrInvalidArgument", in, err)
		}
	}
}

func TestPyFormatParseRoundTrip(t *testing.T) {
	ts := time.Date(2025, 6, 12, 9, 30, 0, 250000000, time.UTC)
	for _, s := range []string{FormatPyDatetime(ts), FormatPyISO(ts)} {
		got, err := ParsePyDatetime(s)
		if err != nil {
			t.Fatalf("round trip parse of %q: %v", s, err)
		}
		if !got.Equal(ts) {
			t.Errorf("round trip of %q = %v, want %v", s, got, ts)
		}
	}
}
