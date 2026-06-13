package sector_rotation

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// priceDeque is a bounded FIFO of Price mirroring collections.deque(maxlen=n):
// pushing onto a full deque drops the oldest element. Oldest is index 0
// (front), newest is index -1 (back).
type priceDeque struct {
	buf    []domain.Price
	maxlen int
}

func newPriceDeque(maxlen int) *priceDeque {
	return &priceDeque{buf: make([]domain.Price, 0, maxlen), maxlen: maxlen}
}

// push appends p, evicting the oldest element if the deque is at capacity
// (deque.append with maxlen semantics).
func (d *priceDeque) push(p domain.Price) {
	if d.maxlen == 0 {
		return
	}
	if len(d.buf) == d.maxlen {
		copy(d.buf, d.buf[1:])
		d.buf[len(d.buf)-1] = p
		return
	}
	d.buf = append(d.buf, p)
}

func (d *priceDeque) len() int { return len(d.buf) }

// front is deque[0] (oldest). Caller must ensure len() > 0.
func (d *priceDeque) front() domain.Price { return d.buf[0] }

// back is deque[-1] (newest). Caller must ensure len() > 0.
func (d *priceDeque) back() domain.Price { return d.buf[len(d.buf)-1] }

// snapshot returns the deque contents oldest-first (for state_dict).
func (d *priceDeque) snapshot() []domain.Price {
	out := make([]domain.Price, len(d.buf))
	copy(out, d.buf)
	return out
}

// ratioReturn reproduces float((new-old)/old) where new/old are exact Decimals
// in the reference. With Price held as 1e-4 fixed-point int64, the exact
// Decimal subtraction is the exact int64 difference of raw units, and the
// Decimal division-then-float() equals float64(rawDiff)/float64(rawOld). This
// avoids the intermediate float subtraction rounding that would otherwise
// diverge from CPython in the last ULP (verified across the parity fixtures).
func ratioReturn(old, new domain.Price) float64 {
	return float64(int64(new)-int64(old)) / float64(int64(old))
}

// formatSignedPct2 reproduces Python's f"{x:+.2f}": always-signed, 2 decimals,
// round-half-to-even, signed zero preserved. Go's %+.2f matches CPython here
// bit-for-bit (verified).
func formatSignedPct2(x float64) string {
	return fmt.Sprintf("%+.2f", x)
}

// dateOf returns the calendar date (UTC) of ts as a midnight-UTC time, matching
// Python's bar.ts.date(): the reference compares only year/month for rollover
// and stores the .isoformat() date string in state. We keep a time.Time
// truncated to the day so callers can read Month()/Year() and format YYYY-MM-DD.
func dateOf(ts time.Time) time.Time {
	u := ts.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
