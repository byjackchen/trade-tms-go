package moomoo

import (
	"testing"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func tid(st *OrderState, i int) string { return st.VenueOrderID + "-" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digs := []byte{}
	for i > 0 {
		digs = append([]byte{byte('0' + i%10)}, digs...)
		i /= 10
	}
	return string(digs)
}

func newState() *OrderState {
	return &OrderState{
		ClientOrderID: "PAPER-O-0", VenueOrderID: "V1", StrategyID: "S", Symbol: "AAPL",
		Side: domain.OrderSideBuy, OrderQty: 100, Status: domain.OrderStatusSubmitted,
	}
}

func upd(status trdcommon.OrderStatus, cumQty domain.Qty, avg domain.Price) mo.OrderUpdate {
	return mo.OrderUpdate{
		ClientOrderID: "PAPER-O-0", VenueOrderID: "V1", Symbol: "AAPL", Side: domain.OrderSideBuy,
		RawStatus: int32(status), OrderQty: 100, DealtQty: cumQty, DealtAvgPrice: avg,
	}
}

func TestSMAcceptThenFill(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()

	eff, err := Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0), tid, wall)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff) != 1 || eff[0].Kind != EffectAccepted || st.Status != domain.OrderStatusAccepted {
		t.Fatalf("accept: %+v status=%s", eff, st.Status)
	}

	eff, err = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Filled_All, 100, domain.MustPrice("150.00")), tid, wall)
	if err != nil {
		t.Fatal(err)
	}
	// Expect a fill delta (100@150) then a FILLED status.
	if len(eff) != 2 || eff[0].Kind != EffectFill || eff[1].Status != domain.OrderStatusFilled {
		t.Fatalf("fill: %+v", eff)
	}
	if eff[0].Fill.Qty != 100 || eff[0].Fill.Price != domain.MustPrice("150.00") {
		t.Fatalf("fill delta wrong: %+v", eff[0].Fill)
	}
	if !st.IsTerminal() {
		t.Fatal("FILLED must be terminal")
	}
}

func TestSMPartialThenFullDeltas(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	_, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0), tid, wall)

	// Partial 40 @ 200 (cumulative).
	eff, _ := Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Filled_Part, 40, domain.MustPrice("200.00")), tid, wall)
	if eff[0].Fill.Qty != 40 || st.Status != domain.OrderStatusPartiallyFilled {
		t.Fatalf("partial1: %+v status=%s", eff, st.Status)
	}
	// Cumulative 100 with new avg 200.60 => delta 60 @ (100*200.60 - 40*200)/60 = 201.00.
	eff, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Filled_All, 100, domain.MustPrice("200.60")), tid, wall)
	if eff[0].Fill.Qty != 60 {
		t.Fatalf("delta qty want 60, got %d", eff[0].Fill.Qty)
	}
	if eff[0].Fill.Price != domain.MustPrice("201.00") {
		t.Fatalf("delta price want 201.00, got %s", eff[0].Fill.Price)
	}
}

func TestSMDuplicateFillIsNoOp(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	_, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0), tid, wall)
	full := upd(trdcommon.OrderStatus_OrderStatus_Filled_All, 100, domain.MustPrice("150.00"))
	_, _ = Apply(st, full, tid, wall)
	// Replay identical terminal push: order already terminal -> nil effects.
	eff, err := Apply(st, full, tid, wall)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff) != 0 {
		t.Fatalf("duplicate terminal push must yield no effects, got %+v", eff)
	}
}

func TestSMDuplicatePartialNoReFill(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	_, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0), tid, wall)
	p := upd(trdcommon.OrderStatus_OrderStatus_Filled_Part, 40, domain.MustPrice("200.00"))
	_, _ = Apply(st, p, tid, wall)
	// Same cumulative qty again: no new fill delta.
	eff, _ := Apply(st, p, tid, wall)
	for _, e := range eff {
		if e.Kind == EffectFill {
			t.Fatalf("duplicate partial must not re-emit a fill: %+v", e)
		}
	}
}

func TestSMRejectTerminal(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	eff, _ := Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Failed, 0, 0), tid, wall)
	if len(eff) != 1 || eff[0].Status != domain.OrderStatusRejected || !st.IsTerminal() {
		t.Fatalf("reject: %+v", eff)
	}
	// Post-terminal push ignored.
	eff, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Filled_All, 100, domain.MustPrice("1.00")), tid, wall)
	if len(eff) != 0 {
		t.Fatal("post-reject push must be ignored")
	}
}

func TestSMFillCancelledFlags(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	_, _ = Apply(st, upd(trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0), tid, wall)
	eff, _ := Apply(st, upd(trdcommon.OrderStatus_OrderStatus_FillCancelled, 0, 0), tid, wall)
	if len(eff) != 1 || !eff[0].FillReversed || eff[0].Status != domain.OrderStatusCanceled {
		t.Fatalf("fill-cancelled: %+v", eff)
	}
}

func TestSMTransientIgnored(t *testing.T) {
	st := newState()
	wall := time.Now().UTC()
	for _, s := range []trdcommon.OrderStatus{
		trdcommon.OrderStatus_OrderStatus_Submitting,
		trdcommon.OrderStatus_OrderStatus_WaitingSubmit,
		trdcommon.OrderStatus_OrderStatus_Cancelling_All,
	} {
		eff, _ := Apply(st, upd(s, 0, 0), tid, wall)
		if len(eff) != 0 {
			t.Fatalf("transient %v must yield no effects", s)
		}
	}
	if st.Status != domain.OrderStatusSubmitted {
		t.Fatalf("transient must not change status, got %s", st.Status)
	}
}
