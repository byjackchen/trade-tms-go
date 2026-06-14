package calendar

import (
	"errors"
	"fmt"
	"time"
	// Embed the IANA timezone database as a fallback so the calendar
	// works in minimal containers without a system tzdata package.
	_ "time/tzdata"
)

// Supported range of the NYSE calendar. Queries outside this range return
// ErrOutOfRange instead of silently wrong answers (recurring rules would
// extrapolate, but ad-hoc closures beyond 2030 are unknowable).
const (
	MinYear = 2000
	MaxYear = 2030
)

// Exchange-local session times (minutes after midnight, America/New_York).
const (
	openHour         = 9
	openMinute       = 30
	closeHour        = 16
	closeMinute      = 0
	earlyCloseHour   = 13
	earlyCloseMinute = 0
)

// Sentinel errors returned by Calendar queries.
var (
	// ErrOutOfRange marks a query outside [MinYear, MaxYear].
	ErrOutOfRange = errors.New("calendar: date outside supported range")
	// ErrNotTradingDay marks a session lookup on a weekend, holiday or
	// special closure.
	ErrNotTradingDay = errors.New("calendar: not a trading day")
	// ErrNoSession marks a next/prev query whose answer lies beyond the
	// supported range.
	ErrNoSession = errors.New("calendar: no session in supported range")
)

// Session is one NYSE trading session.
type Session struct {
	// Date is the session's exchange-local (America/New_York) calendar
	// date.
	Date Date
	// Open is the session open instant (09:30 ET) in UTC.
	Open time.Time
	// Close is the session close instant (16:00 ET, or 13:00 ET on early
	// closes) in UTC.
	Close time.Time
	// EarlyClose reports a 13:00 ET close.
	EarlyClose bool
}

// Calendar is an immutable, precomputed NYSE trading calendar covering
// [MinYear, MaxYear]. It is safe for concurrent use.
type Calendar struct {
	loc      *time.Location
	minDate  Date
	maxDate  Date
	sessions []Session
	// byDate maps a session date to its index in sessions.
	byDate map[Date]int
	// closures maps non-session weekdays to the holiday/closure name.
	closures map[Date]string
}

// NewNYSE builds the NYSE calendar for [MinYear, MaxYear].
func NewNYSE() (*Calendar, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("calendar: load America/New_York: %w", err)
	}

	c := &Calendar{
		loc:      loc,
		minDate:  NewDate(MinYear, time.January, 1),
		maxDate:  NewDate(MaxYear, time.December, 31),
		byDate:   make(map[Date]int, 8192),
		closures: make(map[Date]string, 512),
	}
	c.sessions = make([]Session, 0, 7900)

	for year := MinYear; year <= MaxYear; year++ {
		holidays := nyseHolidaysForYear(year)
		start := NewDate(year, time.January, 1)
		end := NewDate(year, time.December, 31)
		for d := start; !d.After(end); d = d.AddDays(1) {
			switch d.Weekday() {
			case time.Saturday, time.Sunday:
				continue
			}
			if name, ok := holidays[d]; ok {
				c.closures[d] = name
				continue
			}
			if name, ok := specialClosures[d]; ok {
				c.closures[d] = name
				continue
			}
			_, early := nyseEarlyClose(d)
			ch, cm := closeHour, closeMinute
			if early {
				ch, cm = earlyCloseHour, earlyCloseMinute
			}
			s := Session{
				Date:       d,
				Open:       time.Date(d.Year, d.Month, d.Day, openHour, openMinute, 0, 0, loc).UTC(),
				Close:      time.Date(d.Year, d.Month, d.Day, ch, cm, 0, 0, loc).UTC(),
				EarlyClose: early,
			}
			c.byDate[d] = len(c.sessions)
			c.sessions = append(c.sessions, s)
		}
	}
	return c, nil
}

// Location returns the exchange time zone (America/New_York).
func (c *Calendar) Location() *time.Location { return c.loc }

// MinDate returns the first date covered by the calendar.
func (c *Calendar) MinDate() Date { return c.minDate }

// MaxDate returns the last date covered by the calendar.
func (c *Calendar) MaxDate() Date { return c.maxDate }

func (c *Calendar) checkRange(d Date) error {
	if d.Before(c.minDate) || d.After(c.maxDate) {
		return fmt.Errorf("%w: %s not in [%s, %s]", ErrOutOfRange, d, c.minDate, c.maxDate)
	}
	return nil
}

// IsTradingDay reports whether d is an NYSE trading session date.
func (c *Calendar) IsTradingDay(d Date) (bool, error) {
	if err := c.checkRange(d); err != nil {
		return false, err
	}
	_, ok := c.byDate[d]
	return ok, nil
}

// IsEarlyClose reports whether d is a trading day closing at 13:00 ET.
// A non-trading day yields (false, nil).
func (c *Calendar) IsEarlyClose(d Date) (bool, error) {
	if err := c.checkRange(d); err != nil {
		return false, err
	}
	i, ok := c.byDate[d]
	if !ok {
		return false, nil
	}
	return c.sessions[i].EarlyClose, nil
}

// HolidayName returns the holiday or special-closure name for a weekday
// closure (e.g. "Good Friday", "Hurricane Sandy"). Weekends and trading
// days yield ("", false).
func (c *Calendar) HolidayName(d Date) (string, bool) {
	name, ok := c.closures[d]
	return name, ok
}

// SessionOn returns the session held on d. It returns ErrNotTradingDay
// (wrapped, with the closure reason when known) if the exchange is closed
// on d.
func (c *Calendar) SessionOn(d Date) (Session, error) {
	if err := c.checkRange(d); err != nil {
		return Session{}, err
	}
	i, ok := c.byDate[d]
	if !ok {
		if name, closed := c.closures[d]; closed {
			return Session{}, fmt.Errorf("%w: %s (%s)", ErrNotTradingDay, d, name)
		}
		return Session{}, fmt.Errorf("%w: %s (weekend)", ErrNotTradingDay, d)
	}
	return c.sessions[i], nil
}

// NextSession returns the first session strictly after d.
func (c *Calendar) NextSession(d Date) (Session, error) {
	if err := c.checkRange(d); err != nil {
		return Session{}, err
	}
	if i := c.searchFrom(d.AddDays(1)); i < len(c.sessions) {
		return c.sessions[i], nil
	}
	return Session{}, fmt.Errorf("%w: after %s", ErrNoSession, d)
}

// PrevSession returns the last session strictly before d.
func (c *Calendar) PrevSession(d Date) (Session, error) {
	if err := c.checkRange(d); err != nil {
		return Session{}, err
	}
	if i := c.searchFrom(d); i > 0 {
		return c.sessions[i-1], nil
	}
	return Session{}, fmt.Errorf("%w: before %s", ErrNoSession, d)
}

// SessionsInRange returns all sessions with start <= Date <= end, in
// ascending order. An empty (non-nil-error) slice is returned when
// start > end, mirroring the reference system's empty-range semantics.
// The returned slice is freshly allocated; callers may modify it.
func (c *Calendar) SessionsInRange(start, end Date) ([]Session, error) {
	if err := c.checkRange(start); err != nil {
		return nil, err
	}
	if err := c.checkRange(end); err != nil {
		return nil, err
	}
	if start.After(end) {
		return []Session{}, nil
	}
	lo := c.searchFrom(start)
	hi := c.searchFrom(end.AddDays(1))
	out := make([]Session, hi-lo)
	copy(out, c.sessions[lo:hi])
	return out, nil
}

// SessionAt returns the session whose exchange-local date contains the
// instant t, if that date is a trading day. The boolean result is false
// on non-trading dates.
func (c *Calendar) SessionAt(t time.Time) (Session, bool, error) {
	d := DateOf(t, c.loc)
	if err := c.checkRange(d); err != nil {
		return Session{}, false, err
	}
	i, ok := c.byDate[d]
	if !ok {
		return Session{}, false, nil
	}
	return c.sessions[i], true, nil
}

// searchFrom returns the index of the first session with Date >= d.
func (c *Calendar) searchFrom(d Date) int {
	lo, hi := 0, len(c.sessions)
	for lo < hi {
		mid := (lo + hi) / 2
		if c.sessions[mid].Date.Before(d) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
