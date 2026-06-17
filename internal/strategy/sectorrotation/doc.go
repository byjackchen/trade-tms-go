// Package sectorrotation is the SectorRotation SignalGenerator: a multi-symbol,
// rebalance-driven momentum strategy:
//
//   - Universe: N sector ETFs (default 11 Select Sector SPDRs).
//   - Trigger:  first bar of a NEW calendar month (vs the most-recent bar seen
//     across the WHOLE universe). One rebalance per month.
//   - Logic:    rank the universe by lookback-bar return; hold equal-weight
//     positions in the top-K. On rebalance emit FLAT for any current holding
//     that dropped out of the top-K and LONG for any new top-K member not yet
//     held; symbols already correctly positioned produce no signal (no churn).
//   - Sizing:   target_value = equity()/top_k ; shares = floor(target_value/price)
//     where price is the symbol's last close.
//
// Look-ahead guard: the rebalance fires BEFORE the new-month bar is ingested,
// so every symbol contributes its prior-month-end close — the symbol that
// triggered the rollover does NOT yet have today's close folded in.
//
// Numerical semantics:
//   - Per-symbol close history is held as a bounded deque of maxlen
//     lookback+1 (deque(maxlen=...)).
//   - The lookback return is float((new-old)/old), computed by dividing the raw
//     1e-4 fixed-point integer units (float64(newRaw-oldRaw)/float64(oldRaw)).
//   - Sizing uses float64 throughout: equity()/top_k then floor(value/price).
//
// Layer (pure strategy): bars in, signals out. The package depends ONLY on the
// domain layer (Bar/Signal/Price/Qty/intent types) — never on the execution
// engine — preserving the two-layer contract (the core strategy package forbids
// engine imports). The engine bridge (Signal -> market order, FLAT ->
// net-position close, capability publishing) lives in
// internal/strategy/sectoradapter, the sole importer of internal/engine.
//
// May import: internal/domain and the stdlib only.
package sectorrotation
