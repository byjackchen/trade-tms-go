// Package calendar provides the NYSE trading calendar: full-day holiday
// rules (including Good Friday and observed shifts), special closures,
// 13:00-ET early closes, and session open/close instants in
// America/New_York with exact UTC conversion.
//
// The Python reference system intentionally carries no NYSE-calendar
// dependency and approximates trading days as Mon–Fri (see
// docs/spec/calendar-universe.md §1.1–1.2). This package is the spec's
// [IMPROVE] static holiday/early-close table; consumers that must stay
// byte-compatible with the reference (e.g. the Sharadar catch-up
// "weekdays only" semantics) must NOT route their day enumeration through
// this calendar unless explicitly opted in.
//
// Accuracy gate: the session table is golden-tested against
// exchange_calendars XNYS output for 2010–2026 and for the full supported
// range 2000–2030 (see testdata/ and calendar_test.go).
//
// Layer: leaf utility (just above domain). Pure date/time computation with no
// I/O beyond the embedded tzdata fallback.
//
// May import: the standard library only (notably time and the embedded
// time/tzdata). It imports no other internal/ package.
package calendar
