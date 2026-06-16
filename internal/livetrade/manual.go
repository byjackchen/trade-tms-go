package livetrade

// manual.go is the MANUAL (operator-driven / discretionary) trading desk: it lets
// an operator place, cancel and close orders BY HAND against a paper or live
// account, independent of the strategy execution mode. In signal mode the operator
// IS the executor (strategies only signal); in paper/live mode manual orders are an
// override/intervention alongside the strategies' auto-trading.
//
// It holds a Trd_* trade session (a live-bound or paper-bound MoomooExecutor +
// AccountAdapter) and runs every manual action through the SAME order state machine
// + persistence (live.orders/fills/positions) + the mock venue for tests. Manual
// orders are attributed to a MANUAL pseudo-strategy id, distinct from the auto
// strategies' books, so reconciliation + per-strategy accounting stay clean.
//
// SAFETY (paramount — this can place real orders):
//
//   - A live (real-money) manual order requires the FULL 4-factor live activation
//     (real acc id + TMS_LIVE_CONFIRM phrase + UnlockTrade + the TMS-LIVE-REAL-001
//     trader id) — which is ALREADY proven by the fact the executor is live-bound
//     (MoomooExecutor only binds TrdEnvReal via New(ModeLive), which enforced all
//     four) — PLUS a per-order typed confirmation phrase supplied on the request.
//     A paper manual order requires the trade password. There is NO code path that
//     reaches a real order without the full gate: the controller refuses a live
//     order missing the per-order confirm, and the executor's assertEnvInvariants
//     tripwires every submission.
//
//   - Every action (place / cancel / close) writes an ops.audit_log row (operator,
//     symbol, side, qty, override?, ts) via the AuditSink.
//
//   - Idempotent client-order-ids: a manual order's coid is derived
//     deterministically from the request's idempotency key, so a retried submit
//     never double-submits at the broker.
//
//   - RISK GATE: an OPENING manual order runs Portfolio.check (allocator budget +
//     concentration + daily-loss-halt). A violation REJECTS the order unless the
//     request carries an explicit audited override flag (the operator's decision),
//     which is recorded in risk_events + audit. FLAT / closing manual orders bypass
//     the budget (same as auto), matching the gated submitter.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// ManualStrategyID is the pseudo-strategy every manual order is attributed to,
// distinct from the auto strategies' books so reconciliation + per-strategy
// accounting stay clean. It is the strategy id on the live.orders / live.fills /
// live.positions rows a manual action produces.
const ManualStrategyID = "MANUAL"

// ManualLiveConfirmationPhrase is the EXACT per-order typed phrase a live
// (real-money) manual order requires, ON TOP of the 4-factor live activation the
// executor already enforced. It is intentionally verbose + unambiguous so it can
// never be supplied by accident or a generic default.
const ManualLiveConfirmationPhrase = "I CONFIRM THIS REAL MONEY MANUAL ORDER"

// Sentinel errors the controller returns so callers (the API) can map to precise
// status codes. A paper desk with no configured trade password refuses ALL paper
// orders (ErrTradePasswordRequired) — a paper desk must still gate, so an operator
// with no password set cannot place even paper orders by accident.
var (
	// ErrConfirmationRequired is returned when a LIVE manual order is missing (or
	// supplies a wrong) per-order confirmation phrase. The API maps it to 412.
	ErrConfirmationRequired = errors.New("manual trade: per-order live confirmation phrase required")
	// ErrTradePasswordRequired is returned when a PAPER manual order is missing (or
	// supplies a wrong) trade password. The API maps it to 412.
	ErrTradePasswordRequired = errors.New("manual trade: paper trade password required")
	// ErrRiskViolation is returned when the risk gate rejects an opening order and
	// no override was supplied. The API maps it to 422. The wrapped string carries
	// the violated rule + reason.
	ErrRiskViolation = errors.New("manual trade: risk gate violation (supply override to proceed)")
)

// AuditSink records a manual action to ops.audit_log (operator, symbol, side, qty,
// override?, ts). It is REQUIRED in production (every manual action MUST audit); a
// nil sink is allowed only in tests and drops the row. Satisfied by the runner's
// LivePersist manual-audit adapter.
type AuditSink interface {
	RecordManualAction(ctx context.Context, a ManualAuditRecord) error
}

// PriceSource resolves a symbol's last/reference price independently of the manual
// book's own fill-observed prices. It is the FIX for the inert risk gate (finding
// 3): the manual desk's AccountAdapter.LastPrice only knows prices it has seen on
// FILLS, and nothing wires the live node's bar feed into the manual account — so a
// discretionary order on a symbol the operator has never filled prices at 0, the
// budget/concentration rules see 0 notional, and the gate is a no-op. A PriceSource
// (the broker/market-data feed: Qot_RequestHistoryKL's latest daily close in the
// runner) supplies a real price so the gate actually binds. ok=false leaves the
// gate to fall back to the fill-observed LastPrice (which may still be 0).
type PriceSource interface {
	// LastPrice returns symbol's reference price; ok=false when it cannot be resolved.
	LastPrice(ctx context.Context, symbol string) (domain.Price, bool)
}

// ManualAuditRecord is one audited manual action (-> ops.audit_log).
type ManualAuditRecord struct {
	Operator      string
	Action        string // "place" | "cancel" | "close"
	Symbol        string
	Side          string
	Qty           int64
	OrderType     string
	ClientOrderID string
	Override      bool
	RiskRule      string // populated when an override bypassed a risk violation
	Live          bool
	TS            time.Time
}

// ManualController is the operator-driven trading desk over one paper/live
// account. It is safe for concurrent use (the API may serve several operators).
type ManualController struct {
	acct       domain.Account
	exec       *moexec.MoomooExecutor
	gate       *portfolio.Portfolio
	account    *AccountAdapter
	prices     PriceSource
	halt       Halter
	risk       RiskRecorder
	audit      AuditSink
	reconciler *Reconciler

	nav           domain.Money
	paperPassword string
	clock         func() time.Time

	mu sync.Mutex
}

// ManualControllerConfig assembles a ManualController.
type ManualControllerConfig struct {
	// Acct is the bound broker account (simulate => paper, real => live). It MUST
	// match the executor's binding (the constructor enforces: real account <=>
	// live-bound executor) so there is no way to drive a live executor through a
	// "paper" controller or vice versa.
	Acct domain.Account
	// Executor is the paper/live-bound MoomooExecutor (required). Its live binding
	// already proves the 4-factor activation for live.
	Executor *moexec.MoomooExecutor
	// Gate is the portfolio risk gate (allocator budget + concentration + daily-loss
	// halt). Required in production; nil disables the gate (tests only).
	Gate *portfolio.Portfolio
	// Account is the accounting adapter the executor settles into + the gate reads
	// (required).
	Account *AccountAdapter
	// Prices resolves a symbol's reference price for the risk gate when the manual
	// book has no fill-observed price (the runner supplies a broker/market-data
	// lookup). Without it, a discretionary order on a never-filled symbol prices at 0
	// and the budget/concentration gate is inert (finding 3). May be nil (then the
	// gate falls back to the fill-observed LastPrice, which may be 0).
	Prices PriceSource
	// Halt is the live node's halt latch (required: a manual OPEN is suppressed
	// while halted, FLAT/close still allowed — same as the strategy gate).
	Halt Halter
	// Risk records gate decisions + override events to live.risk_events (may be nil).
	Risk RiskRecorder
	// Audit records every manual action to ops.audit_log (required in production;
	// nil drops the row, tests only).
	Audit AuditSink
	// Reconciler runs the P6 reconciliation (broker vs strategy books ->
	// reconciliation_reports) at the end of a SyncFromBroker so drift introduced by
	// externally-placed moomoo trades is reported. May be nil (then SyncFromBroker
	// reflects + audits but skips the reconciliation step + returns no report).
	Reconciler *Reconciler
	// NAV is the daily-loss-halt NAV baseline (the account's opening balance).
	NAV domain.Money
	// PaperTradePassword gates PAPER manual orders. Empty => paper orders are
	// refused (a paper desk must still require the trade password).
	PaperTradePassword string
	// Clock supplies submission timestamps; nil => time.Now (UTC).
	Clock func() time.Time
}

// NewManualController builds the desk, enforcing the mode<->executor binding.
func NewManualController(cfg ManualControllerConfig) (*ManualController, error) {
	if !cfg.Acct.IsBroker() {
		return nil, fmt.Errorf("%w: manual controller requires a broker account (simulate/real), got env %q", domain.ErrInvalidArgument, cfg.Acct.Env)
	}
	if cfg.Executor == nil || cfg.Account == nil {
		return nil, fmt.Errorf("%w: manual controller requires an executor and account", domain.ErrInvalidArgument)
	}
	if cfg.Halt == nil {
		return nil, fmt.Errorf("%w: manual controller requires a halt latch", domain.ErrInvalidArgument)
	}
	// SAFETY: the controller's bound account env MUST match the executor's broker
	// binding. A real account needs a live-bound executor (which proves the
	// 4-factor activation); a simulate account must NOT wrap a live-bound executor
	// (no real-money path through a "paper" desk).
	if cfg.Acct.IsReal() && !cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: live manual controller needs a live-bound executor (4-factor activation not satisfied)", domain.ErrInvalidArgument)
	}
	if !cfg.Acct.IsReal() && cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: paper manual controller must not wrap a live-bound executor", domain.ErrInvalidArgument)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &ManualController{
		acct:          cfg.Acct,
		exec:          cfg.Executor,
		gate:          cfg.Gate,
		account:       cfg.Account,
		prices:        cfg.Prices,
		halt:          cfg.Halt,
		risk:          cfg.Risk,
		audit:         cfg.Audit,
		reconciler:    cfg.Reconciler,
		nav:           cfg.NAV,
		paperPassword: cfg.PaperTradePassword,
		clock:         clock,
	}, nil
}

// IsLive reports whether this desk is bound to a real account.
func (m *ManualController) IsLive() bool { return m.acct.IsReal() }

// Mode reports the desk's bound mode ("paper" | "live"). Surfaced by the desk's
// status/account endpoints so the UI + e2e can positively confirm a PAPER desk
// (never place against live) without inferring it from the session. Derived from
// the bound account's env (simulate => paper, real => live).
func (m *ManualController) Mode() string {
	if m.acct.IsReal() {
		return string(domain.ModeLive)
	}
	return string(domain.ModePaper)
}

// ManualAccountView is a small read-only snapshot of the MANUAL book's account for
// the desk's GET /trade/account endpoint (the UI + e2e read it). All money values
// are USD floats rounded from the fixed-point domain money.
type ManualAccountView struct {
	Cash          float64
	Equity        float64
	DayPnLUSD     float64
	OpenPositions int
}

// AccountSnapshot returns a read-only view of the MANUAL book's account (cash /
// equity / day P&L), marked to market against the desk's NAV baseline. ok=false
// when the snapshot cannot be computed (the caller then omits the fields).
func (m *ManualController) AccountSnapshot() (ManualAccountView, bool) {
	snap, err := m.account.MarkedSnapshot(m.nav)
	if err != nil {
		return ManualAccountView{}, false
	}
	day, derr := snap.TotalPnLToday()
	dayUSD := 0.0
	if derr == nil {
		dayUSD = day.Float64()
	}
	return ManualAccountView{
		Cash:          snap.Cash.Float64(),
		Equity:        snap.NAV.Float64(),
		DayPnLUSD:     dayUSD,
		OpenPositions: len(m.account.OpenPositions()),
	}, true
}

// ManualOrderRequest is one operator order placement.
type ManualOrderRequest struct {
	// Operator identifies the human placing the order (-> audit). Required.
	Operator string
	// IdempotencyKey makes the client-order-id deterministic so a retried request
	// never double-submits. Required (the caller supplies a stable key per intent).
	IdempotencyKey string
	// Symbol / Side / Qty are the order. Qty > 0 (Side encodes direction).
	Symbol string
	Side   domain.OrderSide
	Qty    domain.Qty
	// Type is MARKET (default) or LIMIT. LimitPrice is required (>0) for LIMIT.
	Type       domain.OrderType
	LimitPrice domain.Price
	// Override, when true, lets the order proceed past a risk-gate violation (an
	// audited operator decision). The override + violated rule are recorded.
	Override bool
	// Confirm is the per-order gate: the live confirmation phrase for a LIVE order,
	// or the trade password for a PAPER order.
	Confirm string
	// Reason is a free-text note carried on the order + audit.
	Reason string
}

// ManualOrderResult is the outcome of a placed manual order.
type ManualOrderResult struct {
	ClientOrderID string
	Submitted     bool
}

// PlaceManualOrder places an operator order against the bound account. Flow:
//
//  1. validate the request shape;
//  2. enforce the per-order gate (live: confirmation phrase; paper: trade
//     password) — NO order proceeds without it;
//  3. run the risk gate for an OPENING order (allocator budget + concentration +
//     daily-loss halt). On a violation: reject UNLESS Override is set, in which
//     case record the override (risk_events + audit) and proceed. A FLAT/closing
//     order (one that reduces the net toward 0) bypasses the budget;
//  4. submit through the executor with a deterministic idempotent client-order-id;
//  5. audit the action (ops.audit_log).
func (m *ManualController) PlaceManualOrder(ctx context.Context, req ManualOrderRequest) (ManualOrderResult, error) {
	if err := m.validatePlace(req); err != nil {
		return ManualOrderResult{}, err
	}
	// (2) per-order gate. This is BEFORE any venue contact: a missing/wrong phrase
	// or password fails fast with no order placed.
	if err := m.checkConfirm(req.Confirm); err != nil {
		return ManualOrderResult{}, err
	}

	// SAFETY (findings 3 + 9): serialize the read-gate-submit sequence under m.mu so
	// the isOpening()->runRiskGate(snapshot)->SubmitManual steps are atomic w.r.t.
	// other concurrent manual orders/closes. Without this, two concurrent opening
	// orders on the same symbol can both pass the budget/concentration gate (the gate
	// read + the book mutation are not otherwise serialized), and a place racing a
	// close can mis-net. The executor's coid dedupe still protects a true retry; the
	// mutex protects the risk-budget invariant the gate is supposed to enforce.
	m.mu.Lock()
	defer m.mu.Unlock()

	otype := req.Type
	if otype == "" {
		otype = domain.OrderTypeMarket
	}
	ts := m.clock()
	coid := m.clientOrderID(req.IdempotencyKey)

	// (3) risk gate. An OPENING order (grows the |net| in the order's direction) is
	// gated; a reducing/closing order bypasses the budget (closes always proceed),
	// matching the GatedSubmitter's FLAT bypass.
	opening := m.isOpening(req.Symbol, req.Side, req.Qty)
	var overrodeRule string
	if opening {
		rule, reason, ok := m.runRiskGate(ctx, coid, req, ts)
		if !ok {
			if !req.Override {
				// Rejected, no override: record the rejection (risk_events) + audit the
				// blocked attempt, return a precise error.
				m.recordGate(ctx, false, rule, reason, req, ts)
				m.auditAction(ctx, "place", req, coid, otype, false, rule)
				return ManualOrderResult{}, fmt.Errorf("%w: %s (%s)", ErrRiskViolation, reason, rule)
			}
			// Override: the operator accepted the risk. Record the override decision
			// (approved=true but carrying the bypassed rule) so the audit trail shows
			// a human overrode a real limit.
			overrodeRule = rule
			m.recordOverride(ctx, rule, reason, req, ts)
		}
	}

	// (4) submit (idempotent on the client-order-id).
	coid, submitted, err := m.exec.SubmitManual(ctx, moexec.ManualOrderSpec{
		ClientOrderID: coid,
		StrategyID:    ManualStrategyID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Qty:           req.Qty,
		Type:          otype,
		LimitPrice:    req.LimitPrice,
		Reason:        manualReason(req.Reason),
		TS:            ts,
	})
	if err != nil {
		// Still audit the attempt (the order failed at the venue) so there is a
		// durable record of the operator action.
		m.auditAction(ctx, "place", req, coid, otype, req.Override, overrodeRule)
		return ManualOrderResult{ClientOrderID: coid, Submitted: false}, err
	}

	// (5) audit the successful action.
	m.auditAction(ctx, "place", req, coid, otype, req.Override, overrodeRule)
	return ManualOrderResult{ClientOrderID: coid, Submitted: submitted}, nil
}

// CancelManualOrder cancels a working manual order by client-order-id. It is
// idempotent (cancelling an unknown / already-terminal order is a no-op success)
// and audited.
func (m *ManualController) CancelManualOrder(ctx context.Context, operator, clientOrderID string) error {
	if strings.TrimSpace(operator) == "" {
		return fmt.Errorf("%w: cancel requires an operator", domain.ErrInvalidArgument)
	}
	if strings.TrimSpace(clientOrderID) == "" {
		return fmt.Errorf("%w: cancel requires a client_order_id", domain.ErrInvalidArgument)
	}
	if err := m.exec.CancelManual(ctx, clientOrderID); err != nil {
		return err
	}
	// Audit the cancel (symbol/side/qty resolved from the tracked order when known).
	sym, side, qty := "", "", int64(0)
	if st, ok := m.exec.TrackedOrder(clientOrderID); ok {
		sym, side, qty = st.Symbol, string(st.Side), int64(st.OrderQty)
	}
	m.audit0(ctx, ManualAuditRecord{
		Operator:      operator,
		Action:        "cancel",
		Symbol:        sym,
		Side:          side,
		Qty:           qty,
		ClientOrderID: clientOrderID,
		Live:          m.IsLive(),
		TS:            m.clock(),
	})
	return nil
}

// CloseManualPosition closes (flattens) one symbol's MANUAL position by hand. qty
// <= 0 closes the entire MANUAL net in the symbol; a positive qty closes that many
// shares (clamped to the open size). It is a FLAT/closing order: it bypasses the
// budget (closes always proceed) but, for a LIVE account, STILL requires the
// per-order confirmation phrase (a real-money close is destructive). It is
// idempotent (closing an already-flat symbol is a no-op) + audited.
//
// idempotencyKey, when non-empty, makes the close client-order-id fully
// caller-deterministic (an operator double-click / a client retry on a slow
// request reuses the SAME coid and dedupes at the executor). When empty, the coid
// is derived from the CURRENT OPEN EPISODE's identity — (symbol, signed net, the
// lot's average entry price, and the last-fill timestamp) — so:
//
//   - a repeat close of the SAME open lot (a double-click / retry that reads the
//     identical position snapshot) re-derives the SAME coid and is a no-op
//     re-submit at the executor — never a second SELL / real-money oversell; AND
//
//   - a DIFFERENT open episode that happens to reach the same symbol+net (e.g. the
//     book was opened, closed, then re-opened long-10 — common across e2e runs
//     against the long-lived desk executor) derives a DISTINCT coid, so its close
//     is NOT swallowed by the prior episode's tracked coid (finding 4: the prior
//     (symbol, net)-only key collided across re-opens, the executor's idempotency
//     short-circuit returned early, and NO SELL was placed — the position never
//     flattened). The episode discriminators (AvgPx + the last-fill ts the
//     position already carries) introduce NO wall-clock component of the close's
//     OWN: they are read from the position snapshot, stable for a given open lot.
func (m *ManualController) CloseManualPosition(ctx context.Context, operator, symbol string, qty domain.Qty, confirm string, idempotencyKey string) (ManualOrderResult, error) {
	if strings.TrimSpace(operator) == "" {
		return ManualOrderResult{}, fmt.Errorf("%w: close requires an operator", domain.ErrInvalidArgument)
	}
	if strings.TrimSpace(symbol) == "" {
		return ManualOrderResult{}, fmt.Errorf("%w: close requires a symbol", domain.ErrInvalidArgument)
	}
	// Per-order gate (live: confirm phrase; paper: trade password). A close is
	// destructive on a real account, so it is confirmation-gated too.
	if err := m.checkConfirm(confirm); err != nil {
		return ManualOrderResult{}, err
	}

	// SAFETY (finding 3): serialize the read-position -> submit sequence per desk so
	// two concurrent closes of the same symbol cannot each derive a distinct coid and
	// double-submit. The position read + the coid derivation + the submit are atomic.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Read the MANUAL book's net in the symbol (the desk closes its OWN position,
	// under the MANUAL strategy id, so the fill nets that row to 0 -> CLOSED).
	pos, ok := m.account.Position(ManualStrategyID, symbol)
	if !ok || pos.SignedQty == 0 {
		return ManualOrderResult{Submitted: false}, nil // already flat: idempotent no-op
	}
	side, hasSide := domain.CloseSideFor(pos.SignedQty)
	if !hasSide {
		return ManualOrderResult{Submitted: false}, nil
	}
	abs := pos.SignedQty
	if abs < 0 {
		abs = -abs
	}
	closeQty := abs
	if qty > 0 && qty < abs {
		closeQty = qty
	}

	ts := m.clock()
	// Idempotent close coid: prefer the caller key; else derive from the current
	// OPEN EPISODE's identity — (symbol, signed net, the lot avg entry px, and the
	// last-fill ts the position carries). This dedupes a true double-click of the
	// SAME open lot (identical snapshot -> identical coid) while giving a re-opened
	// same-symbol/same-net position a DISTINCT coid (finding 4). The discriminators
	// come from the position snapshot, so the close adds NO wall-clock component of
	// its own (no oversell from a spurious second SELL).
	keyBase := strings.TrimSpace(idempotencyKey)
	if keyBase == "" {
		keyBase = fmt.Sprintf("close|%s|net%d|px%d|lot%d",
			symbol, int64(pos.SignedQty), int64(pos.AvgPx), pos.UpdatedAt.UTC().UnixNano())
	} else {
		keyBase = "close|" + keyBase
	}
	coid := m.clientOrderID(keyBase)
	coid, submitted, err := m.exec.SubmitManual(ctx, moexec.ManualOrderSpec{
		ClientOrderID: coid,
		StrategyID:    ManualStrategyID,
		Symbol:        symbol,
		Side:          side,
		Qty:           closeQty,
		Type:          domain.OrderTypeMarket,
		Reason:        "manual close",
		TS:            ts,
	})
	m.audit0(ctx, ManualAuditRecord{
		Operator:      operator,
		Action:        "close",
		Symbol:        symbol,
		Side:          string(side),
		Qty:           int64(closeQty),
		OrderType:     string(domain.OrderTypeMarket),
		ClientOrderID: coid,
		Live:          m.IsLive(),
		TS:            ts,
	})
	if err != nil {
		return ManualOrderResult{ClientOrderID: coid, Submitted: false}, err
	}
	return ManualOrderResult{ClientOrderID: coid, Submitted: submitted}, nil
}

// --- internals ---

func (m *ManualController) validatePlace(req ManualOrderRequest) error {
	if strings.TrimSpace(req.Operator) == "" {
		return fmt.Errorf("%w: manual order requires an operator", domain.ErrInvalidArgument)
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return fmt.Errorf("%w: manual order requires an idempotency_key", domain.ErrInvalidArgument)
	}
	if strings.TrimSpace(req.Symbol) == "" {
		return fmt.Errorf("%w: manual order requires a symbol", domain.ErrInvalidArgument)
	}
	if !req.Side.IsValid() {
		return fmt.Errorf("%w: manual order has invalid side %q", domain.ErrInvalidArgument, req.Side)
	}
	if req.Qty <= 0 {
		return fmt.Errorf("%w: manual order has non-positive qty %d", domain.ErrInvalidArgument, req.Qty)
	}
	if req.Type == domain.OrderTypeLimit && req.LimitPrice <= 0 {
		return fmt.Errorf("%w: manual LIMIT order requires a positive limit price", domain.ErrInvalidArgument)
	}
	return nil
}

// checkConfirm enforces the per-order gate: a LIVE order requires the exact
// per-order confirmation phrase; a PAPER order requires the trade password. This
// is the LAST line before the order reaches the venue, on TOP of the executor's
// 4-factor live binding. There is NO bypass.
func (m *ManualController) checkConfirm(confirm string) error {
	if m.IsLive() {
		if confirm != ManualLiveConfirmationPhrase {
			return ErrConfirmationRequired
		}
		return nil
	}
	// Paper: require the configured trade password. An empty configured password
	// means the paper desk is not unlocked — refuse (a paper desk still gates).
	if m.paperPassword == "" || confirm != m.paperPassword {
		return ErrTradePasswordRequired
	}
	return nil
}

// isOpening reports whether the order GROWS the MANUAL net |position| in its
// direction (an OPEN), vs reducing it toward 0 (a close). A reducing order bypasses
// the budget gate (closes always proceed), matching the GatedSubmitter's FLAT
// bypass. The check is against the MANUAL book's net (the desk's own position).
func (m *ManualController) isOpening(symbol string, side domain.OrderSide, qty domain.Qty) bool {
	pos, ok := m.account.Position(ManualStrategyID, symbol)
	net := domain.Qty(0)
	if ok {
		net = pos.SignedQty
	}
	signed := qty
	if side == domain.OrderSideSell {
		signed = -qty
	}
	// Same sign as the existing net (or opening from flat) => growing => opening.
	// Opposite sign that does not overshoot => purely reducing => not opening.
	if net == 0 {
		return true
	}
	if (net > 0) == (signed > 0) {
		return true // adding to the position
	}
	// Reducing: opening only if it flips through 0 (overshoots the net).
	return absQty(signed) > absQty(net)
}

// runRiskGate runs Portfolio.Check for an opening order. ok=true means approved;
// ok=false returns the violated rule + reason. A nil gate always approves.
func (m *ManualController) runRiskGate(ctx context.Context, coid string, req ManualOrderRequest, ts time.Time) (rule, reason string, ok bool) {
	// Daily-loss halt: an opening order is suppressed while halted (FLAT bypasses,
	// but isOpening already excluded reductions). A halted open is a violation that
	// the override flag can also bypass (an explicit operator decision).
	if m.halt != nil && m.halt.IsHalted() {
		return "risk.daily_loss_halt", "trading halted: new opening orders suppressed (existing positions stay open, FLAT still allowed)", false
	}
	if m.gate == nil {
		return "", "", true
	}
	snap, err := m.account.Snapshot()
	if err != nil {
		return "exec.snapshot_failed", err.Error(), false
	}
	price := m.gatePrice(ctx, req)
	// SAFETY (finding 3): a 0 price collapses the order to 0 notional, which makes the
	// budget + concentration rules silently APPROVE an arbitrarily large opening order.
	// If we cannot price the symbol at all (no limit price, no broker quote, no
	// fill-observed price), the gate must NOT silently pass — fail closed so the
	// operator gets a precise 422 (override still available as the audited escape).
	if price <= 0 {
		return "risk.unpriced_symbol", "cannot price " + req.Symbol + " for the risk gate (no limit price, no broker quote); supply a LIMIT price or override", false
	}
	signalSide := domain.SideLong
	if req.Side == domain.OrderSideSell {
		signalSide = domain.SideShort
	}
	proposed := portfolio.NewProposedOrder(ManualStrategyID, req.Symbol, signalSide, req.Qty, price, ts)
	decision := m.gate.Check(proposed, portfolio.SnapshotFromDomain(snap))
	if decision.Approved {
		return "", "", true
	}
	_ = coid
	return decision.RuleName, decision.Reason, false
}

// gatePrice resolves the reference price the risk gate prices an order at, in
// priority order: an explicit LIMIT price (the operator's own ceiling), then the
// broker/market-data PriceSource (so a never-filled discretionary symbol is still
// priced — the finding-3 fix), then the manual book's fill-observed LastPrice
// (which is 0 until the symbol has filled). Returns 0 only when none resolve.
func (m *ManualController) gatePrice(ctx context.Context, req ManualOrderRequest) domain.Price {
	if req.Type == domain.OrderTypeLimit && req.LimitPrice > 0 {
		return req.LimitPrice
	}
	if m.prices != nil {
		if px, ok := m.prices.LastPrice(ctx, req.Symbol); ok && px > 0 {
			return px
		}
	}
	px, _ := m.account.LastPrice(req.Symbol)
	return px
}

// recordGate persists a gate decision to live.risk_events (nil-safe).
func (m *ManualController) recordGate(ctx context.Context, approved bool, rule, reason string, req ManualOrderRequest, ts time.Time) {
	if m.risk == nil {
		return
	}
	side := domain.SideLong
	if req.Side == domain.OrderSideSell {
		side = domain.SideShort
	}
	price := m.gatePrice(ctx, req)
	_ = m.risk.RecordGateDecision(ctx, GateDecision{
		Approved:   approved,
		RuleName:   rule,
		Reason:     reason,
		StrategyID: ManualStrategyID,
		Symbol:     req.Symbol,
		Side:       side,
		Qty:        req.Qty,
		Price:      price,
		TS:         ts,
	})
}

// recordOverride records an operator override of a risk violation: a risk_events
// row marked approved (carrying the bypassed rule) so the audit trail shows a human
// overrode a real limit.
func (m *ManualController) recordOverride(ctx context.Context, rule, reason string, req ManualOrderRequest, ts time.Time) {
	m.recordGate(ctx, true, "override:"+rule, "operator override: "+reason, req, ts)
}

// auditAction writes an ops.audit_log row for a place action.
func (m *ManualController) auditAction(ctx context.Context, action string, req ManualOrderRequest, coid string, otype domain.OrderType, override bool, overrodeRule string) {
	m.audit0(ctx, ManualAuditRecord{
		Operator:      req.Operator,
		Action:        action,
		Symbol:        req.Symbol,
		Side:          string(req.Side),
		Qty:           int64(req.Qty),
		OrderType:     string(otype),
		ClientOrderID: coid,
		Override:      override,
		RiskRule:      overrodeRule,
		Live:          m.IsLive(),
		TS:            m.clock(),
	})
}

func (m *ManualController) audit0(ctx context.Context, rec ManualAuditRecord) {
	if m.audit == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = m.clock()
	}
	_ = m.audit.RecordManualAction(ctx, rec)
}

// clientOrderID derives a deterministic idempotent client-order-id from a manual
// key. The "MANUAL-" prefix keeps manual ids distinct from the executor's
// PAPER-O-/LIVE-O- auto ids so they can never collide in the tracking map or the
// durable order table.
func (m *ManualController) clientOrderID(key string) string {
	env := "PAPER"
	if m.IsLive() {
		env = "LIVE"
	}
	return fmt.Sprintf("MANUAL-%s-%s", env, key)
}

func manualReason(r string) string {
	if strings.TrimSpace(r) == "" {
		return "manual order"
	}
	return "manual: " + r
}

func absQty(q domain.Qty) domain.Qty {
	if q < 0 {
		return -q
	}
	return q
}
