package moomoo

// executor_sync.go is DIRECTION 2 — the broker -> TMS SYNC/REFLECT primitive.
// The operator trades DIRECTLY in the moomoo app (no TMS order placed); they then
// return to TMS and ask the desk to pull the account's ACTUAL state and REFLECT it
// into the TMS books so the externally-placed trades show up. This file holds the
// execution primitive (SyncBrokerInto); the operator-facing SyncFromBroker (audit
// + reconciliation) lives in the manualtrade controller.
//
// SAFETY (paramount): this is READ-ONLY from the broker. It calls ONLY the GET
// methods (Trd_GetPositionList / Trd_GetOrderList / Trd_GetOrderFillList /
// Trd_GetFunds) — it NEVER calls PlaceOrder, so a sync can NOT place a real order
// and is safe in ALL modes, including signal mode. The reflection settles synthetic
// fills into the LOCAL accounting book ONLY; nothing crosses the wire to the venue.
//
// IDEMPOTENCY: the externally-observed broker net per symbol is reconciled against
// the book net under the supplied (MANUAL/EXTERNAL) strategy id by settling a fill
// for the DELTA (broker net - book net). A re-sync of the SAME broker state yields a
// zero delta for every symbol -> no synthetic fill -> no duplicated rows. The
// synthetic fill's trade-id is derived deterministically from (strategy, symbol,
// broker-observed-net) so a re-emit of the same reflection is a durable no-op
// (InsertFill is keyed on the trade-id).

import (
	"context"
	"fmt"
	"sort"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// BrokerSnapshot is the account's ACTUAL state pulled READ-ONLY from the broker
// (the four Trd_Get* reads), normalised. It is what the SYNC reflects into TMS.
type BrokerSnapshot struct {
	Positions []mo.BrokerPosition
	Orders    []mo.OrderUpdate
	Fills     []mo.FillUpdate
	Funds     mo.Funds
}

// BrokerSyncResult reports what a reflection observed + changed (for the sync
// report the controller returns + audits).
type BrokerSyncResult struct {
	// Snapshot is the raw broker state that was pulled (read-only).
	Snapshot BrokerSnapshot
	// PositionsObserved is the count of non-flat broker positions seen.
	PositionsObserved int
	// OrdersObserved / FillsObserved are the broker order / fill row counts.
	OrdersObserved int
	FillsObserved  int
	// Reflected is the count of symbols whose MANUAL/EXTERNAL book net was moved to
	// match the broker (a non-zero delta was settled). Zero on a clean re-sync.
	Reflected int
}

// PullBroker pulls the account's actual state READ-ONLY (no order is placed). It
// is the first half of a sync; SyncBrokerInto reflects the result. Surfacing it
// separately lets the controller render the raw counts even if reflection is a
// no-op.
func (e *MoomooExecutor) PullBroker(ctx context.Context) (BrokerSnapshot, error) {
	positions, err := e.cfg.Client.GetPositionList(ctx, e.accID, e.env)
	if err != nil {
		return BrokerSnapshot{}, fmt.Errorf("sync: GetPositionList: %w", err)
	}
	orders, err := e.cfg.Client.GetOrderList(ctx, e.accID, e.env)
	if err != nil {
		return BrokerSnapshot{}, fmt.Errorf("sync: GetOrderList: %w", err)
	}
	// Fills: moomoo SIMULATE (paper) accounts do NOT expose deal data — OpenD
	// rejects Trd_GetOrderFillList with "Paper trading does not support deal
	// data." The fills are informational only (the reflection settles from
	// POSITIONS, not fills — see SyncBrokerInto/reflectSymbol), so for a paper
	// account we skip the fills pull and still sync positions + orders + funds.
	// Real accounts pull fills as before.
	var fills []mo.FillUpdate
	if e.env == mo.TrdEnvReal {
		fills, err = e.cfg.Client.GetOrderFillList(ctx, e.accID, e.env)
		if err != nil {
			return BrokerSnapshot{}, fmt.Errorf("sync: GetOrderFillList: %w", err)
		}
	}
	funds, err := e.cfg.Client.GetFunds(ctx, e.accID, e.env)
	if err != nil {
		return BrokerSnapshot{}, fmt.Errorf("sync: GetFunds: %w", err)
	}
	return BrokerSnapshot{Positions: positions, Orders: orders, Fills: fills, Funds: funds}, nil
}

// SyncBrokerInto pulls the broker's actual state (PullBroker) and REFLECTS its
// positions into the LOCAL accounting book under strategyID (the MANUAL/EXTERNAL
// book). For each broker symbol it settles a synthetic fill for the DELTA between
// the broker net and the current book net, so the book ends equal to the broker
// truth. It is READ-ONLY at the broker (no PlaceOrder) and idempotent: a re-sync of
// the same broker state settles nothing (zero delta) and re-emits no durable row.
//
// strategyID MUST be the MANUAL/EXTERNAL pseudo-strategy so externally-placed
// trades reflect WITHOUT corrupting the auto-strategy books. The synthetic order +
// fill + position are persisted (live.orders/fills/positions) under that id so the
// cockpit surfaces them.
func (e *MoomooExecutor) SyncBrokerInto(ctx context.Context, strategyID string) (BrokerSyncResult, error) {
	if strategyID == "" {
		return BrokerSyncResult{}, fmt.Errorf("%w: sync requires a strategy id (the MANUAL/EXTERNAL book)", domain.ErrInvalidArgument)
	}
	snap, err := e.PullBroker(ctx)
	if err != nil {
		return BrokerSyncResult{}, err
	}

	res := BrokerSyncResult{
		Snapshot:       snap,
		OrdersObserved: len(snap.Orders),
		FillsObserved:  len(snap.Fills),
	}

	// Net the broker positions per symbol (the broker reports one row per symbol,
	// but be defensive about duplicates). A flat broker row is skipped.
	brokerNet := make(map[string]mo.BrokerPosition, len(snap.Positions))
	for _, p := range snap.Positions {
		if p.Qty == 0 {
			continue
		}
		res.PositionsObserved++
		cur, ok := brokerNet[p.Symbol]
		if !ok {
			brokerNet[p.Symbol] = p
			continue
		}
		cur.Qty += p.Qty
		brokerNet[p.Symbol] = cur
	}

	// Reflect each observed symbol AND each symbol the MANUAL book currently holds
	// but the broker no longer reports (a position the operator CLOSED in moomoo —
	// the broker net is 0, so the book must be driven to 0 too). Union the key sets.
	symbols := make(map[string]struct{}, len(brokerNet))
	for sym := range brokerNet {
		symbols[sym] = struct{}{}
	}
	for _, pos := range e.cfg.Book.OpenPositions() {
		if pos.StrategyID == strategyID && pos.SignedQty != 0 {
			symbols[pos.Symbol] = struct{}{}
		}
	}
	// Deterministic order so the synthetic trade-ids + persistence are stable.
	ordered := make([]string, 0, len(symbols))
	for sym := range symbols {
		ordered = append(ordered, sym)
	}
	sort.Strings(ordered)

	for _, sym := range ordered {
		bp := brokerNet[sym] // zero-value (Qty 0) when the broker no longer holds it
		if reflected, rerr := e.reflectSymbol(ctx, strategyID, sym, bp); rerr != nil {
			return res, rerr
		} else if reflected {
			res.Reflected++
		}
	}
	return res, nil
}

// reflectSymbol drives the book net for (strategyID, symbol) to the broker net by
// settling a synthetic fill for the delta. Returns reflected=true when a non-zero
// delta was applied. The fill's trade-id is deterministic on the TARGET broker net
// so re-reflecting the same state is a durable no-op.
func (e *MoomooExecutor) reflectSymbol(ctx context.Context, strategyID, symbol string, bp mo.BrokerPosition) (bool, error) {
	bookNet := domain.Qty(0)
	if pos, ok := e.cfg.Book.Position(strategyID, symbol); ok {
		bookNet = pos.SignedQty
	}
	delta := bp.Qty - bookNet
	if delta == 0 {
		return false, nil // already reflected: idempotent no-op
	}

	side := domain.OrderSideBuy
	abs := delta
	if delta < 0 {
		side = domain.OrderSideSell
		abs = -delta
	}

	// Price for the synthetic fill: the broker's average cost (the externally-paid
	// basis) when known, else the last market price, else a $1 floor so the fill
	// validates (a positive price is required; the qty/side carry the position truth
	// regardless of the exact basis, which reconciliation does not depend on).
	price := bp.AvgPrice
	if price <= 0 {
		price = bp.Price
	}
	if price <= 0 {
		price = domain.Price(domain.FixedScale) // $1.00 floor
	}

	ts := e.clock.Now()
	// Deterministic ids on the TARGET broker net so a re-sync of the SAME state
	// produces the SAME ids — and since the delta is then 0, no second fill is even
	// attempted. The id still encodes the net so a DIFFERENT later state (operator
	// traded more in moomoo) yields a distinct fill row.
	coid := fmt.Sprintf("SYNC-%s-%s-%s-%d", envTag(e.env), strategyID, symbol, int64(bp.Qty))
	tradeID := coid + "-F"

	fill := domain.Fill{
		TradeID:       tradeID,
		ClientOrderID: coid,
		StrategyID:    strategyID,
		Symbol:        symbol,
		Side:          side,
		Qty:           abs,
		Price:         price,
		Commission:    domain.Money(0),
		TS:            ts,
	}
	if err := fill.Validate(); err != nil {
		return false, fmt.Errorf("sync: synthetic reflect fill %s: %w", tradeID, err)
	}

	// Persist a synthetic order row (so the cockpit's order list shows the sync
	// origin) then settle the fill into the local book + persist the fill + the
	// resulting position. NOTHING crosses the wire to the broker.
	e.persistOrder(ctx, domain.Order{
		ClientOrderID: coid,
		StrategyID:    strategyID,
		Symbol:        symbol,
		Side:          side,
		Type:          domain.OrderTypeMarket,
		TIF:           domain.TIFGTC,
		Qty:           abs,
		Status:        domain.OrderStatusFilled,
		FilledQty:     abs,
		AvgFillPx:     price,
		Reason:        "broker-sync reflect (external)",
		TS:            ts,
	})

	pos, err := e.cfg.Book.ApplyFill(fill)
	if err != nil {
		return false, fmt.Errorf("sync: settle reflect fill %s: %w", tradeID, err)
	}
	e.persistFill(ctx, fill)
	e.persistPosition(ctx, pos)
	return true, nil
}

// envTag renders the env as a short tag for the synthetic sync ids.
func envTag(env mo.TrdEnv) string {
	if env == mo.TrdEnvReal {
		return "LIVE"
	}
	return "PAPER"
}
