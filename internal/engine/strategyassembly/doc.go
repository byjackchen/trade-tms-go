// Package strategyassembly is the Go port of the Python multi-strategy wiring
// (scripts/multi_strategy_backtest.py + src/runner/strategy_assembly.py): it
// constructs the real strategy adapters (SEPA / SectorRotation / Pairs / ORB)
// from resolved params, the Allocator capital split + RiskConstraints portfolio
// gate, and the per-bar look-ahead-safe context provider, returning everything
// the engine assembler needs as a single Assembly.
//
// It is the ONLY package that imports both the engine seam and every strategy
// adapter, so the per-strategy adapter packages stay decoupled from each other.
// No cycle: adapters import engine; this package imports adapters + engine +
// params + portfolio; engine imports portfolio (not this).
//
// Equity-provider late binding: the strategy SignalGenerators need an
// EquityProvider closure that reads the LIVE account equity, but the account is
// created inside engine.New AFTER the strategies are constructed. We resolve the
// ordering with a LiveEquity holder: generators are built over holder.Get
// (which returns the starting balance until bound), then the caller binds the
// holder to the engine's account via Assembly.BindEquity(eng) before Run. This
// mirrors the Python equity_provider that pulls engine.portfolio.account(VENUE)
// .balance_total at sizing time.
//
// Layer: wiring/composition (above the adapters, below runner). It is the single
// fan-in point for every strategy adapter; the live and backtest paths both go
// through it so they assemble identical strategy state.
//
// May import: internal/engine, internal/params, internal/riskgate, the pure
// strategy packages (internal/strategy/orb, .../pairs, .../sepa,
// .../sectorrotation) and their adapters (.../orbadapter, .../pairsadapter,
// .../sepaadapter, .../sectoradapter), plus the standard library. It must NOT
// import runner, livengine or publish.
package strategyassembly
