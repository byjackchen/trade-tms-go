package calendar

import "time"

// NYSE holiday and early-close rules.
//
// The Python reference deliberately has no NYSE-calendar dependency
// (docs/spec/calendar-universe.md §1.1); this package implements the
// [IMPROVE] static holiday/early-close table from that section. The rule
// set below reproduces the canonical exchange_calendars XNYS calendar
// exactly over 2000–2030 (golden-tested against
// testdata/nyse_sessions_*.csv, generated from exchange_calendars 4.13.2).
//
// Regular session: 09:30–16:00 America/New_York.
// Early-close session: 09:30–13:00 America/New_York.

// Rule applicability years (NYSE history; sources: NYSE notices,
// exchange_calendars exchange_calendar_xnys.py).
const (
	// mlkFirstYear: Martin Luther King Jr. Day first observed by NYSE.
	mlkFirstYear = 1998
	// juneteenthFirstYear: Juneteenth National Independence Day first
	// observed by NYSE.
	juneteenthFirstYear = 2022
	// july3WednesdayEarlyCloseFirstYear: since 2013 the NYSE closes early
	// on Wednesday July 3 (before that, a Wednesday July 3 was a full
	// session and Friday July 5 closed early instead).
	july3WednesdayEarlyCloseFirstYear = 2013
	// july5FridayEarlyCloseLastYear: through 2012 the NYSE closed early on
	// a Friday July 5 (e.g. 2002-07-05); from 2013 on it is a full session
	// (2013, 2019, 2024 were full days).
	july5FridayEarlyCloseLastYear  = 2012
	july5FridayEarlyCloseFirstYear = 1996
)

// specialClosures are full-day closings outside the recurring holiday
// rules, 2000–2030.
var specialClosures = map[Date]string{
	NewDate(2001, time.September, 11): "September 11 attacks",
	NewDate(2001, time.September, 12): "September 11 attacks",
	NewDate(2001, time.September, 13): "September 11 attacks",
	NewDate(2001, time.September, 14): "September 11 attacks",
	NewDate(2004, time.June, 11):      "Day of mourning for President Ronald Reagan",
	NewDate(2007, time.January, 2):    "Day of mourning for President Gerald Ford",
	NewDate(2012, time.October, 29):   "Hurricane Sandy",
	NewDate(2012, time.October, 30):   "Hurricane Sandy",
	NewDate(2018, time.December, 5):   "Day of mourning for President George H.W. Bush",
	NewDate(2025, time.January, 9):    "Day of mourning for President Jimmy Carter",
}

// adHocEarlyCloses are one-off 13:00 ET closes outside the recurring
// early-close rules.
var adHocEarlyCloses = map[Date]string{
	// Friday after Christmas 2003; not a recurring rule in the modern era
	// (2008-12-26, 2014-12-26 and 2025-12-26 were full sessions).
	NewDate(2003, time.December, 26): "Day after Christmas 2003",
}

// easterSunday returns Western (Gregorian) Easter Sunday for year, using
// the Anonymous Gregorian ("Meeus/Jones/Butcher") algorithm.
func easterSunday(year int) Date {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return NewDate(year, time.Month(month), day)
}

// nthWeekday returns the n-th (1-based) given weekday of month/year.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) Date {
	first := NewDate(year, month, 1)
	offset := (int(wd) - int(first.Weekday()) + 7) % 7
	return first.AddDays(offset + (n-1)*7)
}

// lastWeekday returns the last given weekday of month/year.
func lastWeekday(year int, month time.Month, wd time.Weekday) Date {
	last := NewDate(year, month+1, 1).AddDays(-1)
	offset := (int(last.Weekday()) - int(wd) + 7) % 7
	return last.AddDays(-offset)
}

// observedSatFriSunMon applies the standard NYSE observance shift for a
// fixed-date holiday: Saturday → preceding Friday, Sunday → following
// Monday.
func observedSatFriSunMon(d Date) Date {
	switch d.Weekday() {
	case time.Saturday:
		return d.AddDays(-1)
	case time.Sunday:
		return d.AddDays(1)
	default:
		return d
	}
}

// nyseHolidaysForYear returns the recurring full-day holidays observed in
// the given calendar year, keyed by observed date. Special closures are
// handled separately (specialClosures).
func nyseHolidaysForYear(year int) map[Date]string {
	h := make(map[Date]string, 10)

	// New Year's Day. Sunday → observed Monday Jan 2. Saturday → NOT
	// observed (NYSE Rule 7.2: a Saturday holiday is observed the prior
	// Friday unless that Friday ends a monthly/yearly accounting period;
	// Dec 31 always does, so e.g. 2021-12-31 was a full trading day).
	newYear := NewDate(year, time.January, 1)
	switch newYear.Weekday() {
	case time.Sunday:
		h[newYear.AddDays(1)] = "New Year's Day (observed)"
	case time.Saturday:
		// not observed
	default:
		h[newYear] = "New Year's Day"
	}

	if year >= mlkFirstYear {
		h[nthWeekday(year, time.January, time.Monday, 3)] = "Martin Luther King Jr. Day"
	}
	h[nthWeekday(year, time.February, time.Monday, 3)] = "Washington's Birthday"
	h[easterSunday(year).AddDays(-2)] = "Good Friday"
	h[lastWeekday(year, time.May, time.Monday)] = "Memorial Day"
	if year >= juneteenthFirstYear {
		h[observedSatFriSunMon(NewDate(year, time.June, 19))] = "Juneteenth National Independence Day"
	}
	h[observedSatFriSunMon(NewDate(year, time.July, 4))] = "Independence Day"
	h[nthWeekday(year, time.September, time.Monday, 1)] = "Labor Day"
	h[nthWeekday(year, time.November, time.Thursday, 4)] = "Thanksgiving Day"
	h[observedSatFriSunMon(NewDate(year, time.December, 25))] = "Christmas Day"

	return h
}

// nyseEarlyClose reports whether d, already known to be a trading day,
// closes at 13:00 ET, and the reason.
func nyseEarlyClose(d Date) (string, bool) {
	if reason, ok := adHocEarlyCloses[d]; ok {
		return reason, true
	}

	// Day after Thanksgiving (the Friday following the 4th Thursday of
	// November) always closes early in the modern era.
	if d.Month == time.November || d.Month == time.December {
		if d == nthWeekday(d.Year, time.November, time.Thursday, 4).AddDays(1) {
			return "Day after Thanksgiving", true
		}
	}

	// Christmas Eve, when it falls Monday–Thursday. (A Friday Dec 24 is
	// the observed Christmas holiday; weekend Dec 24 is not a session.)
	if d.Month == time.December && d.Day == 24 {
		switch d.Weekday() {
		case time.Monday, time.Tuesday, time.Wednesday, time.Thursday:
			return "Christmas Eve", true
		}
	}

	// July 3: Monday/Tuesday/Thursday always; Wednesday only since 2013.
	// (A Friday July 3 is the observed Independence Day holiday.)
	if d.Month == time.July && d.Day == 3 {
		switch d.Weekday() {
		case time.Monday, time.Tuesday, time.Thursday:
			return "Day before Independence Day", true
		case time.Wednesday:
			if d.Year >= july3WednesdayEarlyCloseFirstYear {
				return "Day before Independence Day", true
			}
		}
	}

	// Friday July 5, 1996–2012 only (e.g. 2002-07-05).
	if d.Month == time.July && d.Day == 5 && d.Weekday() == time.Friday &&
		d.Year >= july5FridayEarlyCloseFirstYear && d.Year <= july5FridayEarlyCloseLastYear {
		return "Day after Independence Day", true
	}

	return "", false
}
