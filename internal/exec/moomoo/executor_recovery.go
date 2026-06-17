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
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// FlattenConfirmationPhrase is the typed phrase that gates a flatten-on-kill, so
// a destructive close-all can never fire by accident.
const FlattenConfirmationPhrase = "FLATTEN ALL POSITIONS"

// Flatten closes ALL open positions with FLAT market orders (decision 7). It is
// confirmation-gated (phrase must equal FlattenConfirmationPhrase) and
// idempotent.
//
// CORRECT MODEL (the per-strategy BOOK, not the broker aggregate):
//
//	A flatten must leave the per-strategy position BOOK truly flat — every open
//	'<strategy>|<symbol>' row CLOSED — AND the broker flat. The PRIMARY source is
//	therefore the executor's own AccountBook (the per-strategy book), NOT the
//	broker's GetPositionList (which nets ACROSS strategies). For each non-flat
//	book row we submit ONE closing market order under THAT SAME (strategy_id,
//	symbol) with the opposite signed qty, so the fill settles back into the SAME
//	(strategy, symbol) position and nets it to 0. UpsertPosition then writes
//	status=CLOSED on the ORIGINATING row instead of opening a phantom
//	'FLATTEN|<sym>' row. Multi-strategy-same-symbol each close under their own id.
//
//	Determinism: OpenPositions() returns the book rows in (strategy_id, symbol)
//	order, so the kill sequence is reproducible.
//
//	SAFETY drift sweep: after closing every book row we re-read the broker
//	(GetPositionList); any residual net per symbol is book-vs-broker DRIFT (the
//	book thought it was flat but the broker is not, or vice versa). We close that
//	residual too — under flattenStrategyID, netting any FLATTEN row it touches to
//	0 so the sweep NEVER leaves a phantom OPEN row — and emit a reconciliation/
//	risk event noting the drift. In normal operation (book == broker) there is no
//	residual and the sweep submits nothing.
//
//	Idempotency (settled book): a second flatten on an already-flat book finds no
//	open book rows and no broker residual, so it submits nothing (no new rows).
//
//	Idempotency (IN-FLIGHT closes): OpenPositions() returns only the SETTLED book,
//	so a close placed by a prior flatten that has not yet filled is still an open
//	book row here. Without a guard a second flatten (operator double-click, or a
//	flatten chased by an emergency-kill) re-submits a full set of closes and, once
//	both sets fill, OVER-SELLS the flat position into a new short. We therefore
//	skip any book row that already has a NON-TERMINAL working close order on its
//	(strategy, symbol) in the closing direction (hasWorkingCloseOrder): the close
//	is already in flight, so a repeat flatten submits zero new orders and the book
//	settles to flat — never short. The drift sweep applies the same guard under
//	the FLATTEN pseudo-strategy.
//
// SAFETY: the closing orders are ungated by the portfolio budget (closing is
// always allowed per the risk spec — FLAT bypasses the budget + the daily-loss
// halt). They go through the SAME assertEnvInvariants path, so a paper kill can
// never close the live account.
func (e *MoomooExecutor) Flatten(ctx context.Context, confirmation, reason string) ([]string, error) {
	if confirmation != FlattenConfirmationPhrase {
		return nil, fmt.Errorf("%w: flatten requires the exact confirmation phrase", domain.ErrInvalidArgument)
	}
	ts := e.clock.Now()
	var submitted []string
	var firstErr error

	// PRIMARY: close each per-strategy BOOK row under its OWN strategy id so the
	// fill nets the ORIGINATING '<strategy>|<symbol>' row to 0 -> CLOSED.
	// OpenPositions() is already sorted by (strategy_id, symbol) for determinism.
	for _, p := range e.cfg.Book.OpenPositions() {
		side, ok := domain.CloseSideFor(p.SignedQty)
		if !ok {
			continue // flat (defensive; OpenPositions excludes flats)
		}
		// IDEMPOTENCY: OpenPositions() is the SETTLED book, so a close placed by a
		// prior (in-flight) flatten still shows here as an open row. If a non-
		// terminal close in the same direction is already working on this
		// (strategy, symbol), the position is already being flattened — skip it so a
		// repeat flatten does not double-submit and over-sell into a short.
		if e.hasWorkingCloseOrder(p.StrategyID, p.Symbol, side) {
			continue
		}
		absQty := p.SignedQty
		if absQty < 0 {
			absQty = -absQty
		}
		coid := e.nextClientOrderID()
		if _, perr := e.place(ctx, coid, p.StrategyID, p.Symbol, side, absQty,
			"flatten-on-kill: "+reason, ts); perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		submitted = append(submitted, coid)
	}

	// SAFETY DRIFT SWEEP: after closing every book row, any residual broker net
	// per symbol is book-vs-broker drift. Close it under the FLATTEN pseudo-
	// strategy (broker positions are netted across strategies) and record the
	// drift. Closing the residual nets any FLATTEN|<sym> row it creates to 0, so
	// the sweep cannot leave a phantom OPEN row. In normal operation (book ==
	// broker) there is no residual.
	swept, serr := e.flattenBrokerDrift(ctx, reason, ts)
	submitted = append(submitted, swept...)
	if serr != nil && firstErr == nil {
		firstErr = serr
	}
	return submitted, firstErr
}

// flattenBrokerDrift re-reads the broker after the per-strategy book has been
// closed and submits a closing order for any residual net per symbol (a true
// book-vs-broker drift), under flattenStrategyID. It records a reconciliation/
// risk event for each drift so the divergence is surfaced, never silent. It
// returns the client-order-ids it submitted (empty when book == broker, the
// normal case) and the first submission error.
//
// NOTE: this reads the SAME broker view the per-strategy closes will settle
// against. The closing fills for the book rows are still in flight (the venue
// fills are pushed asynchronously), so on a healthy book the broker still shows
// the soon-to-close aggregate here. To avoid mistaking that in-flight close for
// drift, we subtract the qty the per-strategy book closes per symbol: only a NET
// residual beyond what the book closes is treated as drift.
//
// PHANTOM-ROW SAFETY: a true residual is a broker lot the per-strategy book
// never saw, so there is no originating book row for the close to net against —
// closing it bare would leave a FLATTEN|<sym> row OPEN at -residual (a NEW
// phantom). To keep flatten's "book row-by-row flat" guarantee, we first SEED
// the residual into the FLATTEN book position (an opening fill in the broker's
// direction, mirroring crash-recovery seeding of orphan broker lots), THEN
// submit the closing order under FLATTEN. When that close fills, FLATTEN|<sym>
// nets +residual -> 0 -> CLOSED, so the sweep leaves NO phantom OPEN row.
func (e *MoomooExecutor) flattenBrokerDrift(ctx context.Context, reason string, ts time.Time) ([]string, error) {
	positions, err := e.cfg.Client.GetPositionList(ctx, e.accID, e.env)
	if err != nil {
		return nil, fmt.Errorf("flatten: drift sweep GetPositionList: %w", err)
	}
	// closing[symbol] = signed qty the per-strategy book closes already cover
	// (sum of the book rows' signed qty: a long book row +N is being closed, so
	// the broker's +N is accounted for).
	closing := make(map[string]domain.Qty, len(positions))
	for _, p := range e.cfg.Book.OpenPositions() {
		closing[p.Symbol] += p.SignedQty
	}

	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })
	var submitted []string
	var firstErr error
	for _, p := range positions {
		// residual = broker net MINUS what the per-strategy book already closes.
		residual := p.Qty - closing[p.Symbol]
		if residual == 0 {
			continue
		}
		side, ok := domain.CloseSideFor(residual)
		if !ok {
			continue
		}
		// IDEMPOTENCY: if a prior flatten already has a non-terminal FLATTEN sweep
		// close working on this symbol, the residual is already being closed — skip
		// so a repeat flatten neither re-seeds the FLATTEN book (corrupting the
		// position) nor double-submits the sweep.
		if e.hasWorkingCloseOrder(flattenStrategyID, p.Symbol, side) {
			continue
		}
		absQty := residual
		if absQty < 0 {
			absQty = -absQty
		}
		e.recordRisk(ctx, flattenStrategyID, p.Symbol, "exec.flatten_drift",
			fmt.Sprintf("book-vs-broker drift on flatten: broker net %d, book closes %d, residual %d",
				p.Qty, closing[p.Symbol], residual))

		// SEED the residual into the FLATTEN book position so the close below nets
		// it to 0 (no phantom OPEN row). The opening fill carries the broker's side
		// (a +residual broker lot seeds a BUY into FLATTEN|<sym>); the close then
		// uses the opposite (CloseSideFor) side for the same absolute qty.
		px := p.AvgPrice
		if px <= 0 {
			px = p.Price
		}
		if px > 0 {
			openSide := domain.OrderSideBuy
			if residual < 0 {
				openSide = domain.OrderSideSell
			}
			seed := domain.Fill{
				TradeID:       fmt.Sprintf("FLATTEN-DRIFT-SEED-%s", p.Symbol),
				ClientOrderID: fmt.Sprintf("FLATTEN-DRIFT-SEED-%s", p.Symbol),
				StrategyID:    flattenStrategyID,
				Symbol:        p.Symbol,
				Side:          openSide,
				Qty:           absQty,
				Price:         px,
				TS:            ts,
			}
			if serr := seed.Validate(); serr == nil {
				if _, aerr := e.cfg.Book.ApplyFill(seed); aerr != nil {
					e.logf("moomoo executor: flatten drift seed %s: %v", p.Symbol, aerr)
				} else {
					e.persistPosition(ctx, mustPosition(e.cfg.Book, flattenStrategyID, p.Symbol))
				}
			}
		}

		coid := e.nextClientOrderID()
		if _, perr := e.place(ctx, coid, flattenStrategyID, p.Symbol, side, absQty,
			"flatten-on-kill drift sweep: "+reason, ts); perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		submitted = append(submitted, coid)
	}
	return submitted, firstErr
}

// hasWorkingCloseOrder reports whether a NON-TERMINAL order in the closing
// direction is already working on (strategyID, symbol) — i.e. a close this
// flatten would otherwise duplicate. It is the in-flight idempotency guard: the
// settled book (OpenPositions) still shows a position whose close has been
// submitted but not yet filled, so without this guard a repeat flatten would
// re-submit the close and over-sell the position into a short once both fill.
//
// The direction match (Side == closeSide) is deliberate: a still-working OPENING
// order in the OPPOSITE direction is not a close and must NOT suppress the
// flatten. A terminal order (FILLED/CANCELED/REJECTED) no longer holds size in
// flight, so it does not count — its effect is already in the settled book.
func (e *MoomooExecutor) hasWorkingCloseOrder(strategyID, symbol string, closeSide domain.OrderSide) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.orders {
		if st.StrategyID == strategyID && st.Symbol == symbol &&
			st.Side == closeSide && !st.IsTerminal() {
			return true
		}
	}
	return false
}

// mustPosition reads the (strategy, symbol) snapshot from the book for a
// persistence write; it returns a flat snapshot if the position is unexpectedly
// absent (so persistPosition still writes a consistent row).
func mustPosition(book AccountBook, strategyID, symbol string) domain.Position {
	if p, ok := book.Position(strategyID, symbol); ok {
		return p
	}
	return domain.Position{StrategyID: strategyID, Symbol: symbol}
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
