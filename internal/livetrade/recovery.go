package livetrade

// recovery.go implements the session-level crash recovery + flatten-on-kill
// orchestration (P6 decisions 6 + 7) on top of the executor's broker-restore +
// flatten primitives.
//
// CRASH RECOVERY (decision 6): RestoreFromBroker rebuilds the executor's
// in-flight order state from the broker, restores the broker's positions into
// the accounting book (so the strategy books resume from the authoritative
// venue truth), and returns the broker positions for the caller to reconcile.
// Combined with RestoreStrategyState (SG state from PG) and a reconciliation
// pass, the node resumes cleanly: identical subsequent behaviour + positions
// intact.
//
// FLATTEN-ON-KILL (decision 7): Flatten submits idempotent FLAT market orders
// closing ALL open broker positions, confirmation-gated + audited (the executor
// owns the primitive; this is the session-facing entry point).

import (
	"context"
	"fmt"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
)

// FlattenConfirmationPhrase re-exports the executor's flatten confirmation phrase
// so callers (the command consumer / CLI) gate on the same constant.
const FlattenConfirmationPhrase = moexec.FlattenConfirmationPhrase

// RestoreFromBroker rebuilds in-flight order state + position book from the
// broker after a restart (decision 6). It (1) asks the executor to re-track the
// broker's open orders + cumulative fill snapshot, (2) seeds the accounting book
// from the broker's positions so the strategy books resume from venue truth, and
// (3) returns the broker positions for the caller to reconcile. Idempotent.
//
// Position seeding policy: broker positions are NETTED across strategies; we
// attribute the restored aggregate to the RECOVERY pseudo-strategy so the
// accounting net (the value strategies size FLAT closes against, and the value
// reconciliation compares) matches the broker exactly. Per-strategy attribution
// is recovered from the persisted strategy SG state (RestoreStrategyState), not
// from the netted broker view.
func (t *TradeSession) RestoreFromBroker(ctx context.Context) ([]mo.BrokerPosition, error) {
	positions, attribErr := t.exec.RestoreFromBroker(ctx)
	// attribErr is a per-strategy ATTRIBUTION gap on a restored in-flight order
	// (the strategy id could not be re-keyed from the durable submit record). The
	// netted positions are still authoritative + must be seeded so the broker-vs-
	// net reconcile is correct and FLAT-close sizing works; we seed FIRST, then
	// propagate attribErr so the caller surfaces the gap (recovery fails loud
	// rather than resuming with mis-attributed in-flight orders). A hard IO error
	// from the executor (positions==nil) short-circuits before seeding.
	if positions == nil && attribErr != nil {
		return nil, attribErr
	}
	if err := t.seedPositions(ctx, positions); err != nil {
		return nil, err
	}
	return positions, attribErr
}

// recoveryStrategyID is the pseudo-strategy the netted broker positions are
// attributed to on restore (matches the executor's FLATTEN attribution domain:
// broker positions are cross-strategy aggregates).
const recoveryStrategyID = "RECOVERY"

// seedPositions opens the accounting book to match the broker positions by
// applying a synthetic opening fill per non-flat broker position (at the broker
// avg price). This is a RECONSTRUCTION, not a trade: no order is placed, nothing
// is sent to the venue. The synthetic fills carry a RECOVERY trade id so they
// are auditable + idempotent (re-running with an already-seeded book is a no-op
// because the net already matches — we only seed symbols whose accounting net
// differs from the broker).
func (t *TradeSession) seedPositions(ctx context.Context, positions []mo.BrokerPosition) error {
	_ = ctx
	for _, p := range positions {
		if p.Qty == 0 {
			continue
		}
		current := t.account.NetPositionAcrossStrategies(p.Symbol)
		delta := p.Qty - current
		if delta == 0 {
			continue // already seeded / matches the broker
		}
		side := domain.OrderSideBuy
		absQty := delta
		if delta < 0 {
			side = domain.OrderSideSell
			absQty = -delta
		}
		px := p.AvgPrice
		if px <= 0 {
			px = p.Price
		}
		if px <= 0 {
			return fmt.Errorf("livetrade: cannot seed %s: broker reported no price", p.Symbol)
		}
		f := domain.Fill{
			TradeID:       fmt.Sprintf("RECOVERY-%s", p.Symbol),
			ClientOrderID: fmt.Sprintf("RECOVERY-%s", p.Symbol),
			StrategyID:    recoveryStrategyID,
			Symbol:        p.Symbol,
			Side:          side,
			Qty:           absQty,
			Price:         px,
			TS:            t.exec.Now(),
		}
		if err := f.Validate(); err != nil {
			return fmt.Errorf("livetrade: invalid recovery seed fill for %s: %w", p.Symbol, err)
		}
		if _, err := t.account.ApplyFill(f); err != nil {
			return fmt.Errorf("livetrade: seeding %s position: %w", p.Symbol, err)
		}
	}
	return nil
}

// Flatten closes ALL open broker positions with idempotent FLAT market orders
// (decision 7). It is confirmation-gated (the executor checks the phrase) +
// audited (each close is a tracked order). Returns the submitted client-order-ids.
func (t *TradeSession) Flatten(ctx context.Context, confirmation, reason string) ([]string, error) {
	return t.exec.Flatten(ctx, confirmation, reason)
}
