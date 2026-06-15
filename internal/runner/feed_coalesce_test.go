package runner

// feed_coalesce_test.go is the white-box unit test for the intraday
// coalesce + close-detect path of MoomooFeed.PushHandler (the flood fix). It is
// in package `runner` (not runner_test) so it can drain the unexported pushCh
// directly without standing up a livengine.Session — the fix lives entirely in
// the push handler, upstream of the BarEvent stream.
//
// It SIMULATES the market-open KLType_1Min flood: ~16 forming-bar pushes/sec per
// symbol across 12 symbols over several simulated minutes (many same-barTS
// updates per minute, then a barTS rollover), and asserts the flood collapses to
// exactly ONE closed bar per (symbol, minute) — the final OHLCV — in TS order,
// with zero drops and bounded (==#symbols) memory.

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// price builds a Price from an integer "dollars" value (exact, no parse) for
// terse test fixtures.
func price(dollars int) domain.Price {
	return domain.MustPrice(strconv.Itoa(dollars) + ".00")
}

// TestPushHandlerCoalescesIntradayFlood drives the canonical market-open flood
// and asserts the four properties from the diagnosis: (a) exactly one CLOSED bar
// per (symbol, minute) with the LAST OHLCV, in TS order; (b) zero drops even with
// a tiny buffer; (c) bounded memory (pending map == #symbols); (d) coalescing
// keeps the latest value within a minute.
func TestPushHandlerCoalescesIntradayFlood(t *testing.T) {
	const (
		nSymbols     = 12
		nMinutes     = 5  // simulated minutes of streaming
		pushesPerMin = 16 // ~16 forming re-pushes/sec/symbol per minute
		// Deliberately tiny relative to the flood (nSymbols*(nMinutes+1)*
		// pushesPerMin = 1152 forming pushes): a naive forward-every-push would
		// saturate this in milliseconds and drop ~99% of pushes. The fix collapses
		// the flood to one emit per (symbol, minute), so even this small buffer
		// never fills. Sized to comfortably absorb one rollover burst (all
		// nSymbols close at once) without depending on drainer scheduling.
		smallBuffer = 64
	)

	syms := make([]string, nSymbols)
	for i := range syms {
		syms[i] = "S" + strconv.Itoa(i)
	}

	// A coalescing 1-minute feed with a deliberately small buffer. A drain
	// goroutine empties pushCh into `got` so emits never block on the buffer; the
	// emit() path still drops (with a warn) if it ever sees a full channel, which
	// we assert never happens by counting received vs. expected.
	feed := NewMoomooFeed(syms, qotcommon.KLType_KLType_1Min, smallBuffer, zerolog.Nop())

	var (
		mu   sync.Mutex
		got  = make(map[string][]domain.Bar) // symbol -> closed bars in arrival order
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		for b := range feed.pushCh {
			mu.Lock()
			got[b.Symbol] = append(got[b.Symbol], b)
			mu.Unlock()
		}
	}()

	base := time.Date(2026, time.June, 15, 14, 30, 0, 0, time.UTC) // 10:30 ET

	// Drive the flood: for each minute, push `pushesPerMin` forming updates per
	// symbol with the SAME barTS but a STRICTLY RISING close (so the final value
	// of each minute is unambiguous). The minute rollover (next barTS) closes the
	// prior minute. Symbols are interleaved within each tick to mimic the real
	// multiplexed push stream.
	// Push nMinutes+1 minutes of flood: the (m+1)-th minute's first push CLOSES
	// minute m via the production close-detect path (a strictly-newer barTS), so
	// all nMinutes asserted minutes are emitted by a real successor push — not by
	// the shutdown FlushPending. The final (nMinutes-th) minute stays pending.
	totalPushes := 0
	for m := 0; m < nMinutes+1; m++ {
		barTS := base.Add(time.Duration(m) * time.Minute)
		for tick := 0; tick < pushesPerMin; tick++ {
			// close rises within the minute: last tick (pushesPerMin-1) is final.
			c := price(100 + m*100 + tick)
			for _, s := range syms {
				bar := domain.Bar{
					Symbol: s, TS: barTS,
					Open: price(100 + m*100), High: c, Low: price(100 + m*100),
					Close: c, Volume: int64(1000 + tick),
				}
				feed.PushHandler(s, qotcommon.KLType_KLType_1Min, []domain.Bar{bar})
				totalPushes++
			}
		}
	}

	// (c) Bounded memory: pending holds exactly one forming bar per symbol — the
	// still-open LAST minute — never the push count.
	feed.mu.Lock()
	pendingLen := len(feed.pending)
	feed.mu.Unlock()
	if pendingLen != nSymbols {
		t.Fatalf("pending map size = %d, want %d (one forming bar per symbol)", pendingLen, nSymbols)
	}

	// Close pushCh to end the drain goroutine. We do NOT FlushPending here: the
	// nMinutes asserted bars were already closed by successor pushes above, so the
	// drain is fully observed without depending on the shutdown burst (which is
	// covered separately by TestPushHandlerStaleAndWrongTypeIgnored).
	close(feed.pushCh)
	<-done

	// Per symbol: exactly nMinutes closed bars, in strict TS order, each carrying
	// the FINAL OHLCV of its minute.
	mu.Lock()
	defer mu.Unlock()
	receivedTotal := 0
	for _, s := range syms {
		bars := got[s]
		receivedTotal += len(bars)
		if len(bars) != nMinutes {
			t.Fatalf("symbol %s: got %d closed bars, want %d (one per minute)", s, len(bars), nMinutes)
		}
		for m, b := range bars {
			wantTS := base.Add(time.Duration(m) * time.Minute)
			if !b.TS.Equal(wantTS) {
				t.Fatalf("symbol %s bar %d: TS=%s want %s (out of order)", s, m, b.TS, wantTS)
			}
			if m > 0 && !bars[m-1].TS.Before(b.TS) {
				t.Fatalf("symbol %s: bars not strictly TS-ascending at %d", s, m)
			}
			// (a)+(d): the emitted bar must be the LAST forming value of the minute.
			wantClose := price(100 + m*100 + (pushesPerMin - 1))
			if b.Close != wantClose {
				t.Fatalf("symbol %s minute %d: Close=%s want %s (must be the minute's FINAL value)",
					s, m, b.Close, wantClose)
			}
			if b.Volume != int64(1000+(pushesPerMin-1)) {
				t.Fatalf("symbol %s minute %d: Volume=%d want %d (latest)", s, m, b.Volume, 1000+(pushesPerMin-1))
			}
		}
	}

	// (b) Zero drops: every (symbol, minute) closed bar arrived; nothing dropped
	// despite the tiny buffer and the heavy flood.
	wantTotal := nSymbols * nMinutes
	if receivedTotal != wantTotal {
		t.Fatalf("received %d closed bars, want %d (drops detected)", receivedTotal, wantTotal)
	}

	// Document the collapse ratio the fix achieves.
	t.Logf("flood collapse: %d forming pushes -> %d closed emits (%.0fx reduction)",
		totalPushes, wantTotal, float64(totalPushes)/float64(wantTotal))
}

// TestPushHandlerDailyPathUnchanged asserts the daily (KLType_Day) path is NOT
// coalesced: every pushed daily bar forwards directly and promptly (one push per
// day already), preserving the existing live-feed semantics.
func TestPushHandlerDailyPathUnchanged(t *testing.T) {
	syms := []string{"E1", "E2"}
	feed := NewMoomooFeed(syms, qotcommon.KLType_KLType_Day, 16, zerolog.Nop())
	if feed.coalesce {
		t.Fatalf("daily feed must NOT enable coalesce")
	}

	got := make(map[string]int)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for b := range feed.pushCh {
			got[b.Symbol]++
		}
	}()

	days := []time.Time{
		time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.June, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.June, 12, 0, 0, 0, 0, time.UTC),
	}
	for _, d := range days {
		for _, s := range syms {
			feed.PushHandler(s, qotcommon.KLType_KLType_Day, []domain.Bar{
				{Symbol: s, TS: d, Open: price(50), High: price(50), Low: price(50), Close: price(50), Volume: 10},
			})
		}
	}
	close(feed.pushCh)
	<-done

	// Each daily bar forwarded immediately — no one-bar emit delay from pending.
	for _, s := range syms {
		if got[s] != len(days) {
			t.Fatalf("daily symbol %s: forwarded %d bars, want %d (direct, no coalesce)", s, got[s], len(days))
		}
	}
}

// TestPushHandlerStaleAndWrongTypeIgnored covers the close-detect edge cases:
// a stale (older barTS) push is ignored, and a wrong-KLType push is dropped.
func TestPushHandlerStaleAndWrongTypeIgnored(t *testing.T) {
	feed := NewMoomooFeed([]string{"X"}, qotcommon.KLType_KLType_1Min, 16, zerolog.Nop())

	got := make([]domain.Bar, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for b := range feed.pushCh {
			got = append(got, b)
		}
	}()

	t0 := time.Date(2026, time.June, 15, 14, 30, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t1.Add(time.Minute)

	push := func(ts time.Time, close int, kl qotcommon.KLType) {
		feed.PushHandler("X", kl, []domain.Bar{
			{Symbol: "X", TS: ts, Open: price(close), High: price(close), Low: price(close), Close: price(close), Volume: 1},
		})
	}

	push(t0, 10, qotcommon.KLType_KLType_1Min) // forms minute 0
	push(t1, 20, qotcommon.KLType_KLType_1Min) // closes minute 0 (emit close=10), forms minute 1
	push(t0, 99, qotcommon.KLType_KLType_1Min) // STALE: older than pending t1 -> ignored
	push(t1, 25, qotcommon.KLType_KLType_1Min) // same minute -> coalesce (close now 25)
	push(t0, 88, qotcommon.KLType_KLType_Day)  // WRONG type -> ignored entirely
	push(t2, 30, qotcommon.KLType_KLType_1Min) // closes minute 1 (emit close=25), forms minute 2

	feed.FlushPending() // flush minute 2 (close=30)
	close(feed.pushCh)
	<-done

	if len(got) != 3 {
		t.Fatalf("got %d emits, want 3 (minutes 0,1,2)", len(got))
	}
	wantCloses := []domain.Price{price(10), price(25), price(30)}
	wantTS := []time.Time{t0, t1, t2}
	for i, b := range got {
		if b.Close != wantCloses[i] || !b.TS.Equal(wantTS[i]) {
			t.Fatalf("emit %d: TS=%s close=%s want TS=%s close=%s", i, b.TS, b.Close, wantTS[i], wantCloses[i])
		}
	}
}
