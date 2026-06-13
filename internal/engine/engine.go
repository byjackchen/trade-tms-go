package engine

// engine.go is the assembler: it wires core (loop, msgbus, clock) + accounting
// (account, equity sampler) + exec (executor, fill model) + strategies
// (ScriptedStrategy) into a runnable, deterministic backtest, runs the event
// loop, and returns a structured Result.
//
// # Per-timestamp dispatch order
//
// The deterministic queue orders events by (ts, kind-priority, seq). At one
// timestamp the engine processes, in order:
//
//	KindBar  (per instrument, in registration order)
//	KindFill (settlements produced during KindBar)
//	KindSample (one equity sample for the day)
//
// For the nautilus-compat (this-bar) model, a BarEvent's handler:
//  1. records the bar's close as the symbol's last price (mark-to-market);
//  2. runs every strategy's OnBar (orders submitted to the executor);
//  3. calls executor.ProcessBar(bar) — orders submitted this bar fill at the
//     bar close and are scheduled as FillEvents at the SAME ts (KindFill), so
//     accounting settles them after all bars at this ts have been delivered to
//     strategies. This reproduces Nautilus's "market order in on_bar(T) fills
//     at T's close, at T" (verified empirically).
//
// For the realistic (next-bar) model, the bar handler calls
// executor.ProcessBar FIRST (filling orders pending from prior bars against
// this bar's open) and THEN runs strategies (which queue orders for the next
// bar). A trailing synthetic bar is NOT injected; orders pending after the last
// bar for a symbol remain unfilled (no future data) — the same no-look-ahead
// outcome a live system gets.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/exec"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// Engine is one assembled, runnable backtest. Build with New, then Run once.
// Not safe for concurrent use; Run drives a single goroutine.
type Engine struct {
	cfg   Config
	loop  *core.Loop
	bus   *core.MsgBus
	acct  *accounting.Account
	smplr *accounting.EquitySampler
	exec  *exec.SimExecutor
	rec   *Recorder

	strategies     []Strategy
	registration   []string // instrument symbols in registration order
	registrationIx map[string]int

	// pre-trade gating + look-ahead-safe context (multi-strategy path).
	gate    *portfolio.Portfolio
	ctxProv *portfolio.ContextProvider
	ctxStat *portfolio.SharedContextState
	spySym  string
	ctxCons []ContextConsumer // strategies implementing ContextConsumer

	rejected []RejectedOrder

	// lastBar records each symbol's most recently observed bar (real close +
	// volume), so end-of-run liquidation fills a flattening order against the
	// symbol's last bar exactly as Nautilus's on_stop close_all_positions does.
	lastBar map[string]domain.Bar

	barsProcessed int
	totalBars     int
	sampledDays   int
	firstTS       time.Time
	lastTS        time.Time
}

// New assembles an engine from cfg and a bar feed. It loads bars, registers
// instruments in cfg.Tickers order, builds the strategies, and wires the
// event loop. It does not run; call Run.
func New(ctx context.Context, cfg Config, feed BarFeed) (*Engine, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	profile := cfg.Profile
	if profile == "" {
		profile = ProfileNautilusCompat
	}

	loop := core.NewLoop()
	bus := core.NewMsgBus()
	acct := accounting.NewAccount(cfg.StartingBalance, bus)
	smplr := accounting.NewEquitySampler(acct)
	rec := NewRecorder()
	bus.SubscribeFills(rec)
	bus.SubscribeAccountState(rec)

	model, err := buildModel(profile, cfg.Realistic)
	if err != nil {
		return nil, err
	}

	spySym := cfg.SPYSymbol
	if spySym == "" {
		spySym = "SPY"
	}
	eng := &Engine{
		cfg:            cfg,
		loop:           loop,
		bus:            bus,
		acct:           acct,
		smplr:          smplr,
		rec:            rec,
		registrationIx: make(map[string]int),
		lastBar:        make(map[string]domain.Bar),
		gate:           cfg.Portfolio,
		ctxProv:        cfg.Context,
		spySym:         spySym,
	}
	if cfg.Context != nil {
		eng.ctxStat = portfolio.NewSharedContextState()
	}
	// Fill sink routes executor fills into the loop as FillEvents.
	sink := fillSink{eng: eng}
	eng.exec = exec.NewSimExecutor(model, sink, loop)

	// Build strategies. Real (prebuilt) strategies take precedence; otherwise
	// build the scripted parity drivers from the intent specs. The account is
	// the position reader for FLAT close sizing in either path.
	if len(cfg.PrebuiltStrategies) > 0 {
		eng.strategies = append(eng.strategies, cfg.PrebuiltStrategies...)
	} else {
		for _, spec := range cfg.Strategies {
			st, err := NewScriptedStrategy(spec.ID, spec.Intents, accountPositionReader{acct})
			if err != nil {
				return nil, fmt.Errorf("engine: building strategy %q: %w", spec.ID, err)
			}
			eng.strategies = append(eng.strategies, st)
		}
	}
	// Index the context consumers (strategies that read per-bar regime / market
	// cap / earnings) so the bar handler can inject before OnBar.
	for _, st := range eng.strategies {
		if cc, ok := st.(ContextConsumer); ok {
			eng.ctxCons = append(eng.ctxCons, cc)
		}
	}

	// OUT-OF-BAND warmup priming (Python SEPAUniverseRunner.warmup_ticker,
	// multi_strategy_backtest.py:645-653): before the event loop runs, prime
	// every WarmupConsumer strategy from the pre-window history WITHOUT submitting
	// orders or emitting equity samples. SEPA implements WarmupConsumer; Pairs and
	// SectorRotation do not, so they receive NO warmup (faithful asymmetry). The
	// engine then replays ONLY [Start, End] (the bars the feed loaded). SPY regime
	// warmup is handled by the ContextProvider's own full SPY history, not here.
	eng.primeWarmup()

	// Load and register instruments deterministically in ticker order.
	instruments, err := feed.Load(ctx, cfg.Tickers, cfg.Start, cfg.End)
	if err != nil {
		return nil, fmt.Errorf("engine: loading bars: %w", err)
	}
	for i, ib := range instruments {
		eng.registration = append(eng.registration, ib.Symbol)
		eng.registrationIx[ib.Symbol] = i
	}

	// Register handlers.
	loop.Register(core.KindBar, core.HandlerFunc(eng.handleBar))
	loop.Register(core.KindFill, core.HandlerFunc(eng.handleFill))
	loop.Register(core.KindSample, core.HandlerFunc(eng.handleSample))

	// Seed the queue: schedule every bar and one sample per unique timestamp.
	if err := eng.seed(instruments); err != nil {
		return nil, err
	}
	return eng, nil
}

// seed schedules all BarEvents (in registration order so same-ts bars carry
// ascending seq) and one SampleEvent per unique timestamp.
func (e *Engine) seed(instruments []InstrumentBars) error {
	// Collect the set of unique timestamps for daily sampling.
	tsSet := make(map[int64]time.Time)
	// To keep same-timestamp bars in registration order, schedule instrument by
	// instrument in registration order; within an instrument bars are ascending.
	for _, ib := range instruments {
		for _, bar := range ib.Bars {
			if _, err := e.loop.Schedule(core.BarEvent{Bar: bar}); err != nil {
				return fmt.Errorf("engine: scheduling %s bar at %s: %w", bar.Symbol, bar.TS, err)
			}
			tsSet[bar.TS.UnixNano()] = bar.TS
			e.totalBars++
		}
	}
	// Schedule one sample per unique ts (KindSample sorts after bars/fills).
	times := make([]time.Time, 0, len(tsSet))
	for _, t := range tsSet {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for _, t := range times {
		if _, err := e.loop.Schedule(core.SampleEvent{TS: t}); err != nil {
			return fmt.Errorf("engine: scheduling sample at %s: %w", t, err)
		}
	}
	if len(times) > 0 {
		e.firstTS = times[0]
		e.lastTS = times[len(times)-1]
	}
	return nil
}

// Run drives the loop to completion (or ctx cancellation) and returns the
// Result. It emits the initial account-state at the first timestamp before
// processing, mirroring the Nautilus venue's run-start event.
func (e *Engine) Run(ctx context.Context) (*Result, error) {
	initTS := e.firstTS
	if initTS.IsZero() {
		initTS = dateMidnightUTC(e.cfg.Start)
	}
	if err := e.acct.EmitInitialState(initTS); err != nil {
		return nil, fmt.Errorf("engine: emitting initial account state: %w", err)
	}
	if err := e.loop.Run(ctx); err != nil {
		return nil, fmt.Errorf("engine: run: %w", err)
	}
	// Defensive: a SampleEvent fires (and flushes this-bar fills) for every ts
	// that has bars, so the loop normally drains all this-bar orders. Flush once
	// more in case a feed scheduled bars at a ts with no sample, so no order is
	// stranded unfilled at end-of-run (the order would otherwise vanish silently).
	if _, err := e.exec.FlushThisBar(); err != nil {
		return nil, fmt.Errorf("engine: final this-bar flush: %w", err)
	}
	// End-of-run liquidation: mirror Nautilus's on_stop close_all_positions, which
	// EVERY Python runner implements (pairs/sector/sepa nautilus_runner on_stop ->
	// close_all_positions(instrument_id)). After the last bar, flatten every open
	// net position with a market order filled at the symbol's last close, so the
	// settled cash / total PnL / final NAV reflect the liquidation exactly as
	// Python does. Without this, a position open at a fold's terminal bar leaves
	// its exit unrealized in Go's settled cash, diverging per-fold final_balance /
	// total_pnl from Python by the open position's mark-to-exit (FIXER round-1
	// finding 1). This runs AFTER the final equity sample (taken during the loop
	// at the terminal bar, marking the open position to the same close the
	// liquidation realizes at), so it settles cash without emitting an extra
	// sample — the last NAV point already equals the liquidation value.
	if err := e.liquidateOpenPositions(); err != nil {
		return nil, err
	}
	return e.buildResult()
}

// handleBar dispatches one bar. For the active fill timing it orders the
// account mark, strategy callbacks and executor processing correctly (see the
// package doc above).
func (e *Engine) handleBar(_ context.Context, ev core.Event) error {
	be, ok := ev.(core.BarEvent)
	if !ok {
		return fmt.Errorf("engine: KindBar handler got %T", ev)
	}
	bar := be.Bar
	e.barsProcessed++
	e.bus.PublishBar(bar)

	nextBar := e.exec.Model().Timing() == exec.FillNextBar
	if nextBar {
		// Fill orders pending from prior bars against this bar's open FIRST.
		if _, err := e.exec.ProcessBar(bar); err != nil {
			return err
		}
	}
	// Mark-to-market: record the bar close as the symbol's last price.
	e.acct.ObserveBar(bar)
	// Track the symbol's last bar (close + volume) for end-of-run liquidation.
	e.lastBar[bar.Symbol] = bar

	// Context refresh (multi-strategy path): on the SPY heartbeat bar, advance
	// the look-ahead-safe provider and push the resulting regime / market-cap /
	// earnings snapshot into every ContextConsumer strategy (mirroring the
	// Python Context Actors publishing on the SPY bar; the SG setters persist
	// the value until the next update). SPY is registered FIRST so same-date
	// stock bars dispatched after it already see today's context.
	if e.ctxProv != nil && bar.Symbol == e.spySym {
		e.ctxProv.OnBar(e.ctxStat, bar.TS)
		e.injectContext(bar.TS)
	}

	// Run strategies in registration order (deterministic).
	sub := orderSubmitter{eng: e}
	for _, st := range e.strategies {
		if err := st.OnBar(sub, bar); err != nil {
			return fmt.Errorf("engine: strategy %s on bar %s@%s: %w", st.ID(), bar.Symbol, bar.TS, err)
		}
	}
	if !nextBar {
		// This-bar model: RECORD this bar for the end-of-timestamp flush. Orders
		// submitted during this timestamp's bars are filled together by
		// FlushThisBar (handleSample / Run end) against each order's own symbol's
		// close at this ts — supporting cross-symbol same-ts fills (e.g. both
		// Pairs legs). ProcessBar emits no fills in this-bar mode.
		if _, err := e.exec.ProcessBar(bar); err != nil {
			return err
		}
	}
	if e.cfg.Progress != nil {
		e.cfg.Progress(e.barsProcessed, e.totalBars)
	}
	return nil
}

// injectContext snapshots the current shared context state and pushes it into
// every ContextConsumer strategy. asOf is the heartbeat bar's timestamp (carried
// for telemetry; the values themselves come from the shared state the provider
// just wrote). The snapshot maps carry ONLY the symbols that have a published
// value, so a consumer for a symbol without context keeps its prior value
// (matching the Actors only calling set_* on transitions).
func (e *Engine) injectContext(asOf time.Time) {
	if len(e.ctxCons) == 0 || e.ctxStat == nil {
		return
	}
	ctx := StrategyContext{
		Regime:           e.ctxStat.Regime(),
		AsOf:             asOf,
		MarketCapUSD:     e.ctxStat.MarketCapFloats(),
		EarningsBlackout: e.ctxStat.EarningsBlackouts(),
	}
	for _, cc := range e.ctxCons {
		cc.InjectContext(ctx)
	}
}

// primeWarmup feeds the out-of-band pre-window history into every
// WarmupConsumer strategy, once per (symbol, strategy). It runs BEFORE the loop:
// no executor, no account mutation, no sampling — pure indicator/history
// priming. A nil Warmup config (the default, incl. the P2 parity path) is a
// no-op, so warmup defaults to 0 unless explicitly configured. Each consumer
// self-filters by symbol (SEPA primes only its own symbol; Pairs/Sector are not
// WarmupConsumers and never reach here), so offering every warmup symbol to
// every consumer is correct and order-independent.
func (e *Engine) primeWarmup() {
	if e.cfg.Warmup == nil || len(e.cfg.Warmup.Bars) == 0 {
		return
	}
	// Deterministic order over the warmup symbols (priming is per-symbol
	// independent, but a stable order keeps any future logging reproducible).
	syms := make([]string, 0, len(e.cfg.Warmup.Bars))
	for sym := range e.cfg.Warmup.Bars {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, st := range e.strategies {
		wc, ok := st.(WarmupConsumer)
		if !ok {
			continue
		}
		for _, sym := range syms {
			wc.WarmupBars(sym, e.cfg.Warmup.Bars[sym])
		}
	}
}

// liquidateOpenPositions flattens every open net position at end-of-run,
// mirroring Nautilus's on_stop close_all_positions(instrument_id) (implemented by
// EVERY Python runner: pairs/sector_rotation/sepa nautilus_runner.py on_stop).
// For each open position (iterated in deterministic (strategy, symbol) order, the
// same order Nautilus closes bar_types) it submits a FLAT-closing market order —
// opposite side, abs qty — attributed to the position's owning strategy, and
// fills it at the symbol's LAST observed bar (real close + volume), so the fill
// reproduces the same depth-walk (nautilus-compat) or slippage/commission
// (realistic) the in-loop path applies. The closing order is recorded (so it
// counts toward num_orders/num_filled) and settled synchronously via the
// executor's sink (mutating cash + flattening the position). No equity sample is
// emitted: the final SampleEvent already fired during the loop at the terminal
// bar, marking the open position to the same close the liquidation realizes at,
// so the last NAV point already equals the liquidated value (the curve length —
// one point per test-window trading day — is preserved). A position whose symbol
// has no recorded last bar (never observed) is skipped (it cannot be priced).
func (e *Engine) liquidateOpenPositions() error {
	for _, pos := range e.acct.OpenPositions() {
		side, ok := domain.CloseSideFor(pos.SignedQty)
		if !ok {
			continue // flat (defensive; OpenPositions excludes flats)
		}
		qty, err := pos.SignedQty.Abs()
		if err != nil {
			return fmt.Errorf("engine: liquidation qty for %s/%s: %w", pos.StrategyID, pos.Symbol, err)
		}
		bar, ok := e.lastBar[pos.Symbol]
		if !ok {
			continue // never observed a bar for this symbol; cannot price the close
		}
		coid := e.exec.NewClientOrderID()
		order := domain.NewMarketOrder(coid, pos.StrategyID, pos.Symbol, side, qty,
			"end-of-run liquidation", bar.TS)
		e.rec.RecordOrder(order)
		if err := e.exec.FillAtBar(order, bar); err != nil {
			return fmt.Errorf("engine: liquidating %s/%s: %w", pos.StrategyID, pos.Symbol, err)
		}
	}
	return nil
}

// TotalBars returns the number of bars scheduled for this run (available after
// New, before Run).
func (e *Engine) TotalBars() int { return e.totalBars }

// EquityFloat returns the value the strategy generators size against: the
// account's SETTLED cash balance (= starting + realized = Nautilus
// account(VENUE).balance_total(USD)), NOT cash + unrealized. The Python
// equity_provider on every runner reads balance_total, which does not fold open
// positions' unrealized PnL into the balance; sizing against Equity() instead
// would compute different leg quantities once any position carries unrealized
// PnL, diverging the P&L and objective from Python (FIXER round-3 finding 1).
// Returns the starting balance on the (never-expected) error path so a sizing
// closure built over it degrades gracefully rather than panicking. Strategy
// assemblers bind this as the generators' EquityProvider so sizing reflects the
// live settled book.
func (e *Engine) EquityFloat() float64 {
	return e.acct.CashFloat()
}

// handleFill settles a FillEvent popped from the queue. The engine's own
// executor settles fills synchronously via fillSink (so same-bar position reads
// are correct), so this handler is reached only if an EXTERNAL scheduler
// injects KindFill events. It is kept registered for that forward-compat path
// and applies identical settlement semantics.
func (e *Engine) handleFill(_ context.Context, ev core.Event) error {
	fe, ok := ev.(core.FillEvent)
	if !ok {
		return fmt.Errorf("engine: KindFill handler got %T", ev)
	}
	if _, _, err := e.acct.ApplyFill(fe.Fill); err != nil {
		return fmt.Errorf("engine: settling fill %s: %w", fe.Fill.TradeID, err)
	}
	// Publish the fill to observers (recorder) AFTER settlement so position
	// state is consistent for any observer that reads the account.
	e.bus.PublishFill(fe.Fill)
	return nil
}

// handleSample flushes this-timestamp's this-bar fills, THEN takes one equity
// sample for the day. The SampleEvent fires once per unique timestamp, AFTER all
// KindBar events at that ts (KindSample has the lowest priority), so by the time
// it runs every same-ts bar has been observed and every strategy on_bar has run.
// Flushing here therefore fills all orders submitted during the timestamp against
// their own symbols' closes (cross-symbol same-ts fills) and settles them BEFORE
// the equity sample, so the sample reflects today's fills. No-op for the next-bar
// model (already filled in ProcessBar).
func (e *Engine) handleSample(_ context.Context, ev core.Event) error {
	se, ok := ev.(core.SampleEvent)
	if !ok {
		return fmt.Errorf("engine: KindSample handler got %T", ev)
	}
	if _, err := e.exec.FlushThisBar(); err != nil {
		return fmt.Errorf("engine: flushing this-bar fills at %s: %w", se.TS, err)
	}
	if err := e.smplr.Sample(se.TS); err != nil {
		return fmt.Errorf("engine: sampling at %s: %w", se.TS, err)
	}
	e.sampledDays++
	return nil
}

// buildResult assembles the final Result from the engine state.
//
// FinalBalance / TotalPnL come from the SETTLED cash balance (= Nautilus
// account.balance_total(USD)), NOT the mark-to-market equity. The reference
// run_backtest computes final_balance_usd = balance_total and
// total_pnl_usd = final_balance - starting (multi_strategy_backtest.py:677-679),
// so an open position's unrealized PnL does NOT count toward final_balance_usd /
// total_pnl_usd (it only shows up in the per-bar equity curve, which the sampler
// captures separately as cash + unrealized). Using Equity() here would inflate
// these counters by the live unrealized PnL and diverge from Python (FIXER
// round-3 finding 1). This also keeps FinalBalance consistent with the last
// AccountState.Total (account.json), which already records cash per fill.
func (e *Engine) buildResult() (*Result, error) {
	final, err := e.acct.Cash()
	if err != nil {
		return nil, err
	}
	pnl, err := final.Sub(e.cfg.StartingBalance)
	if err != nil {
		return nil, fmt.Errorf("engine: total pnl: %w", err)
	}
	// Distinct logical strategy ids (prebuilt SEPA runs many per-symbol instances
	// under one id; dedup so the result lists each logical strategy once).
	idSet := make(map[string]struct{}, len(e.strategies))
	strategyIDs := make([]string, 0, len(e.strategies))
	for _, st := range e.strategies {
		if _, seen := idSet[st.ID()]; seen {
			continue
		}
		idSet[st.ID()] = struct{}{}
		strategyIDs = append(strategyIDs, st.ID())
	}
	sort.Strings(strategyIDs)

	stratEquity := make(map[string][]accounting.EquityPoint)
	for _, sid := range e.smplr.StrategyIDs() {
		stratEquity[sid] = e.smplr.StrategyCurve(sid)
	}

	return &Result{
		StartingBalance:  e.cfg.StartingBalance,
		FinalBalance:     final,
		TotalPnL:         pnl,
		Profile:          e.profile(),
		Strategies:       strategyIDs,
		Orders:           e.rec.Orders(),
		Fills:            e.rec.Fills(),
		RejectedOrders:   e.rejected,
		Positions:        e.acct.AllPositions(),
		AccountStates:    e.rec.AccountStates(),
		TotalEquityCurve: e.smplr.TotalCurve(),
		StrategyEquity:   stratEquity,
		BarsProcessed:    e.barsProcessed,
		SampledDays:      e.sampledDays,
		FirstTS:          e.firstTS,
		LastTS:           e.lastTS,
	}, nil
}

func (e *Engine) profile() FillProfile {
	if e.cfg.Profile == "" {
		return ProfileNautilusCompat
	}
	return e.cfg.Profile
}

// ---------------------------------------------------------------------------
// internal adapters
// ---------------------------------------------------------------------------

// orderSubmitter routes strategy order submissions to the executor with a
// deterministic client order id, records the submitted order, and (for signal
// orders) runs the pre-trade portfolio gate.
type orderSubmitter struct{ eng *Engine }

func (s orderSubmitter) SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error) {
	coid := s.eng.exec.NewClientOrderID()
	order := domain.NewMarketOrder(coid, strategyID, symbol, side, qty, reason, ts)
	if err := s.eng.exec.Submit(order); err != nil {
		return "", err
	}
	s.eng.rec.RecordOrder(order)
	return coid, nil
}

// NetPosition exposes the live cross-strategy net position to strategies that
// FLAT-close by reading the venue book (engine.PositionReader). The real
// strategy adapters (SEPA / Pairs) translate a FLAT signal into a closing market
// order sized from the venue net position — the Python runners read
// self.portfolio.net_position(instrument_id) (a NETTING-OMS venue net across
// strategies), so the engine submitter must net across strategies too. WITHOUT
// this, sub.(engine.PositionReader) failed the type assertion and FLAT closes
// silently sized to 0 (a no-op), so a Pairs/SEPA position once opened never
// closed in the integrated path — diverging the equity curve from Python (the
// P3 gap the parity_backtest regression test pins down). strategyID is ignored
// (venue net is cross-strategy, §7.4), matching accountPositionReader.
func (s orderSubmitter) NetPosition(_ string, symbol string) domain.Qty {
	snap, err := s.eng.acct.Snapshot()
	if err != nil {
		return 0
	}
	net, err := snap.NetPositionAcrossStrategies(symbol)
	if err != nil {
		return 0
	}
	return net
}

// SubmitMarketSignal runs the pre-trade portfolio gate (when configured) for a
// strategy signal before submitting. It mirrors src/strategies/_base/runner.py
// :_gate + maybe_check_portfolio: build a ProposedOrder (strategy id, symbol,
// signalSide, abs qty, estimated price = the symbol's last close), snapshot the
// account, and run portfolio.Check. FLAT and qty<=0 bypass the gate inside the
// pipeline (closes always proceed, even during a daily-loss halt). On a
// rejection it records the rejection and returns submitted=false WITHOUT
// placing an order (the runner skips the submit). A nil gate always submits.
func (s orderSubmitter) SubmitMarketSignal(strategyID, symbol string, signalSide domain.SignalSide, orderSide domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	if s.eng.gate != nil && signalSide != domain.SideFlat && qty > 0 {
		// Estimated fill price = the symbol's last observed close (the reference
		// _gate reads self._last_close.get(symbol, Decimal(0))). The engine has
		// already ObserveBar'd this bar's close before strategies run, so the
		// current bar's close is the price — matching Python, which sets
		// _last_close to the current bar at the top of on_bar.
		price, _ := s.eng.acct.LastPrice(symbol) // zero Price when unknown == Decimal(0)
		snap, err := s.eng.acct.Snapshot()
		if err != nil {
			return "", false, fmt.Errorf("engine: gate snapshot for %s/%s: %w", strategyID, symbol, err)
		}
		proposed := portfolio.NewProposedOrder(strategyID, symbol, signalSide, qty, price, ts)
		decision := s.eng.gate.Check(proposed, portfolio.SnapshotFromDomain(snap))
		if !decision.Approved {
			s.eng.rejected = append(s.eng.rejected, RejectedOrder{
				StrategyID: strategyID,
				Symbol:     symbol,
				SignalSide: signalSide,
				Qty:        qty,
				RuleName:   decision.RuleName,
				Reason:     decision.Reason,
				TS:         ts,
			})
			return "", false, nil
		}
	}
	coid, err := s.SubmitMarket(strategyID, symbol, orderSide, qty, reason, ts)
	if err != nil {
		return "", false, err
	}
	return coid, true, nil
}

// fillSink settles executor fills synchronously on the loop goroutine: the
// account is mutated and the fill recorded immediately, so a strategy reading
// its net position later in the SAME bar dispatch sees the up-to-date book.
// This matches Nautilus, which settles each data point's venue before the next
// (engine.pyx:1627-1663) — a market order's fill is visible before subsequent
// strategy reads. Settling inline (rather than deferring a KindFill event past
// the bar handler) is what makes next-bar FLAT close-sizing correct.
//
// The KindFill event kind and Engine.handleFill remain registered for
// forward-compatibility (an external scheduler may inject fills), but the
// engine's own executor settles here.
type fillSink struct{ eng *Engine }

func (s fillSink) EmitFill(f domain.Fill) error {
	if _, _, err := s.eng.acct.ApplyFill(f); err != nil {
		return fmt.Errorf("engine: settling fill %s: %w", f.TradeID, err)
	}
	s.eng.bus.PublishFill(f)
	return nil
}

// accountPositionReader adapts the account to the PositionReader the strategy
// uses for FLAT close sizing. It nets across ALL strategies for the symbol
// (cross-strategy net, §7.4 net_position semantics).
type accountPositionReader struct{ acct *accounting.Account }

func (r accountPositionReader) NetPosition(_ string, symbol string) domain.Qty {
	snap, err := r.acct.Snapshot()
	if err != nil {
		return 0
	}
	net, err := snap.NetPositionAcrossStrategies(symbol)
	if err != nil {
		return 0
	}
	return net
}

// ---------------------------------------------------------------------------
// config + model construction
// ---------------------------------------------------------------------------

func validateConfig(cfg Config) error {
	if len(cfg.Tickers) == 0 {
		return fmt.Errorf("%w: config has no tickers", domain.ErrInvalidArgument)
	}
	seen := make(map[string]struct{}, len(cfg.Tickers))
	for _, t := range cfg.Tickers {
		if t == "" {
			return fmt.Errorf("%w: config has an empty ticker", domain.ErrInvalidArgument)
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("%w: config has duplicate ticker %q", domain.ErrInvalidArgument, t)
		}
		seen[t] = struct{}{}
	}
	if cfg.Start.IsZero() || cfg.End.IsZero() {
		return fmt.Errorf("%w: config needs start and end dates", domain.ErrInvalidArgument)
	}
	if cfg.End.Before(cfg.Start) {
		return fmt.Errorf("%w: config end %s before start %s", domain.ErrInvalidArgument, cfg.End, cfg.Start)
	}
	if cfg.StartingBalance <= 0 {
		return fmt.Errorf("%w: config starting balance must be positive, got %s",
			domain.ErrInvalidArgument, cfg.StartingBalance)
	}
	if cfg.Profile != "" && !cfg.Profile.IsValid() {
		return fmt.Errorf("%w: unknown fill profile %q", domain.ErrInvalidArgument, cfg.Profile)
	}
	if len(cfg.Strategies) == 0 && len(cfg.PrebuiltStrategies) == 0 {
		return fmt.Errorf("%w: config has no strategies", domain.ErrInvalidArgument)
	}
	if len(cfg.Strategies) > 0 && len(cfg.PrebuiltStrategies) > 0 {
		return fmt.Errorf("%w: config sets both Strategies and PrebuiltStrategies (supply exactly one)", domain.ErrInvalidArgument)
	}
	ids := make(map[string]struct{}, len(cfg.Strategies)+len(cfg.PrebuiltStrategies))
	for _, s := range cfg.Strategies {
		if s.ID == "" {
			return fmt.Errorf("%w: config has a strategy with empty id", domain.ErrInvalidArgument)
		}
		if _, dup := ids[s.ID]; dup {
			return fmt.Errorf("%w: config has duplicate strategy id %q", domain.ErrInvalidArgument, s.ID)
		}
		ids[s.ID] = struct{}{}
	}
	// Prebuilt strategies MAY share a logical engine id: the SEPA universe path
	// runs one per-symbol generator instance per stock, all under the single
	// allocator key "SEPA-UNIVERSE-001" (mirroring the Python SEPAUniverseRunner,
	// one strategy id managing N SignalGenerators). The allocator budget and the
	// positions book are keyed by (strategy_id, symbol), so a shared id across
	// distinct symbols is correct, not a collision. We therefore only reject
	// empty ids here, not duplicates.
	for _, s := range cfg.PrebuiltStrategies {
		if s == nil {
			return fmt.Errorf("%w: config has a nil prebuilt strategy", domain.ErrInvalidArgument)
		}
		if s.ID() == "" {
			return fmt.Errorf("%w: config has a prebuilt strategy with empty id", domain.ErrInvalidArgument)
		}
	}
	return nil
}

func buildModel(profile FillProfile, rp RealisticParams) (exec.FillModel, error) {
	switch profile {
	case ProfileNautilusCompat:
		return exec.NautilusCompatModel{}, nil
	case ProfileRealistic:
		return exec.RealisticModel{
			SlippageBps:        rp.SlippageBps,
			CommissionPerShare: rp.CommissionPerShare,
			CommissionBps:      rp.CommissionBps,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unknown fill profile %q", domain.ErrInvalidArgument, profile)
	}
}

// dateMidnightUTC returns the UTC-midnight instant of a calendar date — the
// storage convention for daily bar timestamps.
func dateMidnightUTC(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// compile-time interface checks.
var (
	_ OrderSubmitter = orderSubmitter{}
	_ PositionReader = orderSubmitter{}
	_ exec.FillSink  = fillSink{}
	_ PositionReader = accountPositionReader{}
	_ core.Clock     = (*core.SimClock)(nil)
	_ core.Clock     = core.WallClock{}
	_ core.Clock     = (*core.VirtualClock)(nil)
)
