package domain

// pytime.go provides the timestamp conventions of the reference system
// (spec §1.1) and Python-compatible datetime formatting used by run dumps
// and intent serialization (spec §1, JSON row: datetimes serialize as
// Python str(datetime), e.g. "2024-01-02 00:00:00+00:00").

import (
	"fmt"
	"strings"
	"time"
)

// TimeFromUnixNanos converts integer nanoseconds since the Unix epoch to a
// UTC time.Time using EXACT integer arithmetic.
//
// [IMPROVE vs reference]: Python divides int-ns by float 1e9
// (datetime.fromtimestamp(ts/1e9, UTC)), losing sub-microsecond precision
// for ns values > 2^53. For daily, midnight-aligned bars the loss is zero,
// so behavior is indistinguishable; this exact version is byte-identical to
// Python for all timestamps that are whole microseconds.
func TimeFromUnixNanos(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}

// UnixNanos converts a time.Time to integer nanoseconds since the Unix
// epoch. Defined only for years 1678..2262 (time.Time's int64-ns window),
// like every ts_event in the reference system.
func UnixNanos(t time.Time) int64 { return t.UnixNano() }

// FormatPyDatetime renders t (normalized to UTC) exactly as Python
// str(datetime) does for tz-aware UTC values:
//
//	"2024-01-02 00:00:00+00:00"          (microsecond == 0)
//	"2024-01-02 00:00:00.000123+00:00"   (microsecond != 0)
//
// Space separator, "+00:00" offset [MUST-MATCH the json.dumps(default=str)
// encoding]. Sub-microsecond nanoseconds are truncated (Python datetime has
// microsecond resolution).
func FormatPyDatetime(t time.Time) string {
	return formatPy(t, " ")
}

// FormatPyISO renders t exactly as Python datetime.isoformat() for tz-aware
// UTC values: like FormatPyDatetime but with a "T" separator (used by the
// run dumper, spec §2.16).
func FormatPyISO(t time.Time) string {
	return formatPy(t, "T")
}

func formatPy(t time.Time, sep string) string {
	u := t.UTC()
	base := u.Format("2006-01-02") + sep + u.Format("15:04:05")
	if micro := u.Nanosecond() / 1000; micro != 0 {
		base += fmt.Sprintf(".%06d", micro)
	}
	return base + "+00:00"
}

// pyDatetimeLayouts covers the Python encodings the platform produces:
// str(datetime) (space separator), isoformat() ("T" separator), with or
// without fractional seconds (Go parses optional fractions automatically),
// "+00:00"/"Z" offsets, naive datetimes (assumed UTC, as the reference
// treats every naive value), and bare dates (date.isoformat()).
var pyDatetimeLayouts = []string{
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParsePyDatetime parses the datetime strings produced by the Python
// reference (str(datetime), isoformat(), date.isoformat()) and returns a
// UTC-normalized time.Time.
func ParsePyDatetime(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("%w: empty datetime string", ErrInvalidArgument)
	}
	for _, layout := range pyDatetimeLayouts {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: unrecognized datetime %q", ErrInvalidArgument, s)
}
