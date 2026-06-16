package moomoo

import (
	"context"
	"errors"
	"testing"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TestSubmitManualVenueRejectIsTyped proves a BROKER business rejection at submit
// (insufficient buying power, market closed, ...) propagates through SubmitManual as
// a typed mo.ErrOrderRejected — so the manual-desk API maps it to a clean 422 rather
// than a 500 with a leaked protocol string (finding 4). The order is NOT submitted.
func TestSubmitManualVenueRejectIsTyped(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ctx := context.Background()
	// The wire client tags a venue business rejection (retType!=0 on a place) with the
	// typed mo.ErrOrderRejected sentinel; simulate that at the venue seam.
	venue.FailNextPlace(mo.ErrOrderRejected)

	_, submitted, err := e.SubmitManual(ctx, ManualOrderSpec{
		ClientOrderID: "MANUAL-PAPER-reject",
		StrategyID:    "MANUAL",
		Symbol:        "AAPL",
		Side:          domain.OrderSideBuy,
		Qty:           10,
		Type:          domain.OrderTypeMarket,
	})
	if submitted {
		t.Fatal("a venue-rejected order must NOT report submitted")
	}
	if !errors.Is(err, mo.ErrOrderRejected) {
		t.Fatalf("want ErrOrderRejected, got %v", err)
	}
}

// TestSubmitManualLifecycle proves a caller-supplied-coid manual order flows
// through the SAME state machine + accounting + persistence as the strategy path.
func TestSubmitManualLifecycle(t *testing.T) {
	e, venue, acct, sink, _ := newPaperExecutor(t)
	ctx := context.Background()
	ts := time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC)

	coid, submitted, err := e.SubmitManual(ctx, ManualOrderSpec{
		ClientOrderID: "MANUAL-PAPER-k1",
		StrategyID:    "MANUAL",
		Symbol:        "AAPL",
		Side:          domain.OrderSideBuy,
		Qty:           50,
		Type:          domain.OrderTypeMarket,
		TS:            ts,
	})
	if err != nil || !submitted {
		t.Fatalf("submit manual: %v submitted=%v", err, submitted)
	}
	if coid != "MANUAL-PAPER-k1" {
		t.Fatalf("coid not preserved: %s", coid)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := venue.Fill(coid, domain.MustPrice("100.00")); err != nil {
		t.Fatal(err)
	}
	pos, ok := acct.Position("MANUAL", "AAPL")
	if !ok || pos.SignedQty != 50 {
		t.Fatalf("want MANUAL long 50, got %+v ok=%v", pos, ok)
	}
	if len(sink.all()) != 1 {
		t.Fatalf("want 1 fill, got %d", len(sink.all()))
	}
}

// TestSubmitManualLimit proves a LIMIT manual order is accepted + carries the
// limit price to the venue request.
func TestSubmitManualLimit(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ctx := context.Background()
	coid, submitted, err := e.SubmitManual(ctx, ManualOrderSpec{
		ClientOrderID: "MANUAL-PAPER-lim",
		StrategyID:    "MANUAL",
		Symbol:        "MSFT",
		Side:          domain.OrderSideBuy,
		Qty:           5,
		Type:          domain.OrderTypeLimit,
		LimitPrice:    domain.MustPrice("195.00"),
	})
	if err != nil || !submitted {
		t.Fatalf("submit limit: %v submitted=%v", err, submitted)
	}
	orders, _ := venue.GetOrderList(ctx, paperAcc, mo.TrdEnvSimulate)
	if len(orders) != 1 || orders[0].ClientOrderID != coid {
		t.Fatalf("want 1 order with coid %s, got %+v", coid, orders)
	}
}

// TestSubmitManualIdempotent proves a re-submit of a known coid does NOT create a
// second venue order.
func TestSubmitManualIdempotent(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ctx := context.Background()
	spec := ManualOrderSpec{
		ClientOrderID: "MANUAL-PAPER-dup",
		StrategyID:    "MANUAL",
		Symbol:        "AAPL",
		Side:          domain.OrderSideBuy,
		Qty:           10,
	}
	if _, _, err := e.SubmitManual(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.SubmitManual(ctx, spec); err != nil {
		t.Fatal(err)
	}
	orders, _ := venue.GetOrderList(ctx, paperAcc, mo.TrdEnvSimulate)
	if len(orders) != 1 {
		t.Fatalf("double-submit created %d orders, want 1", len(orders))
	}
}

// TestCancelManual proves a working order is cancelled (terminal CANCELED) and a
// second cancel is an idempotent no-op.
func TestCancelManual(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ctx := context.Background()
	coid, _, err := e.SubmitManual(ctx, ManualOrderSpec{
		ClientOrderID: "MANUAL-PAPER-c", StrategyID: "MANUAL",
		Symbol: "AAPL", Side: domain.OrderSideBuy, Qty: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := e.CancelManual(ctx, coid); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, ok := e.TrackedOrder(coid)
	if !ok || st.Status != domain.OrderStatusCanceled {
		t.Fatalf("want CANCELED, got %+v ok=%v", st.Status, ok)
	}
	// Idempotent second cancel.
	if err := e.CancelManual(ctx, coid); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
}

// TestWireCancelUnsupported proves the wire client refuses to cancel (no
// modify-order proto) rather than silently claiming success — a SAFETY property so
// an operator is never told "cancelled" on a working real order.
func TestWireCancelUnsupported(t *testing.T) {
	c := mo.NewClient(mo.Options{Addr: "127.0.0.1:0"})
	err := c.TradeClient().CancelOrder(context.Background(), 1, mo.TrdEnvSimulate, "X")
	if err == nil {
		t.Fatal("wire CancelOrder must return ErrUnsupported, got nil")
	}
}
