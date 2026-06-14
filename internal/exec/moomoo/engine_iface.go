package moomoo

// engine_iface.go pins the contract that MoomooExecutor IS a drop-in for the
// signal-mode NoopExecutor at the engine seam: it implements
// engine.OrderSubmitter (SubmitMarket / SubmitMarketSignal) and
// engine.PositionReader (NetPosition), so the SAME strategy / session code that
// runs in signal mode runs unmodified in paper/live — just with a real venue
// behind submission and a real broker behind NetPosition.

import "github.com/byjackchen/trade-tms-go/internal/engine"

var (
	_ engine.OrderSubmitter = (*MoomooExecutor)(nil)
	_ engine.PositionReader = (*MoomooExecutor)(nil)
)
