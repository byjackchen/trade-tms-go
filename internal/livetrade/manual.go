package livetrade

// manual.go (now the BROKER-SYNC controller) is DIRECTION 2 — the broker -> TMS
// SYNC/REFLECT desk. There is NO operator order-ENTRY surface in TMS anymore: the
// operator places orders DIRECTLY at the broker (the moomoo app) and TMS only
// pulls that externally-placed state back in. This controller holds a READ-ONLY
// Trd_* trade connection (a paper- or live-bound MoomooExecutor + AccountAdapter)
// and exposes ONLY the sync/connect surface (SyncFromBroker — see broker_sync.go).
//
// It is usable in BOTH signal and auto sessions, paper AND live: the sync only
// reads (Trd_Get*) and reflects the broker truth into a TMS book, so it can never
// place a real order and is safe in every mode.
//
// ATTRIBUTION: synced broker positions are reflected under the EXTERNAL pseudo-
// strategy id (ExternalStrategyID), distinct from the auto strategies' books, so
// externally-placed trades surface in TMS WITHOUT corrupting the strategy books.
// The synced truth is then reconciled (P6: broker net vs the strategy books) so
// drift the external trades introduced is reported to live.reconciliation_reports.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
)

// ExternalStrategyID is the pseudo-strategy every synced (externally-placed)
// position is attributed to, distinct from the auto strategies' books so
// reconciliation + per-strategy accounting stay clean. It is the strategy id on the
// live.orders / live.fills / live.positions rows a broker sync produces. Positions
// placed OUTSIDE TMS and synced in live in under this EXTERNAL book.
const ExternalStrategyID = "EXTERNAL"

// AuditSink records a broker-sync/connect action to ops.audit_log (operator,
// action, ts). It is REQUIRED in production (every sync MUST audit); a nil sink is
// allowed only in tests and drops the row. Satisfied by the runner's LivePersist
// sync-audit adapter.
type AuditSink interface {
	RecordSyncAction(ctx context.Context, a SyncAuditRecord) error
}

// SyncAuditRecord is one audited broker-sync/connect action (-> ops.audit_log).
type SyncAuditRecord struct {
	Operator string
	Action   string // "sync" | "connect"
	Qty      int64  // for "sync": the count of reflected symbols
	Live     bool
	TS       time.Time
}

// BrokerSyncController is the READ-ONLY broker-sync desk over one paper/live
// account. It is safe for concurrent use (the API may serve several operators).
type BrokerSyncController struct {
	acct       domain.Account
	exec       *moexec.MoomooExecutor
	account    *AccountAdapter
	audit      AuditSink
	reconciler *Reconciler

	nav   domain.Money
	clock func() time.Time

	mu sync.Mutex
}

// BrokerSyncControllerConfig assembles a BrokerSyncController.
type BrokerSyncControllerConfig struct {
	// Acct is the bound broker account (simulate => paper, real => live). It MUST
	// match the executor's binding (the constructor enforces: real account <=>
	// live-bound executor) so there is no way to drive a live executor through a
	// "paper" controller or vice versa.
	Acct domain.Account
	// Executor is the paper/live-bound MoomooExecutor (required). Its live binding
	// already proves the 4-factor activation for live. The sync uses ONLY its
	// read-only Trd_Get* surface (SyncBrokerInto); it never places an order.
	Executor *moexec.MoomooExecutor
	// Account is the accounting adapter the sync reflects the broker truth into
	// (required).
	Account *AccountAdapter
	// Audit records every sync/connect action to ops.audit_log (required in
	// production; nil drops the row, tests only).
	Audit AuditSink
	// Reconciler runs the P6 reconciliation (broker vs strategy books ->
	// reconciliation_reports) at the end of a SyncFromBroker so drift introduced by
	// externally-placed moomoo trades is reported. May be nil (then SyncFromBroker
	// reflects + audits but skips the reconciliation step + returns no report).
	Reconciler *Reconciler
	// NAV is the account's opening balance baseline (for the account snapshot view).
	NAV domain.Money
	// Clock supplies action timestamps; nil => time.Now (UTC).
	Clock func() time.Time
}

// NewBrokerSyncController builds the sync desk, enforcing the mode<->executor
// binding.
func NewBrokerSyncController(cfg BrokerSyncControllerConfig) (*BrokerSyncController, error) {
	if !cfg.Acct.IsBroker() {
		return nil, fmt.Errorf("%w: broker-sync controller requires a broker account (simulate/real), got env %q", domain.ErrInvalidArgument, cfg.Acct.Env)
	}
	if cfg.Executor == nil || cfg.Account == nil {
		return nil, fmt.Errorf("%w: broker-sync controller requires an executor and account", domain.ErrInvalidArgument)
	}
	// SAFETY: the controller's bound account env MUST match the executor's broker
	// binding. A real account needs a live-bound executor (which proves the
	// 4-factor activation); a simulate account must NOT wrap a live-bound executor
	// (no real-money binding through a "paper" desk).
	if cfg.Acct.IsReal() && !cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: live broker-sync controller needs a live-bound executor (4-factor activation not satisfied)", domain.ErrInvalidArgument)
	}
	if !cfg.Acct.IsReal() && cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: paper broker-sync controller must not wrap a live-bound executor", domain.ErrInvalidArgument)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &BrokerSyncController{
		acct:       cfg.Acct,
		exec:       cfg.Executor,
		account:    cfg.Account,
		audit:      cfg.Audit,
		reconciler: cfg.Reconciler,
		nav:        cfg.NAV,
		clock:      clock,
	}, nil
}

// IsLive reports whether this desk is bound to a real account.
func (m *BrokerSyncController) IsLive() bool { return m.acct.IsReal() }

// Mode reports the desk's bound mode ("paper" | "live"). Surfaced by the desk's
// status/account endpoints so the UI + e2e can positively confirm a PAPER desk
// (never bound to live) without inferring it from the session. Derived from the
// bound account's env (simulate => paper, real => live).
func (m *BrokerSyncController) Mode() string {
	if m.acct.IsReal() {
		return "live"
	}
	return "paper"
}

// SyncAccountView is a small read-only snapshot of the EXTERNAL book's account for
// the desk's GET /trade/account endpoint (the UI + e2e read it). All money values
// are USD floats rounded from the fixed-point domain money.
type SyncAccountView struct {
	Cash          float64
	Equity        float64
	DayPnLUSD     float64
	OpenPositions int
}

// AccountSnapshot returns a read-only view of the EXTERNAL book's account (cash /
// equity / day P&L), marked to market against the desk's NAV baseline. ok=false
// when the snapshot cannot be computed (the caller then omits the fields).
func (m *BrokerSyncController) AccountSnapshot() (SyncAccountView, bool) {
	snap, err := m.account.MarkedSnapshot(m.nav)
	if err != nil {
		return SyncAccountView{}, false
	}
	day, derr := snap.TotalPnLToday()
	dayUSD := 0.0
	if derr == nil {
		dayUSD = day.Float64()
	}
	return SyncAccountView{
		Cash:          snap.Cash.Float64(),
		Equity:        snap.NAV.Float64(),
		DayPnLUSD:     dayUSD,
		OpenPositions: len(m.account.OpenPositions()),
	}, true
}

// audit0 writes a sync/connect audit row (nil-safe).
func (m *BrokerSyncController) audit0(ctx context.Context, rec SyncAuditRecord) {
	if m.audit == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = m.clock()
	}
	_ = m.audit.RecordSyncAction(ctx, rec)
}
