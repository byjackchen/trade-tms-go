package livengine

// session.go is the live session assembler: it wires internal/core's streaming
// event loop + a streaming DataFeed + the SAME strategy / portfolio / warmup
// code as backtest + a NoopExecutor (signal mode) into a runnable live node.
// One Session runs one mode (signal for P5) over one universe of strategies.
//
// On each incoming bar the session: (1) injects per-bar context on the SPY
// heartbeat (look-ahead-safe, same as backtest), (2) runs every strategy's
// OnBar through the NoopExecutor (records would-be orders, places none), then —
// once a timestamp's bars have all been delivered — (3) evaluates each
// strategy's SignalIntent and emits it to the IntentSink, plus (4) a
// PortfolioHealth snapshot. Warmup priming runs once before the loop (decision
// 3), reusing the WarmupConsumer seam exactly as engine.primeWarmup.
//
// The same intent-evaluation path is shared by RunStream (live / virtual-clock)
// and Replay (EOD batch), which is WHY the live path == batch path
// (consistency_test.go).

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// Mode is the live execution mode. Only ModeSignal is wired in P5.
type Mode string

const (
	// ModeSignal records SignalIntents and submits no orders (decision 3 + 6).
	ModeSignal Mode = "signal"
	// ModePaper runs the full trading loop against the SIMULATE broker account
	// (paper money) via the injected GatedSubmitter (P6 decision 1+2).
	ModePaper Mode = "paper"
	// ModeLive runs against the REAL broker account; reaching it requires the full
	// live-activation gate in the MoomooExecutor (P6 decision 8).
	ModeLive Mode = "live"
)

// IsValid reports whether m is a known mode.
func (m Mode) IsValid() bool { return m == ModeSignal || m == ModePaper || m == ModeLive }

// Config assembles a live Session. It mirrors the relevant fields of
// engine.Config so a caller (strategyassembly.Assembly) can build a live session
// from the SAME inputs as a backtest.
type Config struct {
	// Mode is the execution mode (P5: signal only).
	Mode Mode
	// Strategies are the already-constructed engine.Strategy adapters (SEPA /
	// Sector / Pairs / ORB), the SAME instances a backtest would run.
	Strategies []engine.Strategy
	// Portfolio is the optional pre-trade gate. In signal mode its decisions are
	// informational (recorded as risk events for audit, never blocking — there
	// are no orders to block). May be nil.
	Portfolio *portfolio.Portfolio
	// Context is the optional look-ahead-safe per-bar context provider, advanced
	// on the SPY heartbeat exactly as in backtest. May be nil.
	Context *portfolio.ContextProvider
	// SPYSymbol is the context heartbeat instrument (default "SPY").
	SPYSymbol string
	// Warmup, when non-nil, primes WarmupConsumer strategies (SEPA) from
	// pre-window history before the loop, via the WarmupProvider. Same semantics
	// as engine.WarmupConfig.
	Warmup WarmupProvider
	// WarmupSymbols are the symbols to query the WarmupProvider for at start.
	// Typically the SEPA stock universe. Empty => no warmup.
	WarmupSymbols []string
	// StartingBalance seeds the informational portfolio-health NAV used for the
	// daily-loss-halt headroom in signal mode (no real account exists).
	StartingBalance domain.Money
	// Sink receives emitted intents + health snapshots. Defaults to DiscardSink.
	Sink IntentSink
	// EmitGate, when non-nil, gates NEW-intent emission: a timestamp's intents
	// (and state summaries) are emitted only while EmitGate() returns true. This
	// is the kill-switch / halt mechanism (P5 decision 6): a halt stops emitting
	// NEW intents WITHOUT stopping the loop (bars keep flowing through the
	// strategies so state stays warm; only the emission side pauses). nil means
	// "always emit" (the EOD batch path + tests). Health snapshots are ALWAYS
	// emitted (the cockpit health panel must keep updating during a halt).
	EmitGate func() bool

	// Submitter, when non-nil, REPLACES the signal-mode NoopExecutor for paper /
	// live modes (P6 decision 1+2): the strategies run their OnBar through this
	// engine.OrderSubmitter, which actually PLACES orders (after the pre-submit
	// portfolio gate, wired into the submitter) and reads net positions from the
	// broker-settled account book. It is required for ModePaper / ModeLive and
	// must be nil for ModeSignal (the session refuses a mismatch). The submitter
	// owns the gate + executor; the session only drives the bar loop + emission.
	Submitter engine.OrderSubmitter

	// PostTimestamp, when non-nil, is invoked after each timestamp's strategies
	// have run + intents emitted, with the timestamp. The paper/live trade session
	// uses it to evaluate the daily-loss halt + emit a live health/position
	// snapshot + persist strategy state. nil (signal mode) => no-op.
	PostTimestamp func(ctx context.Context, asOf time.Time) error

	// ObserveBar, when non-nil, is invoked for every bar BEFORE the strategies run
	// (paper/live: record the bar's close into the account book so the gate's
	// estimated-fill price + the health mark-to-market are current, mirroring the
	// engine's ObserveBar). nil (signal mode) => no-op.
	ObserveBar func(bar domain.Bar)
}

// Session is one assembled live node. Build with NewSession, optionally Prime,
// then RunStream (live) or Replay (batch).
type Session struct {
	cfg Config
	// exec is the signal-mode NoopExecutor; nil in paper/live (the injected
	// Submitter is used instead).
	exec *NoopExecutor
	// sub is the order submitter the strategies run through: the NoopExecutor in
	// signal mode, or the injected GatedSubmitter in paper/live.
	sub     engine.OrderSubmitter
	sink    IntentSink
	spySym  string
	ctxStat *portfolio.SharedContextState
	ctxCons []engine.ContextConsumer
	evals   []intentEval
	states  []stateEval

	// per-timestamp accumulation: the timestamp whose bars are currently being
	// delivered, and whether any bar at it has been seen.
	curTS  time.Time
	haveTS bool
	// Telemetry counters. The dispatch loop is single-goroutine, but these are
	// read CONCURRENTLY by the live node's health endpoint / supervisor while a
	// session runs, so they are atomic (the loop writes, observers read).
	emitted       atomic.Int64
	barsSeen      atomic.Int64
	haltedFlushes atomic.Int64
}

// intentEval pairs a strategy with its IntentEvaluator capability (only
// strategies that implement it are asked for intents).
type intentEval struct {
	id   string
	eval engine.IntentEvaluator
}

// stateEval pairs a strategy with its StateSummarizer capability (only
// strategies that implement it publish a state_summary).
type stateEval struct {
	id   string
	eval engine.StateSummarizer
}

// NewSession assembles a session from cfg. It validates the mode + strategies,
// indexes the context consumers and intent evaluators, and prepares the
// NoopExecutor. It does NOT prime warmup or run; call Prime then RunStream/Replay.
func NewSession(cfg Config) (*Session, error) {
	if !cfg.Mode.IsValid() {
		return nil, fmt.Errorf("%w: unknown live mode %q", domain.ErrInvalidArgument, cfg.Mode)
	}
	// Mode/submitter pairing (P6): signal mode uses the internal NoopExecutor and
	// MUST NOT carry an injected submitter; paper/live REQUIRE one (the
	// GatedSubmitter that owns the gate + executor). This is a SAFETY invariant —
	// there is no path where a paper/live session silently runs the no-op
	// (placing nothing) or a signal session reaches a real executor.
	switch cfg.Mode {
	case ModeSignal:
		if cfg.Submitter != nil {
			return nil, fmt.Errorf("%w: signal mode must not carry an order submitter (signal places no orders)", domain.ErrInvalidArgument)
		}
	case ModePaper, ModeLive:
		if cfg.Submitter == nil {
			return nil, fmt.Errorf("%w: %s mode requires an order submitter (the gated executor)", domain.ErrInvalidArgument, cfg.Mode)
		}
	}
	if len(cfg.Strategies) == 0 {
		return nil, fmt.Errorf("%w: live session has no strategies", domain.ErrInvalidArgument)
	}
	if cfg.StartingBalance <= 0 {
		return nil, fmt.Errorf("%w: live session needs a positive starting balance for health NAV", domain.ErrInvalidArgument)
	}
	spy := cfg.SPYSymbol
	if spy == "" {
		spy = "SPY"
	}
	sink := cfg.Sink
	if sink == nil {
		sink = DiscardSink{}
	}
	s := &Session{
		cfg:    cfg,
		sink:   sink,
		spySym: spy,
	}
	if cfg.Mode == ModeSignal {
		s.exec = NewNoopExecutor()
		s.sub = s.exec
	} else {
		s.sub = cfg.Submitter
	}
	if cfg.Context != nil {
		s.ctxStat = portfolio.NewSharedContextState()
	}
	for _, st := range cfg.Strategies {
		if st == nil || st.ID() == "" {
			return nil, fmt.Errorf("%w: live session has a nil/empty-id strategy", domain.ErrInvalidArgument)
		}
		if cc, ok := st.(engine.ContextConsumer); ok {
			s.ctxCons = append(s.ctxCons, cc)
		}
		if ie, ok := st.(engine.IntentEvaluator); ok {
			s.evals = append(s.evals, intentEval{id: st.ID(), eval: ie})
		}
		if se, ok := st.(engine.StateSummarizer); ok {
			s.states = append(s.states, stateEval{id: st.ID(), eval: se})
		}
	}
	return s, nil
}

// Executor exposes the signal-mode NoopExecutor (for telemetry: WouldSubmitCount).
// It is nil in paper/live mode (the injected Submitter owns execution).
func (s *Session) Executor() *NoopExecutor { return s.exec }

// Prime feeds the out-of-band warmup history into every WarmupConsumer strategy,
// once per (symbol, strategy), BEFORE the loop — reusing the exact semantics of
// engine.primeWarmup (no orders, no fills, no emissions; pure indicator/history
// priming). A nil Warmup or empty WarmupSymbols is a no-op. Symbols are primed
// in sorted order (priming is per-symbol independent; a stable order keeps logs
// reproducible).
func (s *Session) Prime(ctx context.Context) error {
	if s.cfg.Warmup == nil || len(s.cfg.WarmupSymbols) == 0 {
		return nil
	}
	syms := append([]string(nil), s.cfg.WarmupSymbols...)
	sort.Strings(syms)
	for _, st := range s.cfg.Strategies {
		wc, ok := st.(engine.WarmupConsumer)
		if !ok {
			continue
		}
		for _, sym := range syms {
			hist, err := s.cfg.Warmup.WarmupBars(ctx, sym)
			if err != nil {
				return fmt.Errorf("livengine: warmup %s: %w", sym, err)
			}
			if len(hist) > 0 {
				wc.WarmupBars(sym, hist)
			}
		}
	}
	return nil
}

// RunStream drives the live loop over feed using the chosen clock discipline.
// For the live engine pass core.StreamWall + nil vc; for a deterministic test
// pass core.StreamVirtual + the controllable clock. It registers the bar handler
// on a fresh StreamLoop and drains the feed's source until end-of-stream or ctx
// cancellation, flushing the final timestamp's intents on a clean drain.
func (s *Session) RunStream(ctx context.Context, feed StreamFeed, mode core.StreamClockMode, vc *core.VirtualClock) error {
	loop := core.NewStreamLoop(mode, vc)
	loop.Register(core.KindBar, core.HandlerFunc(s.handleBar))

	src, err := feed.Open(ctx)
	if err != nil {
		return fmt.Errorf("livengine: opening feed: %w", err)
	}
	if rerr := loop.RunStream(ctx, src); rerr != nil {
		return fmt.Errorf("livengine: stream run: %w", rerr)
	}
	// Flush the final (still-open) timestamp's intents — the loop ends without a
	// boundary bar to trigger the flush.
	return s.flushTimestamp(ctx)
}

// Replay drives the SAME bar-handling path over a pre-ordered batch of bars
// (the EOD-replay / consistency-proof path). It is the idempotent engine-REPLAY
// mode (decision 4): the caller supplies the dispatch-ordered bars for
// [as_of-window, as_of] and Replay evaluates + emits each timestamp's intents
// identically to RunStream. No event loop is needed — the bars are already
// ordered — but every bar flows through the same handleBar, so the emitted
// intents match the streaming path bit-for-bit.
func (s *Session) Replay(ctx context.Context, bars []domain.Bar) error {
	for _, b := range bars {
		if err := s.onBar(ctx, b); err != nil {
			return fmt.Errorf("livengine: replay: %w", err)
		}
	}
	return s.flushTimestamp(ctx)
}

// handleBar is the core.Handler adapter for KindBar events (streaming path).
func (s *Session) handleBar(ctx context.Context, ev core.Event) error {
	be, ok := ev.(core.BarEvent)
	if !ok {
		return fmt.Errorf("livengine: KindBar handler got %T", ev)
	}
	return s.onBar(ctx, be.Bar)
}

// onBar processes one bar (shared by streaming + replay). On a timestamp
// rollover it first flushes the prior timestamp's intents (so intents are
// evaluated only after every bar at a timestamp has run OnBar), then injects
// context on the SPY heartbeat and runs every strategy's OnBar.
func (s *Session) onBar(ctx context.Context, bar domain.Bar) error {
	if _, off := bar.TS.Zone(); off != 0 {
		return fmt.Errorf("%w: live bar %s at non-UTC %s", domain.ErrInvalidArgument, bar.Symbol, bar.TS)
	}
	// Timestamp rollover: emit the completed timestamp's intents before starting
	// the new one.
	if s.haveTS && bar.TS.After(s.curTS) {
		if err := s.flushTimestamp(ctx); err != nil {
			return err
		}
	}
	if s.haveTS && bar.TS.Before(s.curTS) {
		return fmt.Errorf("%w: live bar %s at %s precedes current timestamp %s",
			core.ErrTimeReversal, bar.Symbol, bar.TS, s.curTS)
	}
	s.curTS = bar.TS
	s.haveTS = true
	s.barsSeen.Add(1)

	// Record the bar's close into the account book (paper/live) BEFORE strategies
	// run, mirroring the engine's ObserveBar: the pre-submit gate's estimated-fill
	// price + the health mark-to-market read the current bar's close. No-op in
	// signal mode.
	if s.cfg.ObserveBar != nil {
		s.cfg.ObserveBar(bar)
	}

	// Context refresh on the SPY heartbeat (look-ahead-safe), identical to
	// engine.handleBar: advance the provider, push the snapshot into every
	// ContextConsumer before OnBar.
	if s.cfg.Context != nil && bar.Symbol == s.spySym {
		s.cfg.Context.OnBar(s.ctxStat, bar.TS)
		s.injectContext(bar.TS)
	}

	// Run strategies through the submitter: the NoopExecutor (signal: records
	// would-be orders, places none) or the GatedSubmitter (paper/live: gate +
	// place). Registration order = cfg.Strategies order (deterministic).
	for _, st := range s.cfg.Strategies {
		if err := st.OnBar(s.sub, bar); err != nil {
			return fmt.Errorf("livengine: strategy %s on bar %s@%s: %w", st.ID(), bar.Symbol, bar.TS, err)
		}
	}
	return nil
}

// injectContext snapshots the shared context state into every ContextConsumer,
// identical to engine.injectContext.
func (s *Session) injectContext(asOf time.Time) {
	if len(s.ctxCons) == 0 || s.ctxStat == nil {
		return
	}
	ctx := engine.StrategyContext{
		Regime:           s.ctxStat.Regime(),
		AsOf:             asOf,
		MarketCapUSD:     s.ctxStat.MarketCapFloats(),
		EarningsBlackout: s.ctxStat.EarningsBlackouts(),
	}
	for _, cc := range s.ctxCons {
		cc.InjectContext(ctx)
	}
}

// flushTimestamp evaluates and emits every strategy's SignalIntent for the
// current timestamp, then emits a PortfolioHealth snapshot, then clears the
// timestamp marker. It is called on a timestamp rollover and at end-of-run. A
// flush with no pending timestamp is a no-op.
func (s *Session) flushTimestamp(ctx context.Context) error {
	if !s.haveTS {
		return nil
	}
	asOf := s.curTS
	// Halt gate (decision 6): when halted, stop emitting NEW intents + state
	// summaries for this timestamp, but STILL emit the health snapshot (the
	// cockpit health panel keeps updating; daily_loss_halt is informational in
	// signal mode). The strategies already ran OnBar above, so their state stays
	// warm — only the emission side pauses.
	if s.cfg.EmitGate != nil && !s.cfg.EmitGate() {
		s.haltedFlushes.Add(1)
		if err := s.emitHealth(ctx, asOf); err != nil {
			return err
		}
		// PostTimestamp (paper/live) still runs while halted: the live health /
		// position snapshot + daily-loss re-evaluation must keep updating even when
		// NEW-intent emission is paused (the halt suppresses intents, not telemetry).
		if s.cfg.PostTimestamp != nil {
			if err := s.cfg.PostTimestamp(ctx, asOf); err != nil {
				return err
			}
		}
		s.haveTS = false
		return nil
	}
	// Evaluate intents in strategy registration order (deterministic), matching
	// the order the batch path would evaluate them.
	for _, ie := range s.evals {
		payload := ie.eval.EvaluateIntentJSON(asOf)
		if err := s.sink.EmitIntent(ctx, IntentRecord{
			StrategyID: ie.id,
			AsOf:       asOf,
			Payload:    payload,
		}); err != nil {
			return fmt.Errorf("livengine: emit intent %s@%s: %w", ie.id, asOf, err)
		}
		s.emitted.Add(1)
	}
	// State summaries (StrategyStateUpdate, §5.8): emitted only when the sink
	// opts in via StateEmitter (DB/Redis sink does; MemSink/DiscardSink do not).
	// Evaluated in registration order, matching the intent order.
	if se, ok := s.sink.(StateEmitter); ok {
		for _, st := range s.states {
			summary := st.eval.StateSummaryJSON()
			if err := se.EmitState(ctx, StateRecord{
				StrategyID: st.id,
				AsOf:       asOf,
				Summary:    summary,
			}); err != nil {
				return fmt.Errorf("livengine: emit state %s@%s: %w", st.id, asOf, err)
			}
		}
	}
	if err := s.emitHealth(ctx, asOf); err != nil {
		return err
	}
	// PostTimestamp (paper/live): evaluate the daily-loss halt + emit the live
	// health/position snapshot + persist strategy state, after this timestamp's
	// intents. No-op in signal mode.
	if s.cfg.PostTimestamp != nil {
		if err := s.cfg.PostTimestamp(ctx, asOf); err != nil {
			return err
		}
	}
	s.haveTS = false
	return nil
}

// emitHealth computes and emits the informational PortfolioHealth snapshot. In
// signal mode there are no positions, so the snapshot reflects an empty book at
// the starting NAV (day P&L 0, no halt — decision 6). When no gate is configured
// the snapshot is skipped (HealthSnapshot needs the risk config).
func (s *Session) emitHealth(ctx context.Context, asOf time.Time) error {
	if s.cfg.Portfolio == nil {
		return nil
	}
	// In paper/live the trade session's PostTimestamp emits the REAL health
	// snapshot (marked against the live account book), so the session's flat-book
	// signal-mode snapshot is suppressed to avoid a misleading zero overwrite.
	if s.cfg.PostTimestamp != nil {
		return nil
	}
	// Empty signal-mode book: NAV = cash = starting balance, no positions, no
	// last-close marks. The reference PortfolioHealthActor reads the venue
	// account; in signal mode that account is flat, so this is the faithful
	// informational snapshot.
	snap := domain.NewAccountSnapshot(
		s.cfg.StartingBalance, s.cfg.StartingBalance, 0, 0,
		map[domain.StrategySymbol]domain.Qty{}, map[string]domain.Price{},
	)
	health := s.cfg.Portfolio.HealthSnapshot(portfolio.SnapshotFromDomain(snap))
	if err := s.sink.EmitHealth(ctx, HealthRecord{AsOf: asOf, Snapshot: health}); err != nil {
		return fmt.Errorf("livengine: emit health@%s: %w", asOf, err)
	}
	return nil
}

// EmittedIntents returns the count of intents emitted so far (telemetry).
func (s *Session) EmittedIntents() int { return int(s.emitted.Load()) }

// BarsSeen returns the count of bars processed so far (telemetry).
func (s *Session) BarsSeen() int { return int(s.barsSeen.Load()) }

// HaltedFlushes returns the count of timestamps whose intent emission was
// suppressed by the EmitGate (halt telemetry; health was still emitted).
func (s *Session) HaltedFlushes() int { return int(s.haltedFlushes.Load()) }
