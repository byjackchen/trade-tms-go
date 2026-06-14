package moomoo

import (
	"context"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Flatten closes all open broker positions with FLAT market orders, gated by the
// confirmation phrase, idempotently.
func TestFlattenOnKillClosesAllPositions(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()

	// Open a long AAPL and a short MSFT.
	long, _ := e.SubmitMarket("S", "AAPL", domain.OrderSideBuy, 100, "x", ts)
	_ = venue.Accept(long)
	_ = venue.Fill(long, domain.MustPrice("150.00"))
	short, _ := e.SubmitMarket("S", "MSFT", domain.OrderSideSell, 50, "x", ts)
	_ = venue.Accept(short)
	_ = venue.Fill(short, domain.MustPrice("300.00"))

	// Wrong phrase => refused.
	if _, err := e.Flatten(context.Background(), "nope", "kill"); err == nil {
		t.Fatal("flatten must require the confirmation phrase")
	}

	coids, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	if len(coids) != 2 {
		t.Fatalf("flatten should submit 2 closing orders (long+short), got %d", len(coids))
	}
	// Fill the closes and verify the venue is flat.
	for _, c := range coids {
		_ = venue.Accept(c)
	}
	// Close prices arbitrary; the close side is opposite the open side.
	for _, c := range coids {
		_ = venue.Fill(c, domain.MustPrice("100.00"))
	}
	positions, _ := venue.GetPositionList(context.Background(), paperAcc, e.Env())
	if len(positions) != 0 {
		t.Fatalf("venue must be flat after flatten+fill, got %+v", positions)
	}

	// Idempotent: a second flatten on an already-flat book submits nothing.
	coids2, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatal(err)
	}
	if len(coids2) != 0 {
		t.Fatalf("flatten on a flat book must submit no orders, got %d", len(coids2))
	}
}

// Crash recovery: after a restart, RestoreFromBroker rebuilds the in-flight
// order's cumulative snapshot so the NEXT push applies a correct delta (no
// re-count of fills that settled before the crash), re-keys the restored order
// to its ORIGINATING strategy (so the post-restore fill is attributed correctly,
// not to an empty-strategy orphan), and returns the broker positions.
func TestRestoreFromBrokerRebuildsCumulativeSnapshot(t *testing.T) {
	const strat = "SEPARunner-000"
	// Pre-crash executor fills 40 of a 100-lot order, then we simulate a crash by
	// building a FRESH executor against the same venue (state lost).
	e, venue, _, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, _ := e.SubmitMarket(strat, "AAPL", domain.OrderSideBuy, 100, "x", ts)
	_ = venue.Accept(coid)
	_ = venue.PartialFill(coid, 40, domain.MustPrice("150.00"))

	// The durable submit record (live.orders) carries coid -> strategy; the
	// resolver replays that across the crash so attribution survives the restart.
	resolver := newMapStrategyResolver()
	resolver.put(coid, strat)

	// --- crash: new executor + new account, same venue ---
	acct2 := newFakeAccount()
	sink2 := &recordSink{}
	e2, err := New(context.Background(), Config{
		Mode: ModePaper, Client: venue, AccID: paperAcc, TraderID: "PAPER-SMOKE-001",
		Sink: sink2, Account: acct2, Strategy: resolver, Clock: fixedClock{t: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	positions, err := e2.RestoreFromBroker(context.Background())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Broker shows 40 long AAPL from the pre-crash partial.
	if len(positions) != 1 || positions[0].Symbol != "AAPL" || positions[0].Qty != 40 {
		t.Fatalf("restore positions: %+v", positions)
	}
	// The restored order must carry the originating strategy id (re-keyed from the
	// durable submit record), NOT an empty string.
	tracked := e2.TrackedOrders()
	if len(tracked) != 1 || tracked[0].StrategyID != strat {
		t.Fatalf("restored order must re-key to strategy %q, got %+v", strat, tracked)
	}
	// The restored order has CumQty=40, so the remaining 60 fill emits a 60 delta
	// (not 100) — proving no double-count across the crash.
	if err := venue.PartialFill(coid, 60, domain.MustPrice("150.00")); err != nil {
		t.Fatal(err)
	}
	fills := sink2.all()
	if len(fills) != 1 || fills[0].Qty != 60 {
		t.Fatalf("post-restore fill must be the 60-share remainder, got %+v", fills)
	}
	// ATTRIBUTION (the fix): the post-restore fill settles under the ORIGINAL
	// strategy, not an empty-strategy orphan.
	if fills[0].StrategyID != strat {
		t.Fatalf("post-restore fill must attribute to %q, got %q", strat, fills[0].StrategyID)
	}
	// And it lands in that strategy's net position (no empty-strategy orphan).
	if pos, ok := acct2.Position(strat, "AAPL"); !ok || pos.SignedQty != 60 {
		t.Fatalf("post-restore fill must net into %s/AAPL (got ok=%v %+v)", strat, ok, pos)
	}
	if _, orphan := acct2.Position("", "AAPL"); orphan {
		t.Fatal("post-restore fill must NOT create an empty-strategy orphan position")
	}
}

// Crash recovery SAFETY: an in-flight order whose strategy id cannot be resolved
// (no durable record / no resolver) must NOT silently attribute its fills to the
// empty strategy. RestoreFromBroker still restores positions (integrity intact)
// but reports the attribution gap, and any subsequent fill on the unresolved
// order is REJECTED by the strengthened Fill.Validate rather than mis-attributed.
func TestRestoreFromBrokerUnresolvedStrategyFailsLoud(t *testing.T) {
	const strat = "SEPARunner-000"
	e, venue, _, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, _ := e.SubmitMarket(strat, "AAPL", domain.OrderSideBuy, 100, "x", ts)
	_ = venue.Accept(coid)
	_ = venue.PartialFill(coid, 40, domain.MustPrice("150.00"))

	// --- crash: fresh executor, EMPTY resolver (the durable record is missing) ---
	acct2 := newFakeAccount()
	sink2 := &recordSink{}
	e2, err := New(context.Background(), Config{
		Mode: ModePaper, Client: venue, AccID: paperAcc, TraderID: "PAPER-SMOKE-001",
		Sink: sink2, Account: acct2, Strategy: newMapStrategyResolver(),
		Clock: fixedClock{t: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	positions, rerr := e2.RestoreFromBroker(context.Background())
	if rerr == nil {
		t.Fatal("restore must report an attribution gap for an unresolved in-flight order")
	}
	// Positions are STILL restored (integrity is never compromised).
	if len(positions) != 1 || positions[0].Qty != 40 {
		t.Fatalf("positions must still restore despite the attribution gap, got %+v", positions)
	}
	// A later fill on the unresolved order is rejected (empty StrategyID) — it
	// never settles into accounting as an orphan.
	if err := venue.PartialFill(coid, 60, domain.MustPrice("150.00")); err != nil {
		t.Fatal(err)
	}
	if got := sink2.all(); len(got) != 0 {
		t.Fatalf("a fill on an unresolved-strategy order must not be emitted, got %+v", got)
	}
	if _, orphan := acct2.Position("", "AAPL"); orphan {
		t.Fatal("unresolved-strategy fill must NOT create an empty-strategy orphan position")
	}
}
