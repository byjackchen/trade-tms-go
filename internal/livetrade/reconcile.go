package livetrade

// reconcile.go is the position reconciler (P6 locked decision 5): periodically +
// on demand it compares the broker's positions (Trd_GetPositionList) against the
// strategy books (the accounting.Account net positions) and produces a
// portfolio.ReconciliationReport. On a mismatch it ALERTS — surfaces the report
// (cockpit + a tms.halts row when configured to halt) — but NEVER auto-trades to
// correct (the spec forbids self-healing trades; a human resolves drift).
//
// The reconcile algorithm itself lives in internal/portfolio.Reconcile (the
// faithful port of reconciliation.py, spec §6). This module only sources the two
// sides from the live system and routes the report to its sinks.

import (
	"context"
	"fmt"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// BrokerPositionsSource yields the broker's current positions (the real venue or
// the mock). Satisfied by *AccountAdapter is NOT what we want here — the broker
// truth comes from the TradeClient — so the reconciler takes the client directly.
type BrokerPositionsSource interface {
	GetPositionList(ctx context.Context, accID uint64, env mo.TrdEnv) ([]mo.BrokerPosition, error)
}

// StrategyBooks yields the per-(strategy, symbol) signed share counts the
// strategies believe they hold (the accounting book). Satisfied by *AccountAdapter.
type StrategyBooks interface {
	// BookPositions returns the signed share count per (strategy, symbol),
	// skipping flat entries (the reconcile algorithm also skips qty==0).
	BookPositions() map[portfolio.PositionKey]int64
}

// ReportSink persists + surfaces a reconciliation report (-> live.reconciliation_reports
// + cockpit). May be nil (tests / Redis-less).
type ReportSink interface {
	SaveReconciliation(ctx context.Context, r portfolio.ReconciliationReport, toleranceShares int64) error
}

// MismatchAlerter is invoked when a report HasIssues (drift detected). The live
// node wires this to halt-on-reconciliation-mismatch + a cockpit alert. It must
// NOT place any order (no auto-correct). May be nil.
type MismatchAlerter interface {
	OnReconciliationMismatch(ctx context.Context, r portfolio.ReconciliationReport)
}

// Reconciler runs reconciliation on demand or on a ticker.
type Reconciler struct {
	broker    BrokerPositionsSource
	books     StrategyBooks
	sink      ReportSink
	alerter   MismatchAlerter
	accID     uint64
	env       mo.TrdEnv
	tolerance int64
	now       func() time.Time

	runs int64
}

// ReconcilerConfig assembles a Reconciler.
type ReconcilerConfig struct {
	// Broker is the broker-position source (the TradeClient; required).
	Broker BrokerPositionsSource
	// Books is the strategy-book source (the account adapter; required).
	Books StrategyBooks
	// Sink persists/surfaces the report (may be nil).
	Sink ReportSink
	// Alerter fires on a mismatch (may be nil).
	Alerter MismatchAlerter
	// AccID + Env bind the broker query (required, non-zero acc id).
	AccID uint64
	Env   mo.TrdEnv
	// ToleranceShares absorbs tiny diffs (inclusive <=); default 0 (exact).
	ToleranceShares int64
	// Now supplies the report timestamp; nil => time.Now.
	Now func() time.Time
}

// NewReconciler builds a Reconciler.
func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	if cfg.Broker == nil || cfg.Books == nil {
		return nil, fmt.Errorf("livetrade: reconciler requires a broker source and strategy books")
	}
	if cfg.AccID == 0 {
		return nil, fmt.Errorf("livetrade: reconciler requires a non-zero acc id")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{
		broker:    cfg.Broker,
		books:     cfg.Books,
		sink:      cfg.Sink,
		alerter:   cfg.Alerter,
		accID:     cfg.AccID,
		env:       cfg.Env,
		tolerance: cfg.ToleranceShares,
		now:       now,
	}, nil
}

// Reconcile runs ONE reconciliation: pull the broker positions, aggregate the
// strategy books, compute the report (portfolio.Reconcile), persist + surface it.
// On a mismatch it fires the alerter (halt + cockpit) but NEVER auto-trades. The
// report is returned so the caller (CLI / endpoint) can render it.
func (r *Reconciler) Reconcile(ctx context.Context) (portfolio.ReconciliationReport, error) {
	brokerPositions, err := r.broker.GetPositionList(ctx, r.accID, r.env)
	if err != nil {
		return portfolio.ReconciliationReport{}, fmt.Errorf("reconcile: GetPositionList: %w", err)
	}
	broker := make(map[string]int64, len(brokerPositions))
	for _, p := range brokerPositions {
		if p.Qty == 0 {
			continue
		}
		broker[p.Symbol] += int64(p.Qty)
	}

	books := r.books.BookPositions()

	report := portfolio.Reconcile(r.now().UTC(), books, broker, r.tolerance)
	r.runs++

	if r.sink != nil {
		if serr := r.sink.SaveReconciliation(ctx, report, r.tolerance); serr != nil {
			return report, fmt.Errorf("reconcile: persist report: %w", serr)
		}
	}
	if report.HasIssues() && r.alerter != nil {
		// Surface the drift (halt + cockpit). NO auto-correct (spec §6).
		r.alerter.OnReconciliationMismatch(ctx, report)
	}
	return report, nil
}

// Runs returns how many reconciliations have been executed (telemetry / tests).
func (r *Reconciler) Runs() int64 { return r.runs }

// RunPeriodic runs Reconcile every interval until ctx is cancelled. A reconcile
// error is non-fatal (logged by the sink/caller via the returned report path is
// not available here; periodic mode swallows transient errors so a single broker
// hiccup does not stop the loop). It runs one reconcile immediately on start.
func (r *Reconciler) RunPeriodic(ctx context.Context, interval time.Duration, onErr func(error)) {
	if interval <= 0 {
		interval = time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if _, err := r.Reconcile(ctx); err != nil && onErr != nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// BookPositions returns the account's signed per-(strategy, symbol) positions,
// skipping flat entries — the StrategyBooks source for reconciliation.
func (a *AccountAdapter) BookPositions() map[portfolio.PositionKey]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[portfolio.PositionKey]int64)
	for _, p := range a.acct.AllPositions() {
		if p.SignedQty == 0 {
			continue
		}
		out[portfolio.PositionKey{StrategyID: p.StrategyID, Symbol: p.Symbol}] = int64(p.SignedQty)
	}
	return out
}

// compile-time check: *AccountAdapter is a StrategyBooks source.
var _ StrategyBooks = (*AccountAdapter)(nil)
