package livetrade

// gate.go is the PRE-SUBMIT portfolio gate for paper/live execution (P6 locked
// decision 4). GatedSubmitter implements engine.OrderSubmitter and engine.
// PositionReader so the SAME strategy adapters that run in backtest/signal mode
// run unmodified — but here every OPENING order is checked against the portfolio
// (allocator budget + aggregate risk constraints) AND the daily-loss-halt rule
// BEFORE it reaches the MoomooExecutor's PlaceOrder. A rejection records a
// live.risk_events row + audit and the order is suppressed (never sent to the
// venue). FLAT / closing orders bypass the budget AND the daily-loss halt —
// closes always proceed, even during a halt, per docs/spec/portfolio-risk.md.
//
// This mirrors the backtest engine's orderSubmitter.SubmitMarketSignal gate
// (internal/engine/engine.go) so live gating is byte-identical to backtest
// gating, plus the live-only daily-loss-halt latch (the live node sets the halt
// state when day P&L < -10% NAV, after which NEW opening orders are rejected
// while existing positions stay open + FLAT remains allowed).

import (
	"context"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// Halter is the live node's halt latch the gate consults + sets. It is satisfied
// by *commands.HaltState (IsHalted / Halt) — kept as a narrow interface so the
// gate is testable without the command plane. A halt of kind daily_loss is
// latched by the gate when the health snapshot crosses the threshold; a manual
// halt is set by an operator. Either way, while halted the gate rejects NEW
// opening orders (FLAT still passes).
type Halter interface {
	// IsHalted reports whether trading is halted (NEW opens are suppressed).
	IsHalted() bool
	// HaltDailyLoss latches a daily-loss halt with the reason (idempotent — a
	// re-halt keeps the first trigger). Distinct method so the gate can set the
	// correct halt kind without depending on the commands.HaltKind type.
	HaltDailyLoss(reason string)
}

// RiskRecorder persists a gate decision to live.risk_events + audit. The gate
// records EVERY rejection (and may record approvals for a full audit trail). May
// be nil (tests / Redis-less); a nil recorder skips persistence but the gate
// still suppresses the order.
type RiskRecorder interface {
	RecordGateDecision(ctx context.Context, d GateDecision) error
}

// GateDecision is one recorded pre-submit gate outcome (-> live.risk_events).
type GateDecision struct {
	Approved   bool
	RuleName   string
	Reason     string
	StrategyID string
	Symbol     string
	Side       domain.SignalSide
	Qty        domain.Qty
	Price      domain.Price
	TS         time.Time
}

// GatedSubmitter runs the portfolio gate ahead of the MoomooExecutor.
type GatedSubmitter struct {
	exec    *moexec.MoomooExecutor
	gate    *portfolio.Portfolio
	account *AccountAdapter
	halt    Halter
	risk    RiskRecorder

	// haltPct is the daily-loss-halt fraction of NAV (e.g. 0.10). Read from the
	// gate's RiskConstraints config so the latch matches the rule exactly.
	haltPct float64
	// nav is the informational NAV used for the daily-loss-halt headroom check
	// (the live account's starting balance / buying power baseline).
	nav domain.Money

	// telemetry
	submitted int64
	rejected  int64
	flatPass  int64
}

// GatedSubmitterConfig assembles a GatedSubmitter.
type GatedSubmitterConfig struct {
	// Executor is the paper/live order executor (required).
	Executor *moexec.MoomooExecutor
	// Gate is the portfolio gating pipeline (allocator + risk). May be nil (no
	// gate => every order submits; not used in production, only ungated tests).
	Gate *portfolio.Portfolio
	// Account is the accounting adapter (required: the gate reads the book + the
	// executor settles into it).
	Account *AccountAdapter
	// Halt is the halt latch (required for daily-loss-halt enforcement).
	Halt Halter
	// Risk records gate decisions (may be nil).
	Risk RiskRecorder
	// NAV is the daily-loss-halt NAV baseline (the live account starting balance).
	NAV domain.Money
}

// NewGatedSubmitter builds a GatedSubmitter. It reads the daily-loss-halt pct
// from the gate's RiskConstraints (default 0.10 when no gate is configured).
func NewGatedSubmitter(cfg GatedSubmitterConfig) *GatedSubmitter {
	haltPct := 0.10
	if cfg.Gate != nil {
		haltPct = cfg.Gate.RiskConstraints().Config().DailyLossHaltPct
	}
	return &GatedSubmitter{
		exec:    cfg.Executor,
		gate:    cfg.Gate,
		account: cfg.Account,
		halt:    cfg.Halt,
		risk:    cfg.Risk,
		haltPct: haltPct,
		nav:     cfg.NAV,
	}
}

// SubmitMarket is the UNGATED primitive (engine.OrderSubmitter). It is used for
// already-decided FLAT/close orders that must always proceed — it delegates
// straight to the executor without the budget/halt check. The strategy adapters
// route FLAT closes here (via the engine seam) so a close is never blocked.
func (g *GatedSubmitter) SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error) {
	g.flatPass++
	return g.exec.SubmitMarket(strategyID, symbol, side, qty, reason, ts)
}

// SubmitMarketSignal runs the pre-submit gate then submits (engine.OrderSubmitter).
//
//   - FLAT or qty<=0: a close/no-op. Bypass the budget AND the daily-loss halt
//     (closes always proceed). Submit unconditionally.
//   - daily-loss halt active (or latched now): reject the NEW opening order. The
//     existing positions stay open; FLAT (above) is unaffected.
//   - otherwise: run portfolio.Check (allocator budget + risk constraints). The
//     FIRST rejection wins; a rejection records a risk event + audit and returns
//     submitted=false WITHOUT placing an order.
func (g *GatedSubmitter) SubmitMarketSignal(strategyID, symbol string, signalSide domain.SignalSide, orderSide domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	ctx := context.Background()

	// FLAT / non-positive qty: a close. Always proceed (bypasses budget + halt).
	if signalSide == domain.SideFlat || qty <= 0 {
		g.flatPass++
		coid, submitted, err := g.exec.SubmitMarketSignal(strategyID, symbol, signalSide, orderSide, qty, reason, ts)
		if submitted {
			g.submitted++
		}
		return coid, submitted, err
	}

	price, _ := g.account.LastPrice(symbol)

	// Daily-loss halt: when the day P&L crosses -haltPct*NAV the live node latches
	// a halt and rejects NEW opens. Re-check the latch here so an open submitted in
	// the same dispatch as the breach is also suppressed.
	if g.dailyLossHalted() {
		g.recordRejection(ctx, "risk.daily_loss_halt",
			"daily loss halt active: new opening orders suppressed (existing positions stay open, FLAT still allowed)",
			strategyID, symbol, signalSide, qty, price, ts)
		return "", false, nil
	}

	// Portfolio gate (allocator budget + aggregate risk constraints), via the
	// SHARED portfolio.GateSignal wrapper (E1): identical Check + rejection-record
	// flow as the backtest engine. The live-only daily-loss halt above is the only
	// extra pre-check; the sink here persists a live.risk_events row.
	if g.gate != nil {
		snap, err := g.account.Snapshot()
		if err != nil {
			return "", false, err
		}
		proposed := portfolio.NewProposedOrder(strategyID, symbol, signalSide, qty, price, ts)
		decision := portfolio.GateSignal(g.gate, proposed, portfolio.SnapshotFromDomain(snap), price,
			gateRejectionSink{g: g, ctx: ctx})
		if !decision.Approved {
			return "", false, nil
		}
	}

	// Approved: submit through the executor (idempotent on its client-order-id).
	coid, submitted, err := g.exec.SubmitMarketSignal(strategyID, symbol, signalSide, orderSide, qty, reason, ts)
	if err != nil {
		return coid, false, err
	}
	if submitted {
		g.submitted++
	}
	return coid, submitted, nil
}

// NetPosition reads the strategy's net position from the broker-settled account
// book (engine.PositionReader), netting across strategies — the same venue-net
// convention strategies size FLAT closes against in backtest.
func (g *GatedSubmitter) NetPosition(_ string, symbol string) domain.Qty {
	return g.account.NetPositionAcrossStrategies(symbol)
}

// EvaluateDailyLossHalt checks the current health snapshot against the daily-loss
// threshold and latches a halt when breached (day P&L < -haltPct*NAV, strict <,
// matching the risk spec). It is called by the trade session after each timestamp
// (the gate reads the up-to-date account book). Returns true when it latched a
// halt this call (so the caller can persist a tms.halts row). Idempotent: once
// halted, further calls return false (the latch keeps the first trigger).
func (g *GatedSubmitter) EvaluateDailyLossHalt() (latched bool, headroomBreached bool) {
	if g.halt == nil || g.halt.IsHalted() {
		return false, g.dailyLossThresholdBreached()
	}
	if g.dailyLossThresholdBreached() {
		g.halt.HaltDailyLoss("day P&L below -" + pctString(g.haltPct) + " of NAV")
		return true, true
	}
	return false, false
}

// dailyLossHalted reports whether NEW opens are currently suppressed: either an
// active halt latch OR a fresh threshold breach evaluated against the live book.
func (g *GatedSubmitter) dailyLossHalted() bool {
	if g.halt != nil && g.halt.IsHalted() {
		return true
	}
	return g.dailyLossThresholdBreached()
}

// dailyLossThresholdBreached evaluates day P&L < -haltPct*NAV (strict) from the
// account snapshot MARKED TO MARKET (the live daily-loss input; the parity
// snapshot keeps day P&L 0 for backtest parity, so the marked snapshot is what
// makes the live halt fire on a held loss).
func (g *GatedSubmitter) dailyLossThresholdBreached() bool {
	if g.gate == nil {
		return false
	}
	snap, err := g.account.MarkedSnapshot(g.nav)
	if err != nil {
		return false
	}
	health := g.gate.HealthSnapshot(portfolio.SnapshotFromDomain(snap))
	return health.IsDailyLossHalt()
}

// gateRejectionSink is the GatedSubmitter's portfolio.RejectionRecorder: it
// routes a portfolio-gate rejection through the existing recordRejection path
// (live.risk_events row + audit + rejected counter), preserving the Price the
// risk-events row keeps. ctx is the per-submit context captured at call time.
type gateRejectionSink struct {
	g   *GatedSubmitter
	ctx context.Context
}

func (s gateRejectionSink) RecordRejection(r portfolio.Rejection) {
	s.g.recordRejection(s.ctx, r.RuleName, r.Reason,
		r.StrategyID, r.Symbol, r.Side, r.Qty, r.Price, r.TS)
}

// recordRejection persists a rejected gate decision + bumps the rejected counter.
func (g *GatedSubmitter) recordRejection(ctx context.Context, rule, reason, strategyID, symbol string, side domain.SignalSide, qty domain.Qty, price domain.Price, ts time.Time) {
	g.rejected++
	if g.risk == nil {
		return
	}
	_ = g.risk.RecordGateDecision(ctx, GateDecision{
		Approved:   false,
		RuleName:   rule,
		Reason:     reason,
		StrategyID: strategyID,
		Symbol:     symbol,
		Side:       side,
		Qty:        qty,
		Price:      price,
		TS:         ts,
	})
}

// SubmittedCount / RejectedCount / FlatPassCount are gate telemetry for the cockpit.
func (g *GatedSubmitter) SubmittedCount() int64 { return g.submitted }
func (g *GatedSubmitter) RejectedCount() int64  { return g.rejected }
func (g *GatedSubmitter) FlatPassCount() int64  { return g.flatPass }
