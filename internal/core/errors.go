package core

// errors.go defines the sentinel errors of the core engine. They wrap nothing
// and are matched with errors.Is by callers and tests.

import "errors"

var (
	// ErrTimeReversal is returned when the clock or queue is asked to move to
	// a timestamp earlier than the current one. The simulated timeline is
	// strictly non-decreasing.
	ErrTimeReversal = errors.New("core: event timestamp precedes current clock")

	// ErrLoopClosed is returned by Schedule after the loop has stopped (run
	// returned). No further events may be enqueued.
	ErrLoopClosed = errors.New("core: event loop is closed")
)
