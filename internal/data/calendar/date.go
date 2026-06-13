package calendar

import (
	"fmt"
	"time"
)

// Date is a timezone-free calendar date. The zero value is invalid; build
// values with NewDate, ParseDate or DateOf.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate returns the date for the given components. Components are
// normalized the same way time.Date normalizes them (e.g. February 30
// becomes March 1 or 2), so callers should pass real dates; ParseDate is
// the strict entry point for external input.
func NewDate(year int, month time.Month, day int) Date {
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// DateOf returns the calendar date of the instant t in location loc.
func DateOf(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{Year: y, Month: m, Day: d}
}

// ParseDate parses a date in ISO-8601 "YYYY-MM-DD" form. Unlike NewDate it
// rejects out-of-range components (e.g. "2024-02-30").
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("calendar: parse date %q: %w", s, err)
	}
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// String renders the date as "YYYY-MM-DD".
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// IsZero reports whether d is the zero value.
func (d Date) IsZero() bool {
	return d == Date{}
}

// midnight returns the instant of midnight on d in loc.
func (d Date) midnight(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// Weekday returns the day of week of d.
func (d Date) Weekday() time.Weekday {
	return d.midnight(time.UTC).Weekday()
}

// AddDays returns d shifted by n calendar days (n may be negative).
func (d Date) AddDays(n int) Date {
	t := d.midnight(time.UTC).AddDate(0, 0, n)
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// Compare returns -1 if d is before other, 0 if equal, +1 if after.
func (d Date) Compare(other Date) int {
	switch {
	case d.Year != other.Year:
		return cmpInt(d.Year, other.Year)
	case d.Month != other.Month:
		return cmpInt(int(d.Month), int(other.Month))
	default:
		return cmpInt(d.Day, other.Day)
	}
}

// Before reports whether d is strictly before other.
func (d Date) Before(other Date) bool { return d.Compare(other) < 0 }

// After reports whether d is strictly after other.
func (d Date) After(other Date) bool { return d.Compare(other) > 0 }

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
