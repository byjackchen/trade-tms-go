// Package exec is the simulated execution venue used by backtests. It provides
// SimExecutor — a fill simulator that accepts order submissions during bar
// processing and produces fills via a configured FillModel (slippage,
// commission, next-bar fill timing identical to the Python reference) — along
// with the small ports it depends on: FillModel (fill legs + timing), FillSink
// (where produced fills are delivered, forwarded by the engine into
// accounting) and SeqSource (monotonic deterministic sequence values for id
// generation). Mirrors the fill-simulation responsibilities embedded in
// src/runner/ and src/adapters of the reference repo.
//
// SimExecutor sits *below* the engine: it is the simulated venue the batch
// engine drives bar-by-bar (ProcessBar/FlushThisBar/FillAtBar). It is NOT the
// strategy-facing execution seam and is not a peer of the live/paper venue
// clients. The strategy-facing order-submission contract is owned by the
// engine: strategies submit through engine.OrderSubmitter and read their net
// position through engine.PositionReader. Different deployment modes plug
// different implementations behind those engine-owned interfaces (the batch
// engine's internal submitter routing into SimExecutor, livengine.NoopExecutor,
// livetrade.GatedSubmitter, moomoo.MoomooExecutor); exec itself only supplies
// the simulated venue and its fill primitives.
//
// Rules:
//   - Fills are deterministic: the same bars + orders produce the same fills,
//     matching the Python reference byte-for-byte.
//   - Live order paths (implemented elsewhere, behind engine.OrderSubmitter)
//     must be idempotent and audit-logged.
package exec
