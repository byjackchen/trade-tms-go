package livetrade

// session.go assembles a paper/live trading session (P6 decision 1): it composes
// the account adapter + MoomooExecutor + GatedSubmitter + reconciler into a
// livengine.Session running in paper/live mode. It owns the PostTimestamp hook
// the session calls after each bar timestamp: evaluate the daily-loss halt, emit
// the live health/position/account snapshot, and persist strategy state for
// crash recovery.
//
// The SAME strategy / portfolio / warmup / context code that runs in signal mode
// (and backtest) runs here unchanged — the ONLY substitution is the submitter
// (gated MoomooExecutor instead of NoopExecutor) and the PostTimestamp telemetry.

import (
	"context"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// StatePersister snapshots + restores per-strategy SG state for crash recovery
// (decision 6). Satisfied by the strategy adapters' engine.StatePersister; the
// session probes each strategy for it.
type StatePersister = engine.StatePersister

// StateStore durably stores + loads strategy SG state_dicts keyed by
// (session, strategy) (-> PG, decision 6). May be nil (tests / no recovery).
type StateStore interface {
	// SaveState persists strategyID's state JSON (idempotent upsert on the key).
	SaveState(ctx context.Context, strategyID string, state []byte) error
	// LoadState returns strategyID's last persisted state JSON, ok=false if none.
	LoadState(ctx context.Context, strategyID string) ([]byte, bool, error)
}

// HealthSink publishes the live health + position + account snapshot after each
// timestamp (-> Redis cockpit + PG). May be nil.
type HealthSink interface {
	EmitLiveHealth(ctx context.Context, snap LiveSnapshot) error
}

// LiveSnapshot is the post-timestamp telemetry the trade session emits: the
// portfolio-health snapshot marked against the live account, the position book,
// the account/buying-power view, and whether a daily-loss halt latched this tick.
type LiveSnapshot struct {
	AsOf            time.Time
	Health          portfolio.PortfolioHealthSnapshot
	Positions       []domain.Position
	DailyLossHalted bool
	// HaltLatchedNow is true on the tick the daily-loss halt first latched.
	HaltLatchedNow bool
}

// TradeSession is one assembled paper/live trading node.
type TradeSession struct {
	cfg     TradeSessionConfig
	account *AccountAdapter
	exec    *moexec.MoomooExecutor
	gated   *GatedSubmitter
	session *livengine.Session

	// statePersisters indexes strategies that can snapshot/restore their SG state.
	statePersisters []statePersister
}

type statePersister struct {
	id string
	sp StatePersister
}

// TradeSessionConfig assembles a TradeSession.
type TradeSessionConfig struct {
	// Mode is paper or live (the livengine mode).
	Mode livengine.Mode
	// Strategies are the engine strategy adapters (same instances backtest uses).
	Strategies []engine.Strategy
	// Gate is the portfolio gating pipeline (allocator + risk). Required for
	// production; nil disables gating (tests only).
	Gate *portfolio.Portfolio
	// Context is the look-ahead-safe per-bar context provider (may be nil).
	Context *portfolio.ContextProvider
	// SPYSymbol is the context heartbeat instrument (default "SPY").
	SPYSymbol string
	// Warmup primes WarmupConsumer strategies before the loop (may be nil).
	Warmup livengine.WarmupProvider
	// WarmupSymbols are the symbols to prime.
	WarmupSymbols []string
	// WarmupBatch is the interleaved pre-window bar stream priming the multi-symbol
	// BatchWarmupConsumer strategies (sector / pairs) before the loop (may be nil).
	WarmupBatch []domain.Bar
	// Account is the accounting adapter the executor settles into (required).
	Account *AccountAdapter
	// Executor is the paper/live MoomooExecutor (required).
	Executor *moexec.MoomooExecutor
	// Halt is the live node's halt latch (required: daily-loss-halt enforcement).
	Halt Halter
	// Risk records gate decisions (may be nil).
	Risk RiskRecorder
	// NAV is the daily-loss-halt NAV baseline (the live account starting balance).
	NAV domain.Money
	// IntentSink receives intents + state summaries (may be DiscardSink).
	IntentSink livengine.IntentSink
	// EmitGate gates NEW-intent + opening-order emission (the halt). nil => always.
	EmitGate func() bool
	// StateStore persists strategy SG state for crash recovery (may be nil).
	StateStore StateStore
	// HealthSink publishes the post-timestamp live snapshot (may be nil).
	HealthSink HealthSink
}

// NewTradeSession assembles a paper/live trading node. It builds the gated
// submitter (gate + executor), indexes the strategies' state persisters, and
// constructs the underlying livengine.Session in paper/live mode wired to the
// PostTimestamp telemetry hook.
func NewTradeSession(cfg TradeSessionConfig) (*TradeSession, error) {
	if cfg.Mode != livengine.ModePaper && cfg.Mode != livengine.ModeLive {
		return nil, fmt.Errorf("%w: trade session mode %q (want paper/live)", domain.ErrInvalidArgument, cfg.Mode)
	}
	if cfg.Account == nil || cfg.Executor == nil {
		return nil, fmt.Errorf("%w: trade session requires an account adapter and executor", domain.ErrInvalidArgument)
	}
	if cfg.Halt == nil {
		return nil, fmt.Errorf("%w: trade session requires a halt latch", domain.ErrInvalidArgument)
	}
	// SAFETY: a live trade session must be backed by a live-bound executor; a
	// paper session by a paper-bound one. The executor's IsLive reflects the
	// activation gate (TrdEnvReal only after the full live gate).
	if cfg.Mode == livengine.ModeLive && !cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: live trade session needs a live-bound executor (activation gate not satisfied)", domain.ErrInvalidArgument)
	}
	if cfg.Mode == livengine.ModePaper && cfg.Executor.IsLive() {
		return nil, fmt.Errorf("%w: paper trade session must not use a live-bound executor", domain.ErrInvalidArgument)
	}

	gated := NewGatedSubmitter(GatedSubmitterConfig{
		Executor: cfg.Executor,
		Gate:     cfg.Gate,
		Account:  cfg.Account,
		Halt:     cfg.Halt,
		Risk:     cfg.Risk,
		NAV:      cfg.NAV,
	})

	ts := &TradeSession{
		cfg:     cfg,
		account: cfg.Account,
		exec:    cfg.Executor,
		gated:   gated,
	}
	for _, st := range cfg.Strategies {
		if sp, ok := st.(StatePersister); ok {
			ts.statePersisters = append(ts.statePersisters, statePersister{id: st.ID(), sp: sp})
		}
	}

	sink := cfg.IntentSink
	if sink == nil {
		sink = livengine.DiscardSink{}
	}
	sess, err := livengine.NewSession(livengine.Config{
		Mode:            cfg.Mode,
		Strategies:      cfg.Strategies,
		Portfolio:       cfg.Gate,
		Context:         cfg.Context,
		SPYSymbol:       cfg.SPYSymbol,
		Warmup:          cfg.Warmup,
		WarmupSymbols:   cfg.WarmupSymbols,
		WarmupBatch:     cfg.WarmupBatch,
		StartingBalance: navOrDefault(cfg.NAV),
		Sink:            sink,
		EmitGate:        cfg.EmitGate,
		Submitter:       gated,
		ObserveBar:      cfg.Account.ObserveBar,
		PostTimestamp:   ts.postTimestamp,
	})
	if err != nil {
		return nil, fmt.Errorf("livetrade: building trade session: %w", err)
	}
	ts.session = sess
	return ts, nil
}

// Session exposes the underlying livengine.Session (Prime / RunStream / Replay).
func (t *TradeSession) Session() *livengine.Session { return t.session }

// GatedSubmitter exposes the pre-submit gate (telemetry / tests).
func (t *TradeSession) GatedSubmitter() *GatedSubmitter { return t.gated }

// Prime restores strategy state (recovery) then primes warmup. The state restore
// runs BEFORE warmup priming so a recovered strategy is not re-warmed over its
// restored state — warmup is for COLD strategies; recovery supersedes it. The IDs
// of restored strategies are threaded into PrimeExcept so the warmup seams SKIP
// them: the batch seam (sector/pairs) APPENDS pre-window bars onto the restored
// cross-symbol ring, which would reset lastUniverseDate to an older pre-window
// month (scrambling the next-bar month rollover) and corrupt the momentum/spread
// window — the invariant this comment promises was previously NOT enforced for the
// append-based batch consumers (only SEPA's replace-based seam was idempotent).
// The caller invokes RestoreFromBroker + Reconcile separately around Prime.
func (t *TradeSession) Prime(ctx context.Context) error {
	restored, err := t.RestoreStrategyState(ctx)
	if err != nil {
		return err
	}
	return t.session.PrimeExcept(ctx, restored)
}

// postTimestamp is the livengine PostTimestamp hook (decision 4+6): after each
// timestamp's strategies + intents, it (1) evaluates the daily-loss halt and
// latches it on a breach, (2) emits the live health/position/account snapshot,
// and (3) persists each strategy's SG state for crash recovery.
func (t *TradeSession) postTimestamp(ctx context.Context, asOf time.Time) error {
	// (1) daily-loss halt evaluation (decision 4): latch a halt when day P&L
	// crosses -haltPct*NAV. While halted the gate rejects NEW opens (existing
	// positions stay open, FLAT still allowed).
	latched, _ := t.gated.EvaluateDailyLossHalt()

	// (2) live telemetry snapshot — MARKED to market so the cockpit health panel
	// reflects the real day P&L (the parity snapshot keeps day P&L 0).
	snap, err := t.account.MarkedSnapshot(t.cfg.NAV)
	if err != nil {
		return fmt.Errorf("livetrade: post-timestamp account snapshot: %w", err)
	}
	var health portfolio.PortfolioHealthSnapshot
	if t.cfg.Gate != nil {
		health = t.cfg.Gate.HealthSnapshot(portfolio.SnapshotFromDomain(snap))
	}
	if t.cfg.HealthSink != nil {
		if herr := t.cfg.HealthSink.EmitLiveHealth(ctx, LiveSnapshot{
			AsOf:            asOf,
			Health:          health,
			Positions:       t.account.openPositions(),
			DailyLossHalted: health.IsDailyLossHalt() || t.cfg.Halt.IsHalted(),
			HaltLatchedNow:  latched,
		}); herr != nil {
			return fmt.Errorf("livetrade: emit live health: %w", herr)
		}
	}

	// (3) persist strategy SG state (decision 6: state_dict to PG on change).
	if err := t.PersistStrategyState(ctx); err != nil {
		return fmt.Errorf("livetrade: persist strategy state: %w", err)
	}
	return nil
}

// PersistStrategyState snapshots every StatePersister strategy's SG state_dict
// and writes it to the StateStore (idempotent upsert). A nil StateStore is a
// no-op. Called after each timestamp + on demand.
func (t *TradeSession) PersistStrategyState(ctx context.Context) error {
	if t.cfg.StateStore == nil {
		return nil
	}
	for _, sp := range t.statePersisters {
		state, err := marshalState(sp.sp.StateDictJSON())
		if err != nil {
			return fmt.Errorf("livetrade: marshal state %s: %w", sp.id, err)
		}
		if err := t.cfg.StateStore.SaveState(ctx, sp.id, state); err != nil {
			return fmt.Errorf("livetrade: save state %s: %w", sp.id, err)
		}
	}
	return nil
}

// RestoreStrategyState loads each StatePersister strategy's last persisted SG
// state from the StateStore and applies it (decision 6). A strategy with no
// stored state is left cold (the warmup path primes it). A nil StateStore is a
// no-op. It returns the set of strategy IDs whose state was actually restored, so
// the caller can SKIP re-warming them (recovery supersedes warmup) — a nil/empty
// map when nothing was restored (all-cold start), which makes warmup behave
// exactly as a fresh session.
func (t *TradeSession) RestoreStrategyState(ctx context.Context) (map[string]bool, error) {
	if t.cfg.StateStore == nil {
		return nil, nil
	}
	var restored map[string]bool
	for _, sp := range t.statePersisters {
		state, ok, err := t.cfg.StateStore.LoadState(ctx, sp.id)
		if err != nil {
			return nil, fmt.Errorf("livetrade: load state %s: %w", sp.id, err)
		}
		if !ok || len(state) == 0 {
			continue
		}
		if err := sp.sp.LoadStateJSON(state); err != nil {
			return nil, fmt.Errorf("livetrade: restore state %s: %w", sp.id, err)
		}
		if restored == nil {
			restored = make(map[string]bool, len(t.statePersisters))
		}
		restored[sp.id] = true
	}
	return restored, nil
}

// BookPositions exposes the strategy session's per-(strategy, symbol) signed book
// so the MANUAL desk's reconciler can aggregate the WHOLE-SYSTEM books (strategy +
// manual) against the broker truth — the broker's Trd_GetPositionList returns the
// ENTIRE account (strategy + manual positions), so reconciling it against the
// manual-only book alone would mis-classify every strategy-held symbol as drift
// (finding 6). Satisfies the StrategyBooks interface.
func (t *TradeSession) BookPositions() map[portfolio.PositionKey]int64 {
	return t.account.BookPositions()
}

// openPositions returns the account's open (non-flat) positions.
func (a *AccountAdapter) openPositions() []domain.Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acct.OpenPositions()
}

// navOrDefault returns a positive NAV for the session's informational balance.
func navOrDefault(nav domain.Money) domain.Money {
	if nav > 0 {
		return nav
	}
	return domain.MustMoney("100000.00")
}
