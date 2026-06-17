package domain

// pytime.go provides the timestamp conventions of the system (spec §1.1) and
// the datetime formatting used by run dumps and intent serialization (spec §1,
// JSON row: datetimes serialize as e.g. "2024-01-02 00:00:00+00:00").

import (
	"fmt"
	"strings"
	"time"
)

// TimeFromUnixNanos converts integer nanoseconds since the Unix epoch to a
// UTC time.Time using EXACT integer arithmetic, preserving full nanosecond
// precision (a float division by 1e9 would lose precision for ns values >
// 2^53). For daily, midnight-aligned bars there is no difference either way.
func TimeFromUnixNanos(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}

// UnixNanos converts a time.Time to integer nanoseconds since the Unix
// epoch. Defined only for years 1678..2262 (time.Time's int64-ns window),
// which covers every ts_event in the system.
func UnixNanos(t time.Time) int64 { return t.UnixNano() }

// FormatPyDatetime renders t (normalized to UTC) in the canonical datetime
// wire format for tz-aware UTC values:
//
//	"2024-01-02 00:00:00+00:00"          (microsecond == 0)
//	"2024-01-02 00:00:00.000123+00:00"   (microsecond != 0)
//
// Space separator, "+00:00" offset. Sub-microsecond nanoseconds are truncated
// (the wire format has microsecond resolution).
func FormatPyDatetime(t time.Time) string {
	return formatPy(t, " ")
}

// FormatPyISO renders t in ISO-8601 form for tz-aware UTC values: like
// FormatPyDatetime but with a "T" separator (used by the run dumper,
// spec §2.16).
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

// pyDatetimeLayouts covers the datetime encodings the platform produces:
// space-separated and "T"-separated forms, with or without fractional seconds
// (Go parses optional fractions automatically), "+00:00"/"Z" offsets, naive
// datetimes (assumed UTC), and bare dates.
var pyDatetimeLayouts = []string{
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParsePyDatetime parses the datetime strings the platform produces
// (space-separated, "T"-separated, and bare-date forms) and returns a
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
