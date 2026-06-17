package hyperopt

// walkforward.go (spec §3). Strategies are rule-based — there is NO train
// period — so the splitter emits only evaluation windows. The arithmetic
// (calendar-day, integer floor) feeds run_backtest fold boundaries and
// therefore every downstream metric, so it is pinned exactly, including the
// "vestigial embargo" quirk (the constant embargo offset shifts all segments
// later by embargo_days once, it is NOT a per-fold gap; consecutive segments
// are adjacent).

import (
	"fmt"
	"time"
)

// EvalSegment is one inclusive evaluation window.
// TestStart and TestEnd are calendar dates (UTC midnight, day-aligned).
type EvalSegment struct {
	TestStart time.Time
	TestEnd   time.Time
}

// dayDiff returns the number of whole calendar days from a to b. Both inputs
// are normalized to UTC midnight so DST/zone offsets never perturb the count.
func dayDiff(a, b time.Time) int {
	au := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bu := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(bu.Sub(au).Hours() / 24)
}

// addDays returns t + n calendar days at UTC midnight.
func addDays(t time.Time, n int) time.Time {
	u := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return u.AddDate(0, 0, n)
}

// floorDivInt returns floor division a / b for b > 0 and a >= 0
// (all call sites here use non-negative a).
func floorDivInt(a, b int) int { return a / b }

// ExpandingAnchored carves [start, end] into nFolds non-overlapping inclusive
// evaluation segments (spec §3.1).
//
// Validation order and messages (all errors):
//
//	end <= start                                   -> "end must be after start"
//	n_folds < 1                                    -> "n_folds must be >= 1"
//	embargo_days < 0                               -> "embargo_days must be >= 0"
//	remaining_days < n_folds || segment_days < 1   -> "date range too short for requested folds"
//
// Algorithm (integer floor arithmetic):
//
//	total_days     = (end - start).days + 1
//	buffer_days    = max(total_days // 3, 1)
//	remaining_days = total_days - buffer_days - embargo_days
//	segment_days   = remaining_days // n_folds
//	for idx in 0..n_folds-1:
//	    prev_end   = start + (buffer_days + idx*segment_days - 1) days
//	    test_start = prev_end + (embargo_days + 1) days
//	    test_end   = test_start + (segment_days - 1) days
//	    if idx == n_folds-1: test_end = end   # last fold absorbs the remainder
func ExpandingAnchored(start, end time.Time, nFolds, embargoDays int) ([]EvalSegment, error) {
	// Reference compares date objects: end <= start raises. dayDiff is the
	// signed calendar-day distance, so end after start iff dayDiff > 0.
	if dayDiff(start, end) <= 0 {
		return nil, fmt.Errorf("end must be after start")
	}
	if nFolds < 1 {
		return nil, fmt.Errorf("n_folds must be >= 1")
	}
	if embargoDays < 0 {
		return nil, fmt.Errorf("embargo_days must be >= 0")
	}

	totalDays := dayDiff(start, end) + 1
	bufferDays := floorDivInt(totalDays, 3)
	if bufferDays < 1 {
		bufferDays = 1
	}
	remainingDays := totalDays - bufferDays - embargoDays
	if remainingDays < nFolds {
		return nil, fmt.Errorf("date range too short for requested folds")
	}
	segmentDays := floorDivInt(remainingDays, nFolds)
	if segmentDays < 1 {
		return nil, fmt.Errorf("date range too short for requested folds")
	}

	segments := make([]EvalSegment, 0, nFolds)
	endDay := startDay(end)
	for idx := 0; idx < nFolds; idx++ {
		prevEnd := addDays(start, bufferDays+idx*segmentDays-1)
		testStart := addDays(prevEnd, embargoDays+1)
		testEnd := addDays(testStart, segmentDays-1)
		if idx == nFolds-1 {
			testEnd = endDay
		}
		segments = append(segments, EvalSegment{TestStart: testStart, TestEnd: testEnd})
	}
	return segments, nil
}

func startDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
