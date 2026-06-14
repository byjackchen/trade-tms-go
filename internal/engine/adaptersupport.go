package engine

// adaptersupport.go provides the shared FLAT-close helpers the four strategy
// adapters (orb/sepa/sector/pairs) previously hand-copied byte-for-byte
// (modularization-review.md §E4): a position read through the OrderSubmitter's
// optional PositionReader, and the venue-net FLAT-close sizing + submit. The
// adapters keep their strategy-specific signal translation and per-adapter reason
// strings; only the position-read + close-sizing boilerplate is centralized, so a
// change to the venue-net convention or the close path is made in one place.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// NetPositionVia reads strategyID's live net position in symbol if the submitter
// also implements PositionReader; otherwise it reports flat (0). The FLAT path
// then no-ops on 0, matching a book the engine has already flattened (e.g. the
// signal-mode NoopExecutor, which always reports flat). This is the exact
// per-adapter netPosition helper, now shared.
func NetPositionVia(sub OrderSubmitter, strategyID, symbol string) domain.Qty {
	if pr, ok := sub.(PositionReader); ok {
		return pr.NetPosition(strategyID, symbol)
	}
	return 0
}

// CloseToFlat sizes and submits the venue-net FLAT-closing market order for
// strategyID in symbol: it reads the net (via NetPositionVia), derives the
// closing side (no order when already flat), submits the abs-qty close through
// the UNGATED SubmitMarket primitive (closes always proceed), and reports whether
// an order was placed. reasonFor receives the signed net so each adapter can
// build its own reason string (the reference reason strings embed the closed
// quantity). It returns any submit error.
//
// This is the byte-identical FLAT-close path the four adapters shared; the only
// per-adapter variation (the reason string) is supplied by the closure, so the
// sizing/submit semantics stay single-sourced.
func CloseToFlat(sub OrderSubmitter, strategyID, symbol string, ts time.Time, reasonFor func(net domain.Qty) string) (placed bool, err error) {
	net := NetPositionVia(sub, strategyID, symbol)
	side, ok := domain.CloseSideFor(net)
	if !ok {
		return false, nil // already flat: no order
	}
	qty := net
	if qty < 0 {
		qty = -qty
	}
	if _, err := sub.SubmitMarket(strategyID, symbol, side, qty, reasonFor(net), ts); err != nil {
		return false, err
	}
	return true, nil
}
