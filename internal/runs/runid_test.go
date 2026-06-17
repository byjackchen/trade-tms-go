package runs

import (
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"
)

// runTSCheckRE is the relaxed CHECK regex from migration 000012 (the second
// arrow group is the optional collision-free suffix). Keeping it in the test
// guarantees every NewRunID output is INSERT-able into tms.runs.
var runTSCheckRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_([01]\d|2[0-3])-[0-5]\d-[0-5]\d(-\d{6}-\d{4})?$`)

// TestNewRunTSMatchesCheck pins the plain second-resolution form (golden runs /
// explicit idempotency keys) against the relaxed DB CHECK.
func TestNewRunTSMatchesCheck(t *testing.T) {
	ts := NewRunTS(time.Date(2026, 6, 14, 2, 31, 59, 0, time.UTC))
	if ts != "2026-06-14_02-31-59" {
		t.Fatalf("NewRunTS = %q", ts)
	}
	if !runTSCheckRE.MatchString(ts) {
		t.Fatalf("NewRunTS %q violates run_ts CHECK", ts)
	}
}

// TestNewRunIDMatchesCheck pins the collision-free form against the CHECK.
func TestNewRunIDMatchesCheck(t *testing.T) {
	id := NewRunID(time.Date(2026, 6, 14, 2, 31, 59, 123456000, time.UTC))
	if !runTSCheckRE.MatchString(id) {
		t.Fatalf("NewRunID %q violates run_ts CHECK", id)
	}
	// Base second-resolution prefix is preserved (sortable + human-readable).
	if got := id[:19]; got != "2026-06-14_02-31-59" {
		t.Fatalf("NewRunID base prefix = %q", got)
	}
}

// TestNewRunIDCollisionFreeSameInstant is the direct regression for the round-3
// data-loss bug: many run keys generated for the EXACT same wall-clock instant
// (and even the exact same microsecond) must all be DISTINCT, because run_ts is
// the UNIQUE natural key and a collision triggers a silent DELETE-then-INSERT
// overwrite. NewRunTS at second resolution would produce ONE value for all of
// these; NewRunID must produce N distinct ones.
func TestNewRunIDCollisionFreeSameInstant(t *testing.T) {
	// Pin the SAME instant (down to the nanosecond) for every call so only the
	// monotonic counter can differentiate them — the worst case.
	now := time.Date(2026, 6, 14, 2, 31, 59, 123456000, time.UTC)
	const n = 5000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewRunID(now)
		if _, dup := seen[id]; dup {
			t.Fatalf("NewRunID produced a duplicate key %q at iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct keys, got %d", n, len(seen))
	}
}

// TestNewRunIDConcurrentDistinct exercises the atomic counter from multiple
// goroutines (the real worker-concurrency scenario: 4 backtests claimed in the
// same second).
func TestNewRunIDConcurrentDistinct(t *testing.T) {
	now := time.Date(2026, 6, 14, 2, 31, 59, 0, time.UTC)
	const workers, per = 8, 1000
	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*per)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, per)
			for i := range local {
				local[i] = NewRunID(now)
			}
			mu.Lock()
			for _, id := range local {
				seen[id] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != workers*per {
		t.Fatalf("concurrent NewRunID collided: %d distinct of %d", len(seen), workers*per)
	}
}

// TestNewRunIDSortable proves keys generated later in time sort AFTER earlier
// ones lexically, so the run list's ORDER BY run_ts DESC keeps newest-first.
func TestNewRunIDSortable(t *testing.T) {
	t0 := time.Date(2026, 6, 14, 2, 31, 59, 100000000, time.UTC)
	t1 := time.Date(2026, 6, 14, 2, 32, 0, 0, time.UTC) // next second
	a := NewRunID(t0)
	b := NewRunID(t1)
	keys := []string{b, a}
	sort.Strings(keys)
	if keys[0] != a || keys[1] != b {
		t.Fatalf("lexical sort did not order by time: %v", keys)
	}
}

// TestTSToISORoundTripsSuffix proves the strategy-summary ts derivation parses
// BOTH run_ts forms. The collision-free form must recover the sub-second
// instant the microsecond field encodes.
func TestTSToISORoundTripsSuffix(t *testing.T) {
	// Plain second-resolution: no sub-second.
	if got := tsToISO("2026-06-14_02-31-59"); got != "2026-06-14T02:31:59+00:00" {
		t.Fatalf("tsToISO(second) = %q", got)
	}
	// Collision-free form encoding 123456 microseconds → .123456 fractional.
	if got := tsToISO("2026-06-14_02-31-59-123456-0007"); got != "2026-06-14T02:31:59.123456+00:00" {
		t.Fatalf("tsToISO(suffix) = %q", got)
	}
	// Zero microseconds with a non-zero counter still renders the base second.
	if got := tsToISO("2026-06-14_02-31-59-000000-0042"); got != "2026-06-14T02:31:59+00:00" {
		t.Fatalf("tsToISO(zero-micros) = %q", got)
	}
	// Garbage falls through unchanged (defensive, matches prior behaviour).
	if got := tsToISO("not-a-ts"); got != "not-a-ts" {
		t.Fatalf("tsToISO(garbage) = %q", got)
	}
}

// TestSplitRunID unit-checks the parser used by tsToISO.
func TestSplitRunID(t *testing.T) {
	base, micros, ok := splitRunID("2026-06-14_02-31-59-123456-0007")
	if !ok || base != "2026-06-14_02-31-59" || micros != 123456 {
		t.Fatalf("splitRunID suffix = %q,%d,%v", base, micros, ok)
	}
	base, micros, ok = splitRunID("2026-06-14_02-31-59")
	if ok || base != "2026-06-14_02-31-59" || micros != 0 {
		t.Fatalf("splitRunID plain = %q,%d,%v", base, micros, ok)
	}
	// Malformed suffix (wrong digit counts) is treated as a plain key.
	if _, _, ok := splitRunID("2026-06-14_02-31-59-12345-0007"); ok {
		t.Fatalf("splitRunID should reject 5-digit micros field")
	}
}
