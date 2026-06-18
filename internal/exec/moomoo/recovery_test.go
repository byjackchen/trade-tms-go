package moomoo

import (
	"context"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Flatten closes all open positions with FLAT market orders, gated by the
// confirmation phrase, idempotently. The CORRECT MODEL: each close is submitted
// under the ORIGINATING (strategy, symbol) so the fill nets that BOOK row to 0
// -> CLOSED — no phantom 'FLATTEN|<sym>' rows are created.
func TestFlattenOnKillClosesAllPositions(t *testing.T) {
	e, venue, acct, _, persist := newPaperExecutor(t)
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
	// The closes MUST be submitted under the originating strategy 'S', NOT the
	// FLATTEN pseudo-strategy — that is what nets each book row to 0 -> CLOSED.
	for _, c := range coids {
		st := trackedByCOID(t, e, c)
		if st.StrategyID != "S" {
			t.Fatalf("close order %s must be attributed to the originating strategy 'S', got %q",
				c, st.StrategyID)
		}
		if st.StrategyID == flattenStrategyID {
			t.Fatalf("close order %s must NOT use the FLATTEN pseudo-strategy (phantom row)", c)
		}
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

	// BOOK truly flat: every originating row nets to 0 (CLOSED) and there is NO
	// phantom 'FLATTEN|<sym>' open row.
	if open := acct.OpenPositions(); len(open) != 0 {
		t.Fatalf("the per-strategy BOOK must be row-by-row flat after flatten, got open rows %+v", open)
	}
	if p, ok := acct.Position("S", "AAPL"); !ok || p.SignedQty != 0 {
		t.Fatalf("S/AAPL must net to 0 (CLOSED), got ok=%v %+v", ok, p)
	}
	if p, ok := acct.Position("S", "MSFT"); !ok || p.SignedQty != 0 {
		t.Fatalf("S/MSFT must net to 0 (CLOSED), got ok=%v %+v", ok, p)
	}
	if _, phantom := acct.Position(flattenStrategyID, "AAPL"); phantom {
		t.Fatal("flatten must not create a phantom FLATTEN|AAPL position row")
	}
	if _, phantom := acct.Position(flattenStrategyID, "MSFT"); phantom {
		t.Fatal("flatten must not create a phantom FLATTEN|MSFT position row")
	}
	// The LAST persisted position write for each originating row must be flat
	// (signed_qty 0), so UpsertPosition stamps status=CLOSED on the originating
	// '<strategy>|<sym>' row (the cockpit's open-position count then drops to 0).
	if last := lastPersistedPosition(persist, "S", "AAPL"); last == nil || last.SignedQty != 0 {
		t.Fatalf("last persisted S/AAPL must be flat (CLOSED), got %+v", last)
	}
	if last := lastPersistedPosition(persist, "S", "MSFT"); last == nil || last.SignedQty != 0 {
		t.Fatalf("last persisted S/MSFT must be flat (CLOSED), got %+v", last)
	}
	for _, p := range persist.positionsSnapshot() {
		if p.StrategyID == flattenStrategyID {
			t.Fatalf("no FLATTEN-strategy position must be persisted (phantom row), got %+v", p)
		}
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

// TestFlattenClosesEachStrategyRowMultiSameSymbol proves the CORRECT MODEL for
// the multi-strategy-same-symbol case: two strategies each hold a position in
// XLK; the flatten must close EACH under its OWN strategy id so both originating
// rows net to 0 -> CLOSED (not a single netted FLATTEN|XLK row that leaves both
// strategy rows phantom-open). This is the exact defect from the diagnosis.
func TestFlattenClosesEachStrategyRowMultiSameSymbol(t *testing.T) {
	e, venue, acct, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()

	// Two strategies, SAME symbol XLK: A long 348, B short 100. Broker nets to
	// +248, but the per-strategy BOOK holds two distinct open rows.
	a, _ := e.SubmitMarket("SectorRotation-001", "XLK", domain.OrderSideBuy, 348, "x", ts)
	_ = venue.Accept(a)
	_ = venue.Fill(a, domain.MustPrice("200.00"))
	b, _ := e.SubmitMarket("Hedge-002", "XLK", domain.OrderSideSell, 100, "x", ts)
	_ = venue.Accept(b)
	_ = venue.Fill(b, domain.MustPrice("200.00"))

	// Pre-flatten: two open book rows for the same symbol.
	if open := acct.OpenPositions(); len(open) != 2 {
		t.Fatalf("expected 2 open book rows (one per strategy) for XLK, got %+v", open)
	}

	coids, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	// EACH strategy row gets its OWN close, under its OWN strategy id — 2 orders,
	// none under FLATTEN (broker net is +248 but we never close the aggregate).
	if len(coids) != 2 {
		t.Fatalf("flatten must submit one close PER strategy row (2), got %d", len(coids))
	}
	gotStrats := map[string]domain.Qty{}
	for _, c := range coids {
		st := trackedByCOID(t, e, c)
		if st.StrategyID == flattenStrategyID {
			t.Fatalf("multi-strategy close must NOT net into a FLATTEN row, order %s used %q", c, st.StrategyID)
		}
		gotStrats[st.StrategyID] = st.OrderQty
	}
	if gotStrats["SectorRotation-001"] != 348 {
		t.Fatalf("SectorRotation-001 must close its OWN 348, got %v", gotStrats["SectorRotation-001"])
	}
	if gotStrats["Hedge-002"] != 100 {
		t.Fatalf("Hedge-002 must close its OWN 100, got %v", gotStrats["Hedge-002"])
	}

	// Fill the closes; both originating rows net to 0 and the venue is flat.
	for _, c := range coids {
		_ = venue.Accept(c)
		_ = venue.Fill(c, domain.MustPrice("200.00"))
	}
	if p, ok := acct.Position("SectorRotation-001", "XLK"); !ok || p.SignedQty != 0 {
		t.Fatalf("SectorRotation-001/XLK must net to 0 (CLOSED), got ok=%v %+v", ok, p)
	}
	if p, ok := acct.Position("Hedge-002", "XLK"); !ok || p.SignedQty != 0 {
		t.Fatalf("Hedge-002/XLK must net to 0 (CLOSED), got ok=%v %+v", ok, p)
	}
	if open := acct.OpenPositions(); len(open) != 0 {
		t.Fatalf("BOOK must be row-by-row flat after multi-strategy flatten, got %+v", open)
	}
	if _, phantom := acct.Position(flattenStrategyID, "XLK"); phantom {
		t.Fatal("multi-strategy flatten must not create a phantom FLATTEN|XLK row")
	}
	pos, _ := venue.GetPositionList(context.Background(), paperAcc, e.Env())
	if len(pos) != 0 {
		t.Fatalf("venue must be flat after multi-strategy flatten+fill, got %+v", pos)
	}
}

// TestFlattenBrokerDriftSweepClosesResidual proves the SAFETY drift sweep: when
// the broker holds MORE than the per-strategy book accounts for (true book-vs-
// broker drift, e.g. an externally-opened lot), the flatten still leaves the
// broker flat by closing the residual under FLATTEN — and nets that FLATTEN row
// to 0 so the sweep itself leaves no phantom OPEN row — while recording the drift.
func TestFlattenBrokerDriftSweepClosesResidual(t *testing.T) {
	venue := NewMockVenue(paperAcc)
	acct := newFakeAccount()
	sink := &recordSink{}
	persist := &recordPersist{}
	risk := &recordRisk{}
	e, err := New(context.Background(), Config{
		Account: domain.NewBrokerAccount("moomoo", domain.EnvPaper, paperAcc, ""), Client: venue, TraderID: "PAPER-SMOKE-001",
		Sink: sink, Book: acct, Persist: persist, Risk: risk,
		Clock: fixedClock{t: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC()

	// Book row: strategy S long 100 SPY (settles into both book and venue).
	o, _ := e.SubmitMarket("S", "SPY", domain.OrderSideBuy, 100, "x", ts)
	_ = venue.Accept(o)
	_ = venue.Fill(o, domain.MustPrice("400.00"))
	// DRIFT: the broker also holds 30 extra SPY the book never saw (external lot).
	venue.SetPosition("SPY", 130, domain.MustPrice("400.00"))

	coids, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	// One close for the book row (S, 100) + one drift-sweep close (FLATTEN, 30).
	if len(coids) != 2 {
		t.Fatalf("expected book close + drift sweep (2 orders), got %d: %+v", len(coids), coids)
	}
	var sweptFlatten bool
	for _, c := range coids {
		st := trackedByCOID(t, e, c)
		if st.StrategyID == flattenStrategyID {
			sweptFlatten = true
			if st.OrderQty != 30 {
				t.Fatalf("drift sweep must close only the 30-share residual, got %v", st.OrderQty)
			}
		}
	}
	if !sweptFlatten {
		t.Fatal("a broker residual beyond the book must be swept under FLATTEN")
	}
	// The drift must be recorded as a risk/reconciliation event.
	if !risk.has("exec.flatten_drift") {
		t.Fatalf("book-vs-broker drift must record a risk event, got %+v", risk.snapshot())
	}

	// Fill both closes: broker flat, and the FLATTEN sweep row nets to 0 (no
	// phantom OPEN row).
	for _, c := range coids {
		_ = venue.Accept(c)
		_ = venue.Fill(c, domain.MustPrice("400.00"))
	}
	pos, _ := venue.GetPositionList(context.Background(), paperAcc, e.Env())
	if len(pos) != 0 {
		t.Fatalf("venue must be flat after flatten+drift sweep, got %+v", pos)
	}
	if p, ok := acct.Position(flattenStrategyID, "SPY"); ok && p.SignedQty != 0 {
		t.Fatalf("the FLATTEN drift-sweep row must net to 0, not leave a phantom OPEN row, got %+v", p)
	}
	if open := acct.OpenPositions(); len(open) != 0 {
		t.Fatalf("book must be flat after flatten+drift sweep+fills, got %+v", open)
	}
}

// TestFlattenIdempotentWhileCloseInFlight proves flatten is idempotent against
// IN-FLIGHT closes (the double-submit blocker): a second flatten issued BEFORE
// the first close fills must submit ZERO new orders, and once the single close
// fills the book settles to FLAT — never oversold into a short.
//
// Without the in-flight guard the second flatten re-reads the still-+100 settled
// book (OpenPositions reflects only filled fills) and re-submits a full close;
// after BOTH closes fill the position is -100 (a phantom short), contradicting
// the flatten guarantee. The guard skips a book row that already has a non-
// terminal close working on its (strategy, symbol).
func TestFlattenIdempotentWhileCloseInFlight(t *testing.T) {
	e, venue, acct, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()

	// Open S/AAPL +100 (settles into book + venue).
	open, _ := e.SubmitMarket("S", "AAPL", domain.OrderSideBuy, 100, "x", ts)
	_ = venue.Accept(open)
	_ = venue.Fill(open, domain.MustPrice("150.00"))
	if p, ok := acct.Position("S", "AAPL"); !ok || p.SignedQty != 100 {
		t.Fatalf("setup: S/AAPL must be +100, got ok=%v %+v", ok, p)
	}

	// First flatten: one SELL close, left IN FLIGHT (not accepted/filled).
	first, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatalf("first flatten: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first flatten must submit exactly 1 close, got %d: %+v", len(first), first)
	}
	closeCOID := first[0]
	if st := trackedByCOID(t, e, closeCOID); st.Side != domain.OrderSideSell || st.IsTerminal() {
		t.Fatalf("the in-flight close must be a non-terminal SELL, got %+v", st)
	}

	// SECOND flatten BEFORE the first close fills: the settled book still shows
	// +100 (the close has not settled), but the working close must suppress a
	// re-submit — ZERO new orders.
	second, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatalf("second flatten: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("a second flatten while the close is in flight must submit ZERO new orders "+
			"(no double-submit), got %d: %+v", len(second), second)
	}

	// Now fill the single close. The book nets to 0 (FLAT) — NOT -100 (short).
	_ = venue.Accept(closeCOID)
	_ = venue.Fill(closeCOID, domain.MustPrice("155.00"))
	if p, ok := acct.Position("S", "AAPL"); !ok || p.SignedQty != 0 {
		t.Fatalf("after the single close fills the book must be FLAT (0), not oversold; got ok=%v %+v", ok, p)
	}
	if open := acct.OpenPositions(); len(open) != 0 {
		t.Fatalf("book must be row-by-row flat (no phantom short), got %+v", open)
	}
	pos, _ := venue.GetPositionList(context.Background(), paperAcc, e.Env())
	if len(pos) != 0 {
		t.Fatalf("venue must be flat (not short) after the single close fills, got %+v", pos)
	}

	// And once flat, a further flatten still submits nothing (settled idempotency).
	third, err := e.Flatten(context.Background(), FlattenConfirmationPhrase, "kill")
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("flatten on a flat book must submit no orders, got %d", len(third))
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
		Account: domain.NewBrokerAccount("moomoo", domain.EnvPaper, paperAcc, ""), Client: venue, TraderID: "PAPER-SMOKE-001",
		Sink: sink2, Book: acct2, Strategy: resolver, Clock: fixedClock{t: ts},
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
		Account: domain.NewBrokerAccount("moomoo", domain.EnvPaper, paperAcc, ""), Client: venue, TraderID: "PAPER-SMOKE-001",
		Sink: sink2, Book: acct2, Strategy: newMapStrategyResolver(),
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
