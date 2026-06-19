// Package unified holds the cross-cutting UNIFICATION PROOF: the executable
// evidence that all five execution modes (backtest, hyperopt, live-signal,
// paper, live) run on the SAME engine assembly — the identical strategy /
// portfolio / context set produced by internal/engine/strategyassembly — and
// differ ONLY in the two injected seams: the Clock and the Executor.
//
// It has no production code; it is a test-only package so it can import the
// batch consumer (internal/engine), the streaming consumer
// (internal/livengine), and the shared assembler (strategyassembly) without
// creating an import cycle in any of them. The companion narrative lives in
// docs/reference/architecture.md (the "five modes, one engine" table); this package keeps
// that document HONEST by failing the build if the shared-assembly invariant
// ever regresses.
package unified
