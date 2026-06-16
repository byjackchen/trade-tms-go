package moomoo

import (
	"context"
	"testing"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TestSyncBrokerIntoReflectsExternalPosition proves DIRECTION 2 at the executor
// layer: a broker position that was NEVER placed via TMS is reflected into the
// MANUAL/EXTERNAL book (a synthetic settling fill for the broker net), with the
// order/fill/position persisted — and it is READ-ONLY (no PlaceOrder at the venue).
func TestSyncBrokerIntoReflectsExternalPosition(t *testing.T) {
	e, venue, acct, _, persist := newPaperExecutor(t)
	ctx := context.Background()

	// The operator traded directly in moomoo: broker holds long 25 TSLA.
	venue.SetPosition("TSLA", 25, domain.MustPrice("250.00"))

	res, err := e.SyncBrokerInto(ctx, "MANUAL")
	if err != nil {
		t.Fatalf("SyncBrokerInto: %v", err)
	}
	if res.PositionsObserved != 1 || res.Reflected != 1 {
		t.Fatalf("want 1 observed/1 reflected, got %+v", res)
	}

	// The MANUAL book now mirrors the broker truth.
	pos, ok := acct.Position("MANUAL", "TSLA")
	if !ok || pos.SignedQty != 25 {
		t.Fatalf("want MANUAL long 25 TSLA, got %+v ok=%v", pos, ok)
	}

	// READ-ONLY: the sync placed NO order at the venue.
	orders, _ := venue.GetOrderList(ctx, paperAcc, mo.TrdEnvSimulate)
	if len(orders) != 0 {
		t.Fatalf("sync must place NO venue order (read-only), got %d", len(orders))
	}

	// The reflection persisted a synthetic order + fill + position.
	if len(persist.fills) != 1 {
		t.Fatalf("want 1 persisted reflect fill, got %d", len(persist.fills))
	}
	if len(persist.orders) != 1 {
		t.Fatalf("want 1 persisted reflect order, got %d", len(persist.orders))
	}
}

// TestSyncBrokerIntoIdempotent proves re-syncing the SAME broker state reflects
// nothing + settles no second fill (no double count).
func TestSyncBrokerIntoIdempotent(t *testing.T) {
	e, venue, acct, _, persist := newPaperExecutor(t)
	ctx := context.Background()
	venue.SetPosition("AMD", 40, domain.MustPrice("120.00"))

	if _, err := e.SyncBrokerInto(ctx, "MANUAL"); err != nil {
		t.Fatal(err)
	}
	res2, err := e.SyncBrokerInto(ctx, "MANUAL")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Reflected != 0 {
		t.Fatalf("re-sync must reflect nothing, got %d", res2.Reflected)
	}
	pos, _ := acct.Position("MANUAL", "AMD")
	if pos.SignedQty != 40 {
		t.Fatalf("re-sync double-counted: want 40, got %d", pos.SignedQty)
	}
	if len(persist.fills) != 1 {
		t.Fatalf("re-sync settled a second fill: want 1, got %d", len(persist.fills))
	}
}

// TestSyncBrokerIntoReflectsExternalClose proves a symbol the book holds but the
// broker no longer reports is driven back to flat.
func TestSyncBrokerIntoReflectsExternalClose(t *testing.T) {
	e, venue, acct, _, _ := newPaperExecutor(t)
	ctx := context.Background()

	venue.SetPosition("MSFT", 30, domain.MustPrice("400.00"))
	if _, err := e.SyncBrokerInto(ctx, "MANUAL"); err != nil {
		t.Fatal(err)
	}
	// Operator flattened MSFT in moomoo.
	venue.SetPosition("MSFT", 0, domain.MustPrice("0"))
	res, err := e.SyncBrokerInto(ctx, "MANUAL")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reflected != 1 {
		t.Fatalf("want the close reflected, got %d", res.Reflected)
	}
	pos, _ := acct.Position("MANUAL", "MSFT")
	if pos.SignedQty != 0 {
		t.Fatalf("want flat after external close, got %d", pos.SignedQty)
	}
}

// TestSyncBrokerIntoRequiresStrategy proves an empty strategy id is rejected.
func TestSyncBrokerIntoRequiresStrategy(t *testing.T) {
	e, _, _, _, _ := newPaperExecutor(t)
	if _, err := e.SyncBrokerInto(context.Background(), ""); err == nil {
		t.Fatal("want error for empty strategy id")
	}
}
