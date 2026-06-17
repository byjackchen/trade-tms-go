package runner

// manual.go wires the operator-driven MANUAL trade desk onto the live node. The
// desk is independent of the strategy execution mode: it establishes its OWN
// Trd_* connection (a paper- or live-bound MoomooExecutor) to the account so an
// operator can place/cancel/close orders by hand EVEN while the strategy session
// stays in signal mode (strategies only signal; the operator is the executor). In
// paper/live mode the desk is a manual override alongside the strategy's auto book.
//
// SAFETY: connecting a LIVE manual desk re-runs the FULL 4-factor activation via
// moexec.New with a real-env Account (Account.IsReal()) — real acc id +
// TMS_LIVE_CONFIRM phrase + UnlockTrade + the TMS-LIVE-REAL-001 trader id —
// exactly as the strategy live path; there is no
// manual path to a real account without it. The connect is audited (an
// ops.audit_log row). The desk's own per-order confirm + risk gate run on every
// order (see internal/livetrade/manual.go).

import (
	"context"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

// ConnectManualSession establishes (or re-binds) the manual trade desk in the
// requested mode (paper or live). It requires the moomoo client to be connected
// (Run must have started). For live it requires the live acc id + confirmation
// phrase to have been configured at node start (NewLive enforced them up front) —
// a node started without live creds can NEVER connect a live manual desk, mirroring
// the strategy-mode live gate. Returns the bound controller.
//
// The desk gets an INDEPENDENT accounting book (a fresh accounting.Account) so its
// MANUAL positions are tracked separately from the auto strategies' books and
// reconcile cleanly under the MANUAL pseudo-strategy id.
func (l *Live) ConnectManualSession(ctx context.Context, mode string, paperTradePassword string) (*livetrade.ManualController, error) {
	switch mode {
	case modePaper:
		if l.cfg.PaperAccID == 0 {
			return nil, fmt.Errorf("manual connect: paper desk requires a SIMULATE acc id (TMS_MOOMOO_PAPER_ACC_ID)")
		}
	case modeLive:
		// SAFETY: identical up-front gate as the strategy live path. The executor
		// constructor re-asserts the full 4-factor activation below.
		if l.cfg.LiveAccID == 0 {
			return nil, fmt.Errorf("manual connect: live desk requires a REAL acc id (TMS_MOOMOO_LIVE_ACC_ID) — refusing to activate")
		}
		if l.cfg.LiveConfirmationPhrase != moexec.LiveConfirmationPhrase {
			return nil, fmt.Errorf("manual connect: live desk requires the exact confirmation phrase (TMS_LIVE_CONFIRM) — refusing to activate")
		}
		if l.cfg.TraderID != moexec.LiveTraderID {
			return nil, fmt.Errorf("manual connect: live desk requires trader-id %q (the distinct real-money namespace)", moexec.LiveTraderID)
		}
	default:
		return nil, fmt.Errorf("manual connect: mode %q invalid (want paper|live)", mode)
	}

	l.mu.RLock()
	client := l.client
	sessionID := l.sessionID
	l.mu.RUnlock()
	if client == nil {
		return nil, fmt.Errorf("manual connect: moomoo client not connected yet (node still starting)")
	}

	// Resolve the ONE broker account this manual desk binds (paper -> simulate
	// PaperAccID, live -> real LiveAccID). The executor derives TrdEnv + the live
	// gate from it; persistence stamps its id.
	tradeAcct := l.resolveAccount(mode)

	startMoney, err := domain.MoneyFromFloat64(l.startingBalance())
	if err != nil {
		return nil, fmt.Errorf("manual connect: invalid starting balance: %w", err)
	}

	// Independent durability + accounting for the manual book. The account row is
	// ensured before any order/position references it (FK).
	persist := NewLivePersist(l.pool, l.publisher, sessionID, tradeAcct.ID, l.cfg.TraderID, "MOOMOO", l.log)
	if err := persist.UpsertAccount(ctx, tradeAcct); err != nil {
		return nil, fmt.Errorf("manual connect: upserting account %s: %w", tradeAcct.ID, err)
	}
	acct := accounting.NewAccount(startMoney, nil)
	account := livetrade.NewAccountAdapter(acct)

	execCfg := moexec.Config{
		Account:  tradeAcct,
		Client:   client.TradeClient(),
		TraderID: l.cfg.TraderID,
		Sink:     noopFillSink{},
		Book:     account,
		Persist:  persist,
		Risk:     persist,
		Strategy: persist,
		Logf:     func(f string, a ...any) { l.log.Warn().Msgf("manual-exec: "+f, a...) },
	}
	if tradeAcct.IsReal() {
		execCfg.ConfirmationPhrase = l.cfg.LiveConfirmationPhrase
		execCfg.UnlockPassword = l.cfg.UnlockPassword
	}
	// SAFETY: this is the live-activation gate for the manual desk. It refuses to
	// build a live executor unless phrase + real acc id + live trader id +
	// UnlockTrade all pass.
	exec, err := moexec.New(ctx, execCfg)
	if err != nil {
		return nil, fmt.Errorf("manual connect: activating %s executor: %w", mode, err)
	}

	// DIRECTION 2 reconciler: SyncFromBroker reflects the broker truth into the
	// MANUAL book then runs THIS reconciliation so drift between the broker and TMS is
	// reported to tms.reconciliation_reports + surfaced (never auto-traded).
	//
	// SCOPE (finding 6): the broker's Trd_GetPositionList returns the WHOLE account
	// (every auto-strategy book + the manual book). Reconciling it against the
	// MANUAL-only book would mis-classify every strategy-held symbol as drift
	// (SymbolsOnlyAtBroker) and, with a halting alerter, would HALT the entire live
	// node on an operator's "Sync from broker" click in paper/live mode. So the
	// manual reconciler aggregates the WHOLE-SYSTEM books — the manual desk's account
	// PLUS the active strategy session's book (read live each reconcile; nil in signal
	// mode, where there is no strategy executor) — and uses a NON-halting alerter: a
	// sync is READ-ONLY and "safe in ALL modes", so it logs + reports drift but must
	// NEVER halt the node. (The strategy session's OWN periodic reconciler still halts
	// on genuine strategy-vs-broker drift; that path is unchanged.)
	combinedBooks := livetrade.CombineBooks(
		account.BookPositions,   // the MANUAL book
		l.strategyBookPositions, // the live strategy session's book (nil in signal mode)
	)
	reconciler, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker:          client.TradeClient(),
		Books:           combinedBooks,
		Sink:            persist,
		Alerter:         l.manualReconcileAlerter(),
		AccID:           tradeAcct.BrokerAccID,
		Env:             execEnv(tradeAcct),
		ToleranceShares: l.cfg.ReconcileTolerance,
	})
	if err != nil {
		return nil, fmt.Errorf("manual connect: building manual reconciler: %w", err)
	}

	mc, err := livetrade.NewManualController(livetrade.ManualControllerConfig{
		Acct:               tradeAcct,
		Executor:           exec,
		Gate:               l.manualGate(),
		Account:            account,
		Prices:             newBrokerPriceSource(client),
		Halt:               l.halt,
		Risk:               persist,
		Audit:              persist,
		Reconciler:         reconciler,
		NAV:                startMoney,
		PaperTradePassword: paperTradePassword,
	})
	if err != nil {
		return nil, fmt.Errorf("manual connect: building manual controller: %w", err)
	}

	l.manualMu.Lock()
	l.manual = mc
	l.manualMu.Unlock()

	// Audit the connect (the desk now holds a broker connection).
	_ = persist.RecordManualAction(ctx, livetrade.ManualAuditRecord{
		Operator: "tms-trade:" + l.cfg.TraderID,
		Action:   "connect",
		Live:     tradeAcct.IsReal(),
		TS:       time.Now().UTC(),
	})
	l.log.Warn().Str("mode", mode).Bool("live", tradeAcct.IsReal()).
		Msg("MANUAL trade desk connected")
	return mc, nil
}

// manualGate builds a portfolio gate for the manual desk attributed to the MANUAL
// pseudo-strategy with 100% capital budget + the default risk constraints, so the
// manual desk's risk gate (allocator budget + concentration + daily-loss-halt) is
// active without depending on the auto-strategy allocation. A construction error
// (it cannot happen with these static inputs) disables the gate rather than
// crashing the node — the per-order confirm + the daily-loss halt still apply.
func (l *Live) manualGate() *riskgate.Gate {
	alloc, err := riskgate.NewAllocator([]riskgate.StrategyAllocation{
		{StrategyID: livetrade.ManualStrategyID, CapitalPct: 1.0},
	})
	if err != nil {
		l.log.Error().Err(err).Msg("manual gate: allocator build failed (gate disabled)")
		return nil
	}
	rc, err := riskgate.NewRiskConstraints(riskgate.RiskConstraintsConfig{
		DailyLossHaltPct: 0.10,
		MaxSingleNamePct: 0.50,
		ConcentrationPct: 0.40,
	})
	if err != nil {
		l.log.Error().Err(err).Msg("manual gate: risk constraints build failed (gate disabled)")
		return nil
	}
	return riskgate.NewGate(alloc, rc)
}

// ManualController returns the connected manual desk (nil until
// ConnectManualSession binds one). The API/HTTP layer reads it to serve the
// /trade/* mutation endpoints.
func (l *Live) ManualController() *livetrade.ManualController {
	l.manualMu.Lock()
	defer l.manualMu.Unlock()
	return l.manual
}

// strategyBookPositions returns the ACTIVE paper/live strategy session's
// per-(strategy, symbol) book, or nil when there is no strategy executor (signal
// mode, or between sessions). It is read live on every manual reconcile so the
// whole-system books reflect the strategies' CURRENT holdings (finding 6). The trade
// session's account is itself mutex-guarded, so this is safe to read concurrently.
func (l *Live) strategyBookPositions() map[riskgate.PositionKey]int64 {
	l.mu.RLock()
	ts := l.tradeSession
	l.mu.RUnlock()
	if ts == nil {
		return nil
	}
	return ts.BookPositions()
}

// brokerPriceSource resolves a symbol's reference price for the manual desk's risk
// gate from the broker/market-data feed (Qot_RequestHistoryKL's latest daily
// close). It is the runtime fix for the inert risk gate (finding 3): the manual
// book only knows prices it has SEEN on fills, and nothing wires the live node's
// bar feed into the manual account — so a discretionary order on a never-filled
// symbol priced at 0 and the budget/concentration rules never bound. This prices
// ANY symbol (subscribed or not) so the gate actually binds. It is READ-ONLY
// (a history-K-line read; never places an order) and safe in all modes.
//
// It queries the daily K-line over a generous trailing window and takes the LAST
// bar's close — the same "latest close" the venue prices fills at — so the gate's
// notional matches the fill notional. A lookup failure / empty result returns
// ok=false (the controller then falls back, and ultimately fails the gate CLOSED
// for an unpriceable symbol rather than passing it at 0 notional).
type brokerPriceSource struct {
	client MoomooClient
}

func newBrokerPriceSource(client MoomooClient) *brokerPriceSource {
	if client == nil {
		return nil // nil-safe: a nil PriceSource just disables the broker lookup
	}
	return &brokerPriceSource{client: client}
}

// LastPrice returns symbol's latest daily close from the broker, or ok=false.
func (b *brokerPriceSource) LastPrice(ctx context.Context, symbol string) (domain.Price, bool) {
	if b == nil || b.client == nil {
		return 0, false
	}
	// Bound the lookup so a slow/unavailable feed never blocks an order placement.
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// A WIDE trailing window (≈1 year) so we always capture the latest AVAILABLE
	// daily bar and take its close — robust to a long market holiday, a thinly traded
	// symbol, or a stack whose bar data lags "now" (the mock serves real historical PG
	// bars that may predate the wall clock). The history read returns bars ascending
	// by ts, so the last element is the most-recent close.
	now := time.Now().UTC()
	begin := now.AddDate(-1, 0, -1)
	bars, err := b.client.RequestHistoryKL(qctx, symbol, qotcommon.KLType_KLType_Day, begin, now)
	if err != nil || len(bars) == 0 {
		return 0, false
	}
	last := bars[len(bars)-1].Close
	if last <= 0 {
		return 0, false
	}
	return last, true
}

// manualReconcileAlerter is the manual desk's NON-halting drift alerter. A manual
// "Sync from broker" is READ-ONLY and must be safe in ALL modes (the design requires
// it), so reconciliation drift it surfaces is LOGGED + persisted to
// reconciliation_reports (the reconciler's Sink) + visible in the sync response, but
// it MUST NOT halt the live node (unlike the strategy session's own periodic
// reconciler, which halts on genuine strategy-vs-broker drift). A halting manual
// alerter would let an operator's sync click take the whole node down on false
// drift — exactly the bug finding 6 describes.
func (l *Live) manualReconcileAlerter() livetrade.MismatchAlerter {
	return reconcileAlerterFunc(func(_ context.Context, r riskgate.ReconciliationReport) {
		l.log.Warn().Str("summary", r.Summary()).
			Msg("MANUAL sync reconciliation found drift (reported, node NOT halted — sync is read-only/safe in all modes)")
	})
}
