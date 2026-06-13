package calendar

import (
	"embed"
	"encoding/csv"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Golden files generated from exchange_calendars 4.13.2 (XNYS) via
// tmp/gen_nyse_ref.py. Columns: session,open_utc,close_utc,early_close.
//
//go:embed testdata/nyse_sessions_ref.csv testdata/nyse_sessions_2000_2030.csv
var goldenFS embed.FS

type refSession struct {
	date       string
	openUTC    string
	closeUTC   string
	earlyClose bool
}

func loadGolden(t *testing.T, name string) []refSession {
	t.Helper()
	f, err := goldenFS.Open(name)
	require.NoError(t, err)
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	require.NoError(t, err)
	require.Equal(t, []string{"session", "open_utc", "close_utc", "early_close"}, header)

	var out []refSession
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Len(t, rec, 4)
		out = append(out, refSession{
			date:       rec[0],
			openUTC:    rec[1],
			closeUTC:   rec[2],
			earlyClose: rec[3] == "1",
		})
	}
	return out
}

func newNYSE(t *testing.T) *Calendar {
	t.Helper()
	c, err := NewNYSE()
	require.NoError(t, err)
	return c
}

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	require.NoError(t, err)
	return d
}

// assertGolden is the accuracy gate: the Go calendar must reproduce the
// exchange_calendars XNYS session list, open/close UTC instants and
// early-close flags exactly.
func assertGolden(t *testing.T, c *Calendar, golden string, start, end string) {
	t.Helper()
	ref := loadGolden(t, golden)
	require.NotEmpty(t, ref)

	got, err := c.SessionsInRange(mustDate(t, start), mustDate(t, end))
	require.NoError(t, err)
	require.Equal(t, len(ref), len(got), "session count mismatch for %s", golden)

	for i, want := range ref {
		s := got[i]
		require.Equal(t, want.date, s.Date.String(), "row %d: session date", i)
		require.Equal(t, want.openUTC, s.Open.UTC().Format(time.RFC3339), "row %d (%s): open", i, want.date)
		require.Equal(t, want.closeUTC, s.Close.UTC().Format(time.RFC3339), "row %d (%s): close", i, want.date)
		require.Equal(t, want.earlyClose, s.EarlyClose, "row %d (%s): early-close flag", i, want.date)
		require.Equal(t, time.UTC, s.Open.Location(), "row %d (%s): open must be UTC", i, want.date)
		require.Equal(t, time.UTC, s.Close.Location(), "row %d (%s): close must be UTC", i, want.date)
	}
}

func TestGoldenSessions2010to2026(t *testing.T) {
	c := newNYSE(t)
	assertGolden(t, c, "testdata/nyse_sessions_ref.csv", "2010-01-01", "2026-12-31")
}

func TestGoldenSessionsFullRange2000to2030(t *testing.T) {
	c := newNYSE(t)
	assertGolden(t, c, "testdata/nyse_sessions_2000_2030.csv", "2000-01-01", "2030-12-31")
}

func TestIsTradingDay(t *testing.T) {
	c := newNYSE(t)
	cases := []struct {
		date string
		want bool
		why  string
	}{
		{"2024-06-12", true, "ordinary Wednesday"},
		{"2024-06-15", false, "Saturday"},
		{"2024-06-16", false, "Sunday"},
		{"2024-03-29", false, "Good Friday 2024"},
		{"2024-06-19", false, "Juneteenth 2024"},
		{"2021-06-18", true, "Juneteenth not yet observed by NYSE in 2021"},
		{"2027-06-18", false, "Juneteenth 2027 (Sat) observed Friday"},
		{"2022-06-20", false, "Juneteenth 2022 (Sun) observed Monday"},
		{"2021-12-31", true, "New Year's 2022 fell on Saturday: not observed"},
		{"2022-01-03", true, "first session of 2022"},
		{"2023-01-02", false, "New Year's 2023 (Sun) observed Monday"},
		{"2012-10-29", false, "Hurricane Sandy"},
		{"2012-10-30", false, "Hurricane Sandy"},
		{"2012-10-31", true, "reopen after Sandy"},
		{"2001-09-10", true, "last session before the attacks"},
		{"2001-09-11", false, "September 11 attacks"},
		{"2001-09-14", false, "September 11 attacks"},
		{"2001-09-17", true, "reopen after the attacks"},
		{"2004-06-11", false, "Reagan mourning"},
		{"2007-01-02", false, "Ford mourning"},
		{"2018-12-05", false, "G.H.W. Bush mourning"},
		{"2025-01-09", false, "Carter mourning"},
		{"2009-07-03", false, "Independence Day 2009 (Sat) observed Friday"},
		{"2021-07-05", false, "Independence Day 2021 (Sun) observed Monday"},
		{"2030-12-25", false, "Christmas 2030"},
		{"2000-04-21", false, "Good Friday 2000"},
	}
	for _, tc := range cases {
		got, err := c.IsTradingDay(mustDate(t, tc.date))
		require.NoError(t, err, tc.date)
		require.Equal(t, tc.want, got, "%s: %s", tc.date, tc.why)
	}
}

func TestEarlyCloseRules(t *testing.T) {
	c := newNYSE(t)
	cases := []struct {
		date string
		want bool
		why  string
	}{
		{"2024-11-29", true, "day after Thanksgiving"},
		{"2024-12-24", true, "Christmas Eve Tuesday"},
		{"2021-12-23", false, "Dec 23 is a full day even when Dec 24 is the observed holiday"},
		{"2013-07-03", true, "Wednesday July 3 closes early from 2013"},
		{"2002-07-03", false, "Wednesday July 3 was a full session before 2013"},
		{"2002-07-05", true, "Friday July 5 closed early through 2012"},
		{"2013-07-05", false, "Friday July 5 full session from 2013"},
		{"2019-07-05", false, "Friday July 5 2019 full session"},
		{"2024-07-05", false, "Friday July 5 2024 full session"},
		{"2003-12-26", true, "ad-hoc early close, day after Christmas 2003"},
		{"2008-12-26", false, "Friday Dec 26 2008 full session"},
		{"2025-12-26", false, "Friday Dec 26 2025 full session"},
		{"2028-07-03", true, "Monday July 3 2028 early close"},
		{"2024-12-25", false, "holiday, not an early close"},
		{"2024-12-21", false, "Saturday, not an early close"},
	}
	for _, tc := range cases {
		got, err := c.IsEarlyClose(mustDate(t, tc.date))
		require.NoError(t, err, tc.date)
		require.Equal(t, tc.want, got, "%s: %s", tc.date, tc.why)
	}
}

func TestSessionTimesUTCWithDST(t *testing.T) {
	c := newNYSE(t)

	// EST (UTC-5): 09:30 ET = 14:30 UTC, 16:00 ET = 21:00 UTC.
	winter, err := c.SessionOn(mustDate(t, "2024-01-05"))
	require.NoError(t, err)
	require.Equal(t, "2024-01-05T14:30:00Z", winter.Open.Format(time.RFC3339))
	require.Equal(t, "2024-01-05T21:00:00Z", winter.Close.Format(time.RFC3339))
	require.False(t, winter.EarlyClose)

	// EDT (UTC-4): 09:30 ET = 13:30 UTC, 16:00 ET = 20:00 UTC.
	summer, err := c.SessionOn(mustDate(t, "2024-07-05"))
	require.NoError(t, err)
	require.Equal(t, "2024-07-05T13:30:00Z", summer.Open.Format(time.RFC3339))
	require.Equal(t, "2024-07-05T20:00:00Z", summer.Close.Format(time.RFC3339))

	// Early close: 13:00 ET = 18:00 UTC in November (EST).
	early, err := c.SessionOn(mustDate(t, "2024-11-29"))
	require.NoError(t, err)
	require.Equal(t, "2024-11-29T18:00:00Z", early.Close.Format(time.RFC3339))
	require.True(t, early.EarlyClose)
}

func TestNextPrevSession(t *testing.T) {
	c := newNYSE(t)

	next, err := c.NextSession(mustDate(t, "2012-10-26")) // Friday before Sandy
	require.NoError(t, err)
	require.Equal(t, "2012-10-31", next.Date.String())

	prev, err := c.PrevSession(mustDate(t, "2025-01-10")) // Friday after Carter mourning
	require.NoError(t, err)
	require.Equal(t, "2025-01-08", prev.Date.String())

	// Across a normal weekend.
	next, err = c.NextSession(mustDate(t, "2024-06-14"))
	require.NoError(t, err)
	require.Equal(t, "2024-06-17", next.Date.String())

	// From a non-trading day.
	next, err = c.NextSession(mustDate(t, "2001-09-11"))
	require.NoError(t, err)
	require.Equal(t, "2001-09-17", next.Date.String())

	// Year-end into a Saturday New Year (2022): 2021-12-31 trades.
	prev, err = c.PrevSession(mustDate(t, "2022-01-03"))
	require.NoError(t, err)
	require.Equal(t, "2021-12-31", prev.Date.String())

	// Beyond the table going forward.
	_, err = c.NextSession(mustDate(t, "2030-12-31"))
	require.ErrorIs(t, err, ErrNoSession)
}

func TestPrevSessionAtRangeStart(t *testing.T) {
	c := newNYSE(t)
	// 2000-01-03 (Mon) is the first session of the table; there is no
	// earlier session in range.
	_, err := c.PrevSession(mustDate(t, "2000-01-03"))
	require.ErrorIs(t, err, ErrNoSession)
}

func TestSessionOnErrors(t *testing.T) {
	c := newNYSE(t)

	_, err := c.SessionOn(mustDate(t, "2024-03-29"))
	require.ErrorIs(t, err, ErrNotTradingDay)
	require.ErrorContains(t, err, "Good Friday")

	_, err = c.SessionOn(mustDate(t, "2024-06-15"))
	require.ErrorIs(t, err, ErrNotTradingDay)
	require.ErrorContains(t, err, "weekend")

	_, err = c.SessionOn(mustDate(t, "1999-12-31"))
	require.ErrorIs(t, err, ErrOutOfRange)
	_, err = c.SessionOn(mustDate(t, "2031-01-01"))
	require.ErrorIs(t, err, ErrOutOfRange)
}

func TestHolidayName(t *testing.T) {
	c := newNYSE(t)
	cases := map[string]string{
		"2024-03-29": "Good Friday",
		"2012-10-29": "Hurricane Sandy",
		"2025-01-09": "Day of mourning for President Jimmy Carter",
		"2022-06-20": "Juneteenth National Independence Day",
		"2023-01-02": "New Year's Day (observed)",
	}
	for date, want := range cases {
		name, ok := c.HolidayName(mustDate(t, date))
		require.True(t, ok, date)
		require.Equal(t, want, name, date)
	}
	_, ok := c.HolidayName(mustDate(t, "2024-06-12"))
	require.False(t, ok, "trading day has no holiday name")
	_, ok = c.HolidayName(mustDate(t, "2024-06-15"))
	require.False(t, ok, "weekend has no holiday name")
}

func TestSessionsInRangeSemantics(t *testing.T) {
	c := newNYSE(t)

	// Inclusive endpoints.
	got, err := c.SessionsInRange(mustDate(t, "2024-06-10"), mustDate(t, "2024-06-14"))
	require.NoError(t, err)
	require.Len(t, got, 5)
	require.Equal(t, "2024-06-10", got[0].Date.String())
	require.Equal(t, "2024-06-14", got[4].Date.String())

	// Weekend/holiday endpoints are simply excluded.
	got, err = c.SessionsInRange(mustDate(t, "2026-05-23"), mustDate(t, "2026-05-26"))
	require.NoError(t, err)
	require.Len(t, got, 1) // Sat 23, Sun 24, Memorial Day Mon 25 -> only Tue 26
	require.Equal(t, "2026-05-26", got[0].Date.String())

	// end < start -> empty, no error.
	got, err = c.SessionsInRange(mustDate(t, "2024-06-14"), mustDate(t, "2024-06-10"))
	require.NoError(t, err)
	require.Empty(t, got)

	// Out-of-range bounds error.
	_, err = c.SessionsInRange(mustDate(t, "1999-01-01"), mustDate(t, "2024-06-10"))
	require.ErrorIs(t, err, ErrOutOfRange)
	_, err = c.SessionsInRange(mustDate(t, "2024-06-10"), mustDate(t, "2031-01-01"))
	require.ErrorIs(t, err, ErrOutOfRange)
}

func TestSessionAt(t *testing.T) {
	c := newNYSE(t)

	// 2024-07-03 23:00 UTC is 19:00 ET on July 3 (early-close day).
	s, ok, err := c.SessionAt(time.Date(2024, time.July, 3, 23, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "2024-07-03", s.Date.String())
	require.True(t, s.EarlyClose)

	// 2024-07-04 01:00 UTC is still 21:00 ET on July 3.
	s, ok, err = c.SessionAt(time.Date(2024, time.July, 4, 1, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "2024-07-03", s.Date.String())

	// Midday on July 4 (holiday): no session.
	_, ok, err = c.SessionAt(time.Date(2024, time.July, 4, 16, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestDateHelpers(t *testing.T) {
	d := mustDate(t, "2024-02-29")
	require.Equal(t, "2024-02-29", d.String())
	require.Equal(t, time.Thursday, d.Weekday())
	require.Equal(t, "2024-03-01", d.AddDays(1).String())
	require.Equal(t, "2024-02-28", d.AddDays(-1).String())
	require.True(t, d.Before(mustDate(t, "2024-03-01")))
	require.True(t, d.After(mustDate(t, "2024-02-28")))
	require.Equal(t, 0, d.Compare(NewDate(2024, time.February, 29)))
	require.False(t, d.IsZero())
	require.True(t, Date{}.IsZero())

	_, err := ParseDate("2024-02-30")
	require.Error(t, err)
	_, err = ParseDate("not-a-date")
	require.Error(t, err)

	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	// 2024-07-04T01:00Z is 2024-07-03 in New York.
	require.Equal(t, "2024-07-03",
		DateOf(time.Date(2024, time.July, 4, 1, 0, 0, 0, time.UTC), loc).String())
}

func TestEasterSunday(t *testing.T) {
	// Known Western Easter dates (independent source: USNO).
	cases := map[int]string{
		2000: "2000-04-23",
		2008: "2008-03-23",
		2016: "2016-03-27",
		2024: "2024-03-31",
		2025: "2025-04-20",
		2030: "2030-04-21",
	}
	for year, want := range cases {
		require.Equal(t, want, easterSunday(year).String(), "easter %d", year)
	}
}

func TestCalendarAccessors(t *testing.T) {
	c := newNYSE(t)
	require.Equal(t, "America/New_York", c.Location().String())
	require.Equal(t, "2000-01-01", c.MinDate().String())
	require.Equal(t, "2030-12-31", c.MaxDate().String())
}
