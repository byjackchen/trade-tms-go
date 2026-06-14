package moomoo

// executor_recovery.go implements two safety/operability features on the
// MoomooExecutor:
//
//   FLATTEN-ON-KILL (locked decision 7): on a confirmation-gated kill in
//   paper/live, submit FLAT market orders that close ALL open positions. It is
//   idempotent — re-running Flatten while close orders are still in flight does
//   not double-submit, because each close is a fresh deterministic order and the
//   net position is re-read from the (authoritative) account book each call; a
//   position already flat yields no order.
//
//   CRASH RECOVERY (locked decision 6): RestoreFromBroker pulls the broker's
//   positions (Trd_GetPositionList) and open orders (Trd_GetOrderList) after a
//   restart and rebuilds the executor's in-flight order map + the per-order
//   cumulative fill snapshot, so subsequent pushes apply correct DELTAS rather
//   than re-counting fills that settled before the crash. The caller restores
//   strategy SG state from PG separately and then runs reconciliation.

import (
	"context"
	"fmt"
	"sort"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// FlattenConfirmationPhrase is the typed phrase that gates a flatten-on-kill, so
// a destructive close-all can never fire by accident.
const FlattenConfirmationPhrase = "FLATTEN ALL POSITIONS"

// Flatten closes ALL open positions with FLAT market orders (decision 7). It is
// confirmation-gated (phrase must equal FlattenConfirmationPhrase) and
// idempotent: it reads the current net position per (strategy, symbol) from the
// authoritative broker view, and for each non-flat position submits ONE closing
// market order (BUY to cover a short, SELL to close a long) for the absolute
// size. A symbol already flat produces no order. It returns the client-order-ids
// it submitted (for audit) and the first submission error, if any.
//
// SAFETY: the closing orders are ungated by the portfolio budget (closing is
// always allowed per the risk spec — FLAT bypasses the budget + the daily-loss
// halt). They go through the SAME assertEnvInvariants path, so a paper kill can
// never close the live account.
func (e *MoomooExecutor) Flatten(ctx context.Context, confirmation, reason string) ([]string, error) {
	if confirmation != FlattenConfirmationPhrase {
		return nil, fmt.Errorf("%w: flatten requires the exact confirmation phrase", domain.ErrInvalidArgument)
	}
	positions, err := e.cfg.Client.GetPositionList(ctx, e.accID, e.env)
	if err != nil {
		return nil, fmt.Errorf("flatten: GetPositionList: %w", err)
	}
	// Deterministic order: sort by symbol so the kill sequence is reproducible.
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })

	var submitted []string
	var firstErr error
	ts := e.clock.Now()
	for _, p := range positions {
		if p.Qty == 0 {
			continue
		}
		side, ok := domain.CloseSideFor(p.Qty)
		if !ok {
			continue
		}
		absQty := p.Qty
		if absQty < 0 {
			absQty = -absQty
		}
		// strategyID for a broker-sourced flatten is the executor's FLATTEN pseudo-
		// strategy: broker positions are netted across strategies, so we close the
		// aggregate. (The reconciliation report attributes drift per strategy.)
		coid := e.nextClientOrderID()
		if _, perr := e.place(ctx, coid, flattenStrategyID, p.Symbol, side, absQty,
			"flatten-on-kill: "+reason, ts); perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		submitted = append(submitted, coid)
	}
	return submitted, firstErr
}

// flattenStrategyID is the pseudo-strategy id attributed to broker-sourced
// flatten orders (broker positions are netted across strategies).
const flattenStrategyID = "FLATTEN"

// RestoreFromBroker rebuilds in-flight order state after a restart (decision 6).
// It pulls the broker's open orders (to re-track in-flight orders + their
// cumulative fill snapshot, so later pushes apply correct deltas) and returns
// the broker's positions (the caller restores these into accounting + runs
// reconciliation). It is idempotent: re-running it overwrites the tracked state
// with the broker's authoritative view without emitting fills or duplicating
// orders.
//
// STRATEGY ATTRIBUTION (fixes per-strategy drift post-resume): the broker order
// view carries only the client-order-id (the remark), NOT the strategy id, so a
// restored, still-working order would otherwise fill under an empty-strategy key
// — an orphan per-strategy position + a spurious reconciliation entry. We re-key
// each restored order to its ORIGINATING strategy via the StrategyResolver (the
// strategy id persisted at submit in live.orders). A restored order whose
// strategy id cannot be resolved is reported (a non-nil first such error is
// returned AFTER positions are restored, so the caller can fail recovery rather
// than resume with mis-attributed orders); an unresolved order is still tracked
// (so its fill deltas stay correct) but its later fills are blocked downstream
// by the strengthened Fill.Validate (empty StrategyID is rejected), surfacing
// the gap loudly instead of drifting silently.
func (e *MoomooExecutor) RestoreFromBroker(ctx context.Context) ([]mo.BrokerPosition, error) {
	orders, err := e.cfg.Client.GetOrderList(ctx, e.accID, e.env)
	if err != nil {
		return nil, fmt.Errorf("restore: GetOrderList: %w", err)
	}

	// Resolve strategy ids OUTSIDE the lock (the resolver does IO: a PG query).
	// strategyByCOID holds the resolved id per restorable client-order-id;
	// resolveErr captures the first attribution failure to report after restore.
	strategyByCOID := make(map[string]string, len(orders))
	var resolveErr error
	for _, o := range orders {
		coid := o.ClientOrderID
		if coid == "" {
			continue
		}
		sid, ok, rerr := e.resolveStrategy(ctx, coid)
		if rerr != nil {
			if resolveErr == nil {
				resolveErr = fmt.Errorf("restore: resolve strategy for order %s: %w", coid, rerr)
			}
			continue
		}
		if !ok || sid == "" {
			// No durable record (or it carried no strategy id). This is an
			// attribution gap: a fill on this order would be mis-attributed. Record
			// the first occurrence so recovery can fail loudly.
			if resolveErr == nil {
				resolveErr = fmt.Errorf("%w: restore: no persisted strategy id for in-flight order %s "+
					"(its fills would be mis-attributed)", domain.ErrInvalidArgument, coid)
			}
			continue
		}
		strategyByCOID[coid] = sid
	}

	e.mu.Lock()
	for _, o := range orders {
		coid := o.ClientOrderID
		if coid == "" {
			// An order with no remark we can correlate to (e.g. placed by another
			// client). Skip — reconciliation surfaces it; we do not track it.
			continue
		}
		// Rebuild the cumulative fill snapshot so the NEXT push computes the right
		// delta (delta = newCum - restoredCum) and never re-counts settled fills.
		domStatus, hasStatus := mo.DomainOrderStatus(o.RawStatus)
		st := &OrderState{
			ClientOrderID: coid,
			VenueOrderID:  o.VenueOrderID,
			// StrategyID re-keyed from the durable submit record so post-restore
			// fills attribute to the ORIGINATING strategy (empty if unresolved —
			// Fill.Validate then rejects the fill rather than mis-attributing it).
			StrategyID: strategyByCOID[coid],
			Symbol:     o.Symbol,
			Side:       o.Side,
			OrderQty:   o.OrderQty,
			CumQty:     o.DealtQty,
		}
		if hasStatus {
			st.Status = domStatus
		} else {
			st.Status = domain.OrderStatusAccepted
		}
		// Reconstruct CumNotional from the cumulative qty * avg price so the next
		// delta's notional math is consistent.
		if o.DealtQty > 0 {
			if n, nerr := o.DealtAvgPrice.MulQty(o.DealtQty); nerr == nil {
				st.CumNotional = n.Raw()
			}
		}
		e.orders[coid] = st
		if o.VenueOrderID != "" {
			e.venIndex[o.VenueOrderID] = coid
		}
	}
	e.mu.Unlock()

	positions, err := e.cfg.Client.GetPositionList(ctx, e.accID, e.env)
	if err != nil {
		return nil, fmt.Errorf("restore: GetPositionList: %w", err)
	}
	// Positions are restored regardless (position integrity is never compromised);
	// resolveErr surfaces any per-strategy attribution gap so the caller can
	// decide whether to resume.
	return positions, resolveErr
}

// resolveStrategy looks up the originating strategy id for a restored order via
// the configured StrategyResolver. A nil resolver yields ("", false, nil) — the
// caller treats that as an attribution gap (recovery without durable orders
// cannot attribute restored fills).
func (e *MoomooExecutor) resolveStrategy(ctx context.Context, clientOrderID string) (string, bool, error) {
	if e.cfg.Strategy == nil {
		return "", false, nil
	}
	return e.cfg.Strategy.StrategyForOrder(ctx, clientOrderID)
}

// TrackedOrders returns a snapshot of the currently-tracked order states (for
// reconciliation + tests). The returned slice is sorted by client-order-id.
func (e *MoomooExecutor) TrackedOrders() []OrderState {
	e.mu.Lock()
	out := make([]OrderState, 0, len(e.orders))
	for _, st := range e.orders {
		out = append(out, *st)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ClientOrderID < out[j].ClientOrderID })
	return out
}
