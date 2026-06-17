package livetrade_test

// session_test.go drives the full paper-trading session end-to-end against the
// in-memory mock trading venue (no OpenD, no PG): it proves the wiring required
// by the P6 acceptance gate —
//
//   - paper session order lifecycle end-to-end (signal -> gate -> PlaceOrder ->
//     accept/fill push -> accounting + fill sink);
//   - gate rejection suppresses the order + records a risk event;
//   - reconciliation mismatch detection (broker vs strategy books);
//   - crash-recovery resume (restore broker positions into the book);
//   - flatten closes all open positions;
//   - daily-loss-halt latches + rejects new opens while FLAT still passes.
//
// The mock venue delivers pushes synchronously on the calling goroutine, so the
// lifecycle is deterministic and assertable without a clock race.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

const paperAcc = uint64(99001)

// signalStrategy is a test strategy that submits ONE LONG signal per (date,
// symbol) via SubmitMarketSignal — the gated path the real adapters use (so the
// pre-submit portfolio gate runs). FLAT intents route through SubmitMarket
// (close path) sized from the live net position.
type signalStrategy struct {
	id    string
	longs map[string]map[string]domain.Qty // date(YYYY-MM-DD) -> symbol -> qty
	flats map[string]map[string]bool       // date -> symbol -> close
}

func dkey(ts time.Time) string { return ts.UTC().Format("2006-01-02") }

func (s *signalStrategy) ID() string { return s.id }

func (s *signalStrategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	d := dkey(bar.TS)
	if qty, ok := s.longs[d][bar.Symbol]; ok && qty > 0 {
		_, _, err := sub.SubmitMarketSignal(s.id, bar.Symbol, domain.SideLong, domain.OrderSideBuy, qty,
			"test long", bar.TS)
		if err != nil {
			return err
		}
	}
	if s.flats[d][bar.Symbol] {
		pr, _ := sub.(engine.PositionReader)
		var net domain.Qty
		if pr != nil {
			net = pr.NetPosition(s.id, bar.Symbol)
		}
		side, ok := domain.CloseSideFor(net)
		if !ok {
			return nil
		}
		abs := net
		if abs < 0 {
			abs = -abs
		}
		_, _, err := sub.SubmitMarketSignal(s.id, bar.Symbol, domain.SideFlat, side, abs, "test flat", bar.TS)
		return err
	}
	return nil
}

// memRisk records gate decisions in memory.
type memRisk struct {
	mu        sync.Mutex
	decisions []livetrade.GateDecision
}

func (m *memRisk) RecordGateDecision(_ context.Context, d livetrade.GateDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, d)
	return nil
}
func (m *memRisk) rejections() []livetrade.GateDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []livetrade.GateDecision
	for _, d := range m.decisions {
		if !d.Approved {
			out = append(out, d)
		}
	}
	return out
}

// fillSink captures fills the executor feeds the engine.
type fillSink struct {
	mu    sync.Mutex
	fills []domain.Fill
}

func (s *fillSink) EmitFill(f domain.Fill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fills = append(s.fills, f)
	return nil
}
func (s *fillSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.fills) }

// memHealth captures post-timestamp live snapshots.
type memHealth struct {
	mu    sync.Mutex
	snaps []livetrade.LiveSnapshot
}

func (m *memHealth) EmitLiveHealth(_ context.Context, s livetrade.LiveSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps = append(m.snaps, s)
	return nil
}
func (m *memHealth) last() (livetrade.LiveSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.snaps) == 0 {
		return livetrade.LiveSnapshot{}, false
	}
	return m.snaps[len(m.snaps)-1], true
}

// harness bundles a built paper trade session + its venue + sinks.
type harness struct {
	venue   *moexec.MockVenue
	exec    *moexec.MoomooExecutor
	account *livetrade.AccountAdapter
	acct    *accounting.Account
	halt    *commands.HaltState
	risk    *memRisk
	sink    *fillSink
	health  *memHealth
	ts      *livetrade.TradeSession
}

// gate100 builds a single-strategy gate giving the strategy 100% of NAV budget
// with the default risk constraints (10% daily-loss halt, 50% single-name, 40%
// concentration). The budget is large enough that normal opens pass.
func gate100(t *testing.T, id string, haltPct float64) *riskgate.Gate {
	t.Helper()
	alloc, err := riskgate.NewAllocator([]riskgate.StrategyAllocation{
		{StrategyID: id, CapitalPct: 1.0},
	})
	require.NoError(t, err)
	rc, err := riskgate.NewRiskConstraints(riskgate.RiskConstraintsConfig{
		DailyLossHaltPct: haltPct,
		MaxSingleNamePct: 0.50,
		ConcentrationPct: 0.40,
	})
	require.NoError(t, err)
	return riskgate.NewGate(alloc, rc)
}

func newHarness(t *testing.T, strat engine.Strategy, gate *riskgate.Gate, navUSD float64) *harness {
	t.Helper()
	ctx := context.Background()
	venue := moexec.NewMockVenue(paperAcc)
	acct := accounting.NewAccount(domain.MustMoney(ftoa(navUSD)), nil)
	account := livetrade.NewAccountAdapter(acct)
	sink := &fillSink{}
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAcc, "paper")
	exec, err := moexec.New(ctx, moexec.Config{
		Account:  paperAcct,
		Client:   venue,
		TraderID: "PAPER-TEST-001",
		Sink:     sink,
		Book:     account,
	})
	require.NoError(t, err)

	halt := commands.NewHaltState(nil)
	risk := &memRisk{}
	health := &memHealth{}
	ts, err := livetrade.NewTradeSession(livetrade.TradeSessionConfig{
		Acct:       paperAcct,
		Strategies: []engine.Strategy{strat},
		Gate:       gate,
		Account:    account,
		Executor:   exec,
		Halt:       halt,
		Risk:       risk,
		NAV:        domain.MustMoney(ftoa(navUSD)),
		EmitGate:   halt.Emitting,
		HealthSink: health,
	})
	require.NoError(t, err)
	return &harness{
		venue: venue, exec: exec, account: account, acct: acct,
		halt: halt, risk: risk, sink: sink, health: health, ts: ts,
	}
}

// run drives the session over bars with a virtual clock, then flushes.
func (h *harness) run(t *testing.T, bars []domain.Bar) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.ts.Prime(ctx))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, h.ts.Session().RunStream(ctx,
		livengine.SliceStreamFeed{Bars: bars}, core.StreamVirtual, vc))
}

func bar(sym string, day int, px float64) domain.Bar {
	p := domain.MustPrice(ftoa(px))
	ts := time.Date(2024, time.January, day, 0, 0, 0, 0, time.UTC)
	return domain.Bar{Symbol: sym, TS: ts, Open: p, High: p, Low: p, Close: p, Volume: 1000}
}

// driveAccepts pushes accept+fill for every tracked non-terminal order at price.
// The mock venue delivers pushes synchronously, settling accounting inline.
func (h *harness) acceptFillAll(t *testing.T, price float64) {
	t.Helper()
	px := domain.MustPrice(ftoa(price))
	for _, o := range h.exec.TrackedOrders() {
		if o.Status == domain.OrderStatusFilled || o.Status == domain.OrderStatusRejected {
			continue
		}
		require.NoError(t, h.venue.Accept(o.ClientOrderID))
		require.NoError(t, h.venue.Fill(o.ClientOrderID, px))
	}
}

// ftoa formats a USD float to a 2-dp string for MustMoney/MustPrice.
func ftoa(v float64) string {
	// 2-dp is enough for the test universe (whole-dollar prices / round NAVs).
	neg := v < 0
	if neg {
		v = -v
	}
	cents := int64(v*100 + 0.5)
	whole := cents / 100
	frac := cents % 100
	s := itoa(whole) + "." + pad2(frac)
	if neg {
		s = "-" + s
	}
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func pad2(n int64) string {
	s := itoa(n)
	if len(s) < 2 {
		s = "0" + s
	}
	return s
}

// --- tests ---

// TestPaperOrderLifecycle: a LONG signal passes the gate, places a market order,
// the venue accepts + fills it, and accounting + the fill sink record the fill.
func TestPaperOrderLifecycle(t *testing.T) {
	t.Parallel()
	strat := &signalStrategy{
		id:    "TEST-001",
		longs: map[string]map[string]domain.Qty{"2024-01-02": {"AAPL": 100}},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)

	h.run(t, []domain.Bar{bar("AAPL", 2, 150)})

	// The strategy submitted one order through the gate.
	orders := h.exec.TrackedOrders()
	require.Len(t, orders, 1, "one order placed")
	assert.Equal(t, domain.OrderStatusSubmitted, orders[0].Status, "submitted, awaiting venue push")
	assert.Zero(t, len(h.risk.rejections()), "no gate rejection for a within-budget open")

	// Drive the venue: accept + fill.
	h.acceptFillAll(t, 150)

	pos, ok := h.account.Position("TEST-001", "AAPL")
	require.True(t, ok)
	assert.Equal(t, domain.Qty(100), pos.SignedQty, "100 long after fill")
	assert.Equal(t, 1, h.sink.count(), "fill fed to the engine sink")
	assert.Equal(t, int64(1), h.ts.GatedSubmitter().SubmittedCount())
}

// TestGateRejection: an oversized LONG (notional > budget) is rejected by the
// allocator budget; no order is placed + a risk event is recorded.
func TestGateRejection(t *testing.T) {
	t.Parallel()
	// 100000 NAV, budget 100%. A 100000-share order at the gate's estimated price
	// (the bar close 150) is 15,000,000 notional >> budget -> allocator rejects.
	strat := &signalStrategy{
		id:    "TEST-001",
		longs: map[string]map[string]domain.Qty{"2024-01-02": {"AAPL": 100000}},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)

	h.run(t, []domain.Bar{bar("AAPL", 2, 150)})

	assert.Empty(t, h.exec.TrackedOrders(), "rejected order never reaches the venue")
	rej := h.risk.rejections()
	require.Len(t, rej, 1, "one gate rejection recorded")
	assert.Contains(t, rej[0].RuleName, "allocator", "allocator budget rule")
	assert.Equal(t, int64(1), h.ts.GatedSubmitter().RejectedCount())
	assert.Zero(t, h.sink.count(), "no fill")
}

// TestDailyLossHaltRejectsNewOpens: after a loss drives day P&L below -10% NAV,
// the halt latches and a subsequent NEW LONG is rejected, while a FLAT close on
// the existing position still passes.
func TestDailyLossHaltRejectsNewOpens(t *testing.T) {
	t.Parallel()
	// Day 2: open 300 AAPL @ 100 (30k notional: within the 100k budget, the 50%
	// single-name and the 40% concentration limits).
	// Day 3: price collapses to 40 -> mark-to-market loss 300*60 = 18k = 18% NAV
	//        > 10% halt threshold. A NEW MSFT long must be rejected; a FLAT AAPL
	//        close must still pass.
	strat := &signalStrategy{
		id: "TEST-001",
		longs: map[string]map[string]domain.Qty{
			"2024-01-02": {"AAPL": 300},
			"2024-01-03": {"MSFT": 10},
		},
		flats: map[string]map[string]bool{
			"2024-01-03": {"AAPL": true},
		},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)

	ctx := context.Background()
	require.NoError(t, h.ts.Prime(ctx))
	vc := core.NewVirtualClock(time.Time{})

	// Drive day 2 bar, then settle the open at 100 BEFORE day 3 marks the loss.
	// We run the whole stream but intercept by pre-driving: simplest is to run
	// day-2 then accept/fill, then run day-3.
	day2 := []domain.Bar{bar("AAPL", 2, 100)}
	require.NoError(t, h.ts.Session().RunStream(ctx,
		livengine.SliceStreamFeed{Bars: day2}, core.StreamVirtual, vc))
	h.acceptFillAll(t, 100) // 300 AAPL long @ 100

	pos, _ := h.account.Position("TEST-001", "AAPL")
	require.Equal(t, domain.Qty(300), pos.SignedQty)

	// Day 3: price 40 (an 18% NAV unrealized loss) -> halt should latch; MSFT open
	// rejected; AAPL FLAT close still submitted.
	day3 := []domain.Bar{bar("AAPL", 3, 40), bar("MSFT", 3, 50)}
	require.NoError(t, h.ts.Session().RunStream(ctx,
		livengine.SliceStreamFeed{Bars: day3}, core.StreamVirtual, core.NewVirtualClock(time.Time{})))

	assert.True(t, h.halt.IsHalted(), "daily-loss halt latched on the loss")
	snap := h.halt.Snapshot()
	assert.Equal(t, commands.HaltDailyLoss, snap.Kind, "halt kind is daily_loss")

	// The MSFT open must have been rejected by the daily-loss-halt rule.
	var msftRejected bool
	var aaplCloseSubmitted bool
	for _, d := range h.risk.rejections() {
		if d.Symbol == "MSFT" && d.RuleName == "risk.daily_loss_halt" {
			msftRejected = true
		}
	}
	for _, o := range h.exec.TrackedOrders() {
		if o.Symbol == "AAPL" && o.Side == domain.OrderSideSell {
			aaplCloseSubmitted = true
		}
	}
	assert.True(t, msftRejected, "new MSFT open rejected by daily-loss halt")
	assert.True(t, aaplCloseSubmitted, "FLAT AAPL close still passes during the halt")

	hs, ok := h.health.last()
	require.True(t, ok)
	assert.True(t, hs.DailyLossHalted, "health snapshot reports the halt")
}

// memReport captures saved reconciliation reports.
type memReport struct {
	mu      sync.Mutex
	reports []riskgate.ReconciliationReport
}

func (m *memReport) SaveReconciliation(_ context.Context, r riskgate.ReconciliationReport, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, r)
	return nil
}

// memAlerter records reconciliation mismatch alerts (NO auto-correct).
type memAlerter struct {
	mu     sync.Mutex
	alerts int
	halt   *commands.HaltState
}

func (m *memAlerter) OnReconciliationMismatch(_ context.Context, _ riskgate.ReconciliationReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts++
	if m.halt != nil {
		m.halt.Halt(commands.HaltReconciliation, "reconciliation mismatch")
	}
}

// TestReconciliationMismatchDetection: the strategy book holds 100 AAPL but the
// broker reports 95 -> a mismatch is detected, saved, and alerted (halt + cockpit)
// WITHOUT any auto-correcting trade.
func TestReconciliationMismatchDetection(t *testing.T) {
	t.Parallel()
	strat := &signalStrategy{
		id:    "TEST-001",
		longs: map[string]map[string]domain.Qty{"2024-01-02": {"AAPL": 100}},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)
	h.run(t, []domain.Bar{bar("AAPL", 2, 150)})
	h.acceptFillAll(t, 150) // strategy book + venue both at 100

	// Force a divergence: drop the venue position to 95 (simulating a missed fill).
	h.venue.SetPosition("AAPL", 95, domain.MustPrice("150.00"))

	report := &memReport{}
	alerter := &memAlerter{halt: h.halt}
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker:  h.venue,
		Books:   h.account,
		Sink:    report,
		Alerter: alerter,
		AccID:   paperAcc,
		Env:     mo.TrdEnvSimulate,
		Now:     func() time.Time { return time.Date(2024, 1, 2, 21, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)

	r, err := rec.Reconcile(context.Background())
	require.NoError(t, err)
	require.True(t, r.HasIssues(), "drift detected")
	require.Len(t, r.Mismatches, 1)
	assert.Equal(t, "AAPL", r.Mismatches[0].Symbol)
	assert.Equal(t, int64(100), r.Mismatches[0].StrategyBooksSum)
	assert.Equal(t, int64(95), r.Mismatches[0].BrokerNet)
	assert.Equal(t, int64(-5), r.Mismatches[0].Diff, "broker_net - strategy_sum")

	assert.Len(t, report.reports, 1, "report persisted")
	assert.Equal(t, 1, alerter.alerts, "mismatch alerted")
	assert.True(t, h.halt.IsHalted(), "reconciliation mismatch halts (no auto-correct)")
	// No corrective order placed: the only order is the original AAPL open.
	for _, o := range h.exec.TrackedOrders() {
		assert.NotEqual(t, "RECONCILE", o.StrategyID, "no auto-correcting trade")
	}
}

// TestReconciliationClean: when broker and book agree, the report is clean and no
// alert fires.
func TestReconciliationClean(t *testing.T) {
	t.Parallel()
	strat := &signalStrategy{
		id:    "TEST-001",
		longs: map[string]map[string]domain.Qty{"2024-01-02": {"AAPL": 100}},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)
	h.run(t, []domain.Bar{bar("AAPL", 2, 150)})
	h.acceptFillAll(t, 150)

	alerter := &memAlerter{halt: h.halt}
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: h.venue, Books: h.account, Alerter: alerter,
		AccID: paperAcc, Env: mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	r, err := rec.Reconcile(context.Background())
	require.NoError(t, err)
	assert.False(t, r.HasIssues(), "broker == book => clean")
	assert.Equal(t, []string{"AAPL"}, r.Matched)
	assert.Zero(t, alerter.alerts)
	assert.False(t, h.halt.IsHalted())
}

// TestFlattenClosesAll: open two positions, then Flatten submits a closing market
// order per open position; after the venue fills them, the book is flat. The
// flatten is idempotent (a second call with both flat submits nothing).
func TestFlattenClosesAll(t *testing.T) {
	t.Parallel()
	strat := &signalStrategy{
		id: "TEST-001",
		longs: map[string]map[string]domain.Qty{
			"2024-01-02": {"AAPL": 100, "MSFT": 50},
		},
	}
	h := newHarness(t, strat, gate100(t, "TEST-001", 0.10), 100000)
	h.run(t, []domain.Bar{bar("AAPL", 2, 150), bar("MSFT", 2, 200)})
	h.acceptFillAll(t, 150) // fills both opens (AAPL@150, MSFT@150 — price uniform per call is fine)

	require.Equal(t, domain.Qty(100), h.account.NetPositionAcrossStrategies("AAPL"))
	require.Equal(t, domain.Qty(50), h.account.NetPositionAcrossStrategies("MSFT"))

	// A wrong confirmation phrase is refused (SAFETY).
	_, err := h.ts.Flatten(context.Background(), "nope", "kill")
	require.Error(t, err, "flatten refuses a bad confirmation phrase")

	coids, err := h.ts.Flatten(context.Background(), livetrade.FlattenConfirmationPhrase, "kill switch")
	require.NoError(t, err)
	require.Len(t, coids, 2, "one closing order per open position")

	// Fill the closing orders.
	h.acceptFillAll(t, 150)
	assert.Equal(t, domain.Qty(0), h.account.NetPositionAcrossStrategies("AAPL"), "AAPL flat")
	assert.Equal(t, domain.Qty(0), h.account.NetPositionAcrossStrategies("MSFT"), "MSFT flat")

	// Idempotent: a second flatten with everything flat submits nothing.
	coids2, err := h.ts.Flatten(context.Background(), livetrade.FlattenConfirmationPhrase, "kill again")
	require.NoError(t, err)
	assert.Empty(t, coids2, "nothing to close => no orders")
}

// TestCrashRecoveryResume: a fresh session (post-restart) restores the broker
// positions into its book via RestoreFromBroker, then reconciliation is clean —
// the node resumes with positions intact.
func TestCrashRecoveryResume(t *testing.T) {
	t.Parallel()
	// "Before the crash": a venue that already holds 100 AAPL + 50 MSFT (as if a
	// prior session opened them). A brand-new session restores from it.
	venue := moexec.NewMockVenue(paperAcc)
	venue.SetPosition("AAPL", 100, domain.MustPrice("150.00"))
	venue.SetPosition("MSFT", 50, domain.MustPrice("200.00"))

	acct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	account := livetrade.NewAccountAdapter(acct)
	sink := &fillSink{}
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAcc, "paper")
	exec, err := moexec.New(context.Background(), moexec.Config{
		Account: paperAcct, Client: venue,
		TraderID: "PAPER-TEST-001", Sink: sink, Book: account,
	})
	require.NoError(t, err)
	halt := commands.NewHaltState(nil)
	strat := &signalStrategy{id: "TEST-001"}
	ts, err := livetrade.NewTradeSession(livetrade.TradeSessionConfig{
		Acct: paperAcct, Strategies: []engine.Strategy{strat},
		Gate: gate100(t, "TEST-001", 0.10), Account: account, Executor: exec,
		Halt: halt, NAV: domain.MustMoney("100000.00"), EmitGate: halt.Emitting,
	})
	require.NoError(t, err)

	// Restore from the broker: the empty book is seeded to match the venue.
	positions, err := ts.RestoreFromBroker(context.Background())
	require.NoError(t, err)
	require.Len(t, positions, 2)
	assert.Equal(t, domain.Qty(100), account.NetPositionAcrossStrategies("AAPL"), "AAPL restored")
	assert.Equal(t, domain.Qty(50), account.NetPositionAcrossStrategies("MSFT"), "MSFT restored")

	// Reconciliation is now clean (book == broker after restore).
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: venue, Books: account, AccID: paperAcc, Env: mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	r, err := rec.Reconcile(context.Background())
	require.NoError(t, err)
	assert.False(t, r.HasIssues(), "restored book reconciles with the broker")

	// Idempotent: a second restore does not double the positions.
	_, err = ts.RestoreFromBroker(context.Background())
	require.NoError(t, err)
	assert.Equal(t, domain.Qty(100), account.NetPositionAcrossStrategies("AAPL"), "no double-seed")
}

// TestLiveActivationGateRejectsPaperMismatch proves a paper trade session refuses
// a live-bound executor and vice versa (SAFETY, decision 8): signal/paper can
// never reach the live account.
func TestLiveActivationGateRejectsPaperMismatch(t *testing.T) {
	t.Parallel()
	venue := moexec.NewMockVenue(paperAcc)
	acct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	account := livetrade.NewAccountAdapter(acct)
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAcc, "paper")
	realAcct := domain.NewBrokerAccount("moomoo", domain.EnvReal, paperAcc, "live")
	paperExec, err := moexec.New(context.Background(), moexec.Config{
		Account: paperAcct, Client: venue,
		TraderID: "PAPER-TEST-001", Sink: &fillSink{}, Book: account,
	})
	require.NoError(t, err)

	// A LIVE trade session (real Acct) backed by a PAPER executor must be refused.
	_, err = livetrade.NewTradeSession(livetrade.TradeSessionConfig{
		Acct: realAcct, Strategies: []engine.Strategy{&signalStrategy{id: "X"}},
		Account: account, Executor: paperExec, Halt: commands.NewHaltState(nil),
		NAV: domain.MustMoney("100000.00"),
	})
	require.Error(t, err, "live session refuses a paper-bound executor")
}
