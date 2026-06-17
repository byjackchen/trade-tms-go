package livetrade_test

// manual_test.go drives the operator-driven MANUAL trade desk end-to-end against
// the in-memory mock trading venue (no OpenD, no PG) and proves the safety-
// critical acceptance gate:
//
//   - manual order lifecycle: place -> accept/fill push -> MANUAL position;
//   - cancel: a working order is cancelled (terminal CANCELED) + idempotent;
//   - close: CloseManualPosition flattens the MANUAL book row;
//   - LIVE safety: a paper desk can NEVER reach the real account; a live order
//     requires the per-order confirmation phrase (no phrase => 412-equivalent
//     ErrConfirmationRequired, NO order placed); a paper order requires the trade
//     password;
//   - risk gate: an opening order that violates the budget is REJECTED unless an
//     audited override is set (override -> risk_events approved-with-rule + audit);
//   - idempotent double-submit: the same idempotency key never double-submits;
//   - audit: every place/cancel/close writes an audit record (operator/symbol/
//     side/qty/override?).
//
// The mock venue delivers pushes synchronously, so the lifecycle is deterministic.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

const (
	manualPaperAcc = uint64(99001)
	manualRealAcc  = uint64(77002)
	manualUnlock   = "unlock-pw"
	paperTradePw   = "paper-trade-pw"
)

// memAudit captures manual-action audit records.
type memAudit struct {
	mu   sync.Mutex
	recs []livetrade.ManualAuditRecord
}

func (m *memAudit) RecordManualAction(_ context.Context, a livetrade.ManualAuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, a)
	return nil
}
func (m *memAudit) all() []livetrade.ManualAuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]livetrade.ManualAuditRecord, len(m.recs))
	copy(out, m.recs)
	return out
}
func (m *memAudit) byAction(action string) []livetrade.ManualAuditRecord {
	var out []livetrade.ManualAuditRecord
	for _, r := range m.all() {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// manualHarness bundles a built manual desk + its venue + sinks.
type manualHarness struct {
	venue   *moexec.MockVenue
	exec    *moexec.MoomooExecutor
	account *livetrade.AccountAdapter
	acct    *accounting.Account
	halt    *commands.HaltState
	risk    *memRisk
	audit   *memAudit
	report  *memReport
	mc      *livetrade.ManualController
}

// manualGate builds a gate giving the MANUAL strategy `budgetPct` of NAV with the
// default risk constraints.
func manualGateFor(t *testing.T, budgetPct, haltPct float64) *riskgate.Gate {
	t.Helper()
	alloc, err := riskgate.NewAllocator([]riskgate.StrategyAllocation{
		{StrategyID: livetrade.ManualStrategyID, CapitalPct: budgetPct},
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

// memPriceSource is a controllable PriceSource: it returns a configured price for
// a symbol regardless of whether that symbol has ever filled in the manual book —
// modelling the runner's broker/market-data price lookup. It is the test double for
// the finding-3 fix (the gate must bind on a never-filled discretionary symbol).
type memPriceSource struct {
	mu     sync.Mutex
	prices map[string]float64
}

func newMemPriceSource() *memPriceSource { return &memPriceSource{prices: map[string]float64{}} }

func (m *memPriceSource) set(sym string, px float64) {
	m.mu.Lock()
	m.prices[sym] = px
	m.mu.Unlock()
}

func (m *memPriceSource) LastPrice(_ context.Context, sym string) (domain.Price, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	px, ok := m.prices[sym]
	if !ok || px <= 0 {
		return 0, false
	}
	return domain.MustPrice(ftoa(px)), true
}

func newPaperManual(t *testing.T, gate *riskgate.Gate, navUSD float64) *manualHarness {
	return newPaperManualWithPrices(t, gate, navUSD, nil)
}

func newPaperManualWithPrices(t *testing.T, gate *riskgate.Gate, navUSD float64, prices livetrade.PriceSource) *manualHarness {
	t.Helper()
	ctx := context.Background()
	venue := moexec.NewMockVenue(manualPaperAcc)
	acct := accounting.NewAccount(domain.MustMoney(ftoa(navUSD)), nil)
	account := livetrade.NewAccountAdapter(acct)
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, manualPaperAcc, "paper")
	exec, err := moexec.New(ctx, moexec.Config{
		Account:  paperAcct,
		Client:   venue,
		TraderID: "PAPER-TEST-001",
		Sink:     &fillSink{},
		Book:     account,
	})
	require.NoError(t, err)

	halt := commands.NewHaltState(nil)
	risk := &memRisk{}
	audit := &memAudit{}
	report := &memReport{}
	// A reconciler wired to the SAME venue + the manual desk's OWN account book, so
	// SyncFromBroker's DIRECTION-2 reflection can be reconciled (broker truth vs the
	// reflected MANUAL book).
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: venue,
		Books:  account,
		Sink:   report,
		AccID:  manualPaperAcc,
		Env:    mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	mc, err := livetrade.NewManualController(livetrade.ManualControllerConfig{
		Acct:               paperAcct,
		Executor:           exec,
		Gate:               gate,
		Account:            account,
		Prices:             prices,
		Halt:               halt,
		Risk:               risk,
		Audit:              audit,
		Reconciler:         rec,
		NAV:                domain.MustMoney(ftoa(navUSD)),
		PaperTradePassword: paperTradePw,
	})
	require.NoError(t, err)
	return &manualHarness{venue: venue, exec: exec, account: account, acct: acct, halt: halt, risk: risk, audit: audit, report: report, mc: mc}
}

// observe sets the last price so the gate's estimated-fill price + the budget math
// have a current mark.
func (h *manualHarness) observe(sym string, px float64) {
	h.account.ObserveBar(bar(sym, 2, px))
}

// acceptFill drives accept+fill for a coid at price.
func (h *manualHarness) acceptFill(t *testing.T, coid string, px float64) {
	t.Helper()
	require.NoError(t, h.venue.Accept(coid))
	require.NoError(t, h.venue.Fill(coid, domain.MustPrice(ftoa(px))))
}

// --- lifecycle: place -> fill -> position ---

func TestManualPlaceFillPosition(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator:       "alice",
		IdempotencyKey: "k1",
		Symbol:         "AAPL",
		Side:           domain.OrderSideBuy,
		Qty:            10,
		Type:           domain.OrderTypeMarket,
		Confirm:        paperTradePw,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
	assert.True(t, strings.HasPrefix(res.ClientOrderID, "MANUAL-PAPER-"), "coid: %s", res.ClientOrderID)

	h.acceptFill(t, res.ClientOrderID, 100)

	// The MANUAL book row is long 10.
	pos, ok := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	require.True(t, ok)
	assert.Equal(t, domain.Qty(10), pos.SignedQty)

	// Audited.
	place := h.audit.byAction("place")
	require.Len(t, place, 1)
	assert.Equal(t, "alice", place[0].Operator)
	assert.Equal(t, "AAPL", place[0].Symbol)
	assert.Equal(t, "BUY", place[0].Side)
	assert.Equal(t, int64(10), place[0].Qty)
	assert.False(t, place[0].Override)
}

func TestManualLimitOrder(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("MSFT", 200)
	ctx := context.Background()
	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator:       "alice",
		IdempotencyKey: "lim1",
		Symbol:         "MSFT",
		Side:           domain.OrderSideBuy,
		Qty:            5,
		Type:           domain.OrderTypeLimit,
		LimitPrice:     domain.MustPrice("195.00"),
		Confirm:        paperTradePw,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
	st, ok := h.exec.TrackedOrder(res.ClientOrderID)
	require.True(t, ok)
	assert.Equal(t, domain.Qty(5), st.OrderQty)
}

// --- cancel ---

func TestManualCancel(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()
	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "bob", IdempotencyKey: "c1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	require.NoError(t, h.venue.Accept(res.ClientOrderID))

	require.NoError(t, h.mc.CancelManualOrder(ctx, "bob", res.ClientOrderID))
	st, ok := h.exec.TrackedOrder(res.ClientOrderID)
	require.True(t, ok)
	assert.Equal(t, domain.OrderStatusCanceled, st.Status)

	// Idempotent: a second cancel is a no-op success.
	require.NoError(t, h.mc.CancelManualOrder(ctx, "bob", res.ClientOrderID))

	cancels := h.audit.byAction("cancel")
	require.Len(t, cancels, 2)
	assert.Equal(t, "AAPL", cancels[0].Symbol)
}

// --- close ---

func TestManualClosePosition(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	// Open long 10.
	open, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "carol", IdempotencyKey: "o1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open.ClientOrderID, 100)

	// Close the whole position.
	cl, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "")
	require.NoError(t, err)
	require.True(t, cl.Submitted)
	h.acceptFill(t, cl.ClientOrderID, 105)

	pos, _ := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	assert.Equal(t, domain.Qty(0), pos.SignedQty, "position should be flat after close")

	// Closing an already-flat symbol is an idempotent no-op (no order).
	cl2, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "")
	require.NoError(t, err)
	assert.False(t, cl2.Submitted)

	closes := h.audit.byAction("close")
	require.GreaterOrEqual(t, len(closes), 1)
}

// sellOrdersAt counts the SELL orders the venue has recorded for a symbol (each
// distinct client-order-id is one venue order; PlaceOrder dedupes on coid). Used to
// prove a close never double-submits.
func sellOrdersAt(t *testing.T, h *manualHarness, symbol string) int {
	t.Helper()
	orders, err := h.venue.GetOrderList(context.Background(), manualPaperAcc, mo.TrdEnvSimulate)
	require.NoError(t, err)
	n := 0
	for _, o := range orders {
		if o.Symbol == symbol && o.Side == domain.OrderSideSell {
			n++
		}
	}
	return n
}

// TestManualCloseNoDoubleSubmit proves the FIX for the real-money oversell blocker
// (finding 3): two CONCURRENT full-closes of the same long-10 position derive the
// SAME idempotent client-order-id (from (symbol, net) — NO wall-clock component) and
// dedupe at the venue, so EXACTLY ONE SELL is placed and the position flattens to 0
// (never oversells long-10 -> short-10). Run under -race.
func TestManualCloseNoDoubleSubmit(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	// Open long 10.
	open, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "carol", IdempotencyKey: "dsopen", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open.ClientOrderID, 100)
	require.Equal(t, 0, sellOrdersAt(t, h, "AAPL"))

	// Two concurrent full-closes (the operator double-click / a client retry).
	var wg sync.WaitGroup
	coids := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, cerr := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "")
			coids[idx] = res.ClientOrderID
			errs[idx] = cerr
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// Both closes derive the SAME coid (idempotent on the open net) -> ONE SELL.
	assert.Equal(t, coids[0], coids[1], "concurrent closes must derive the same idempotent coid")
	assert.Equal(t, 1, sellOrdersAt(t, h, "AAPL"), "exactly one SELL order placed (no double-submit / oversell)")

	// Fill the single close; the position is flat (never short) — no oversell.
	h.acceptFill(t, coids[0], 105)
	pos, _ := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	assert.Equal(t, domain.Qty(0), pos.SignedQty, "closed to flat, never oversold to short")
}

// TestManualClosePassedIdempotencyKeyDedupes proves a caller-supplied idempotency
// key makes the close coid fully deterministic so a sequential retry (e.g. after a
// timed-out request) reuses the SAME coid and never places a second SELL.
func TestManualClosePassedIdempotencyKeyDedupes(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	open, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "carol", IdempotencyKey: "ido", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open.ClientOrderID, 100)

	first, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "retry-key-1")
	require.NoError(t, err)
	// A retry with the SAME key BEFORE the first fills: same coid, one venue order.
	second, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "retry-key-1")
	require.NoError(t, err)
	assert.Equal(t, first.ClientOrderID, second.ClientOrderID)
	assert.Equal(t, 1, sellOrdersAt(t, h, "AAPL"), "a retry with the same key never double-submits")
}

// TestManualReopenSameNetClosesAgain is the regression for finding 4: a position
// that is opened, closed, then RE-OPENED to the same symbol+net must close AGAIN —
// the re-open's close must place a FRESH SELL, not be swallowed by the prior open
// episode's tracked client-order-id. The prior coid derivation keyed only on
// (symbol, net), so against the long-lived desk executor the second episode's close
// re-derived the SAME coid, hit the executor's idempotency short-circuit, placed NO
// order, and the position never flattened (the e2e 33-manual-close spec timed out
// waiting for qty -> 0). The fix folds the open EPISODE's identity (avg entry px +
// the lot's last-fill ts) into the derived coid, so a re-opened lot gets a distinct
// coid while a true double-click of the SAME lot still dedupes.
func TestManualReopenSameNetClosesAgain(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	// --- Episode 1: open long 10 @ 100, then close it flat. ---
	open1, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "carol", IdempotencyKey: "ep1-open", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open1.ClientOrderID, 100)

	close1, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "")
	require.NoError(t, err)
	require.True(t, close1.Submitted)
	h.acceptFill(t, close1.ClientOrderID, 100)
	pos, _ := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	require.Equal(t, domain.Qty(0), pos.SignedQty, "episode 1 closed flat")
	require.Equal(t, 1, sellOrdersAt(t, h, "AAPL"), "episode 1 placed exactly one SELL")

	// --- Episode 2: RE-OPEN long 10 in the SAME symbol to the SAME net, then close.
	// The fill price differs (105) so the re-opened lot's avg entry px differs,
	// deterministically yielding a distinct close coid even without a clock advance.
	open2, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "carol", IdempotencyKey: "ep2-open", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open2.ClientOrderID, 105)
	pos, _ = h.account.Position(livetrade.ManualStrategyID, "AAPL")
	require.Equal(t, domain.Qty(10), pos.SignedQty, "episode 2 re-opened long 10")

	close2, err := h.mc.CloseManualPosition(ctx, "carol", "AAPL", 0, paperTradePw, "")
	require.NoError(t, err)
	require.True(t, close2.Submitted, "the re-opened position's close is SUBMITTED (not swallowed by the prior episode's coid)")
	assert.NotEqual(t, close1.ClientOrderID, close2.ClientOrderID,
		"a re-opened same-symbol/same-net lot derives a DISTINCT close coid (finding 4)")

	h.acceptFill(t, close2.ClientOrderID, 110)
	pos, _ = h.account.Position(livetrade.ManualStrategyID, "AAPL")
	assert.Equal(t, domain.Qty(0), pos.SignedQty, "episode 2 also closed flat")
	assert.Equal(t, 2, sellOrdersAt(t, h, "AAPL"), "two distinct SELLs across the two open episodes")
}

// TestManualConcurrentOpensSerialized proves the FIX for the gate TOCTOU
// (finding 9): PlaceManualOrder serializes the read-gate-submit sequence under
// m.mu, so concurrent opening orders on the same symbol are handled atomically (no
// data race on the gate read + book mutation; run under -race) and, once the first
// open's fill has SETTLED, a further over-budget open is deterministically rejected
// by the budget/concentration gate (no second open slips past on a stale snapshot).
func TestManualConcurrentOpensSerialized(t *testing.T) {
	// Budget so a 60-share AAPL @ $100 ($6k) fits within the single-name /
	// concentration cap but a SECOND 60-share open (120 shares, $12k) breaches it on
	// a $20k NAV.
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 20000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	// Concurrency safety: two concurrent opens (distinct keys) execute atomically.
	// We only assert NO data race + NO panic here (the -race detector is the gate);
	// the budget bound across SETTLED positions is asserted below. Both may pass
	// since neither has filled yet — that matches the auto GatedSubmitter, which also
	// gates against the settled book, not in-flight orders.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
				Operator: "dave", IdempotencyKey: "race-" + itoa(int64(idx)),
				Symbol: "MSFT", Side: domain.OrderSideBuy, Qty: 1,
				Type: domain.OrderTypeMarket, Confirm: paperTradePw,
			})
		}(i)
	}
	wg.Wait()

	// Now the budget bound: open + FILL 60 AAPL (settles the book), then a second
	// 60-AAPL open is rejected by the gate (the snapshot now carries the first).
	first, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dave", IdempotencyKey: "budget-1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 60, Type: domain.OrderTypeMarket, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, first.ClientOrderID, 100)

	_, err = h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dave", IdempotencyKey: "budget-2", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 60, Type: domain.OrderTypeMarket, Confirm: paperTradePw,
	})
	require.Error(t, err, "the over-budget second open is risk-rejected")
	assert.Contains(t, err.Error(), "risk gate violation")
}

// applyBuy settles a BUY fill of qty shares @ price into an AccountAdapter book.
func applyBuy(book *livetrade.AccountAdapter, strategyID, symbol string, qty int64, price string) error {
	_, err := book.ApplyFill(domain.Fill{
		TradeID:       "t-" + strategyID + "-" + symbol,
		ClientOrderID: "co-" + strategyID + "-" + symbol,
		StrategyID:    strategyID,
		Symbol:        symbol,
		Side:          domain.OrderSideBuy,
		Qty:           domain.Qty(qty),
		Price:         domain.MustPrice(price),
		TS:            time.Unix(0, 0).UTC(),
	})
	return err
}

// stratBooks is a minimal StrategyBooks source for the whole-system reconcile test:
// a fixed per-(strategy, symbol) signed book the strategy session "holds".
type stratBooks map[riskgate.PositionKey]int64

func (b stratBooks) BookPositions() map[riskgate.PositionKey]int64 {
	out := make(map[riskgate.PositionKey]int64, len(b))
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TestManualReconcileWholeSystemNoFalseDrift proves the FIX for the manual-sync
// reconciliation-scope blocker (finding 6): the broker (Trd_GetPositionList) returns
// the WHOLE account (strategy + manual positions). Reconciling it against the
// MANUAL-only book would mis-classify every strategy-held symbol as
// SymbolsOnlyAtBroker -> HasIssues() -> a false node halt. Aggregating the
// WHOLE-SYSTEM books (strategy session + manual) via CombineBooks makes the
// reconcile see the strategy-held symbol on BOTH sides, so there is NO false drift.
func TestManualReconcileWholeSystemNoFalseDrift(t *testing.T) {
	ctx := context.Background()
	venue := moexec.NewMockVenue(manualPaperAcc)

	// Broker truth: SPY 100 (an AUTO strategy's holding) + AAPL 10 (the MANUAL book).
	venue.SetPosition("SPY", 100, domain.MustPrice("400.00"))
	venue.SetPosition("AAPL", 10, domain.MustPrice("100.00"))

	// The MANUAL book holds only AAPL 10 (the desk's own position).
	manualAcct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	manualBook := livetrade.NewAccountAdapter(manualAcct)
	require.NoError(t, applyBuy(manualBook, livetrade.ManualStrategyID, "AAPL", 10, "100.00"))

	// The strategy session holds SPY 100 under an auto strategy id.
	strat := stratBooks{{StrategyID: "sepa", Symbol: "SPY"}: 100}

	// MANUAL-ONLY books would falsely flag SPY as broker-only drift.
	onlyManual, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: venue, Books: manualBook, AccID: manualPaperAcc, Env: mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	repBad, err := onlyManual.Reconcile(ctx)
	require.NoError(t, err)
	assert.True(t, repBad.HasIssues(), "manual-only book MIS-reports the strategy symbol as drift (the bug)")

	// WHOLE-SYSTEM books (manual + strategy) reconcile cleanly: no false drift.
	combined := livetrade.CombineBooks(manualBook.BookPositions, strat.BookPositions)
	wholeSystem, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker: venue, Books: combined, AccID: manualPaperAcc, Env: mo.TrdEnvSimulate,
	})
	require.NoError(t, err)
	repGood, err := wholeSystem.Reconcile(ctx)
	require.NoError(t, err)
	assert.False(t, repGood.HasIssues(), "whole-system books reconcile cleanly (no false drift -> no false halt)")
}

// --- LIVE safety ---

// A paper desk must never reach the real account: even if a live confirmation
// phrase is supplied, a paper executor is SIMULATE-bound and the venue refuses a
// real order. We prove the paper desk's executor is not live + a real order can
// never originate from it.
func TestPaperDeskNeverReachesReal(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	assert.False(t, h.mc.IsLive(), "paper desk must not be live")
	assert.False(t, h.exec.IsLive(), "paper executor must be SIMULATE-bound")

	// The paper desk requires the trade password, NOT a live phrase: supplying the
	// live phrase as the paper confirm is rejected (it is not the trade password).
	_, err := h.mc.PlaceManualOrder(context.Background(), livetrade.ManualOrderRequest{
		Operator: "x", IdempotencyKey: "p1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 1,
		Confirm: livetrade.ManualLiveConfirmationPhrase, // wrong: paper wants the trade pw
	})
	require.ErrorIs(t, err, livetrade.ErrTradePasswordRequired)
}

// A paper controller MUST refuse to wrap a live-bound executor (no real-money path
// through a "paper" desk), and a live controller MUST refuse a paper executor.
func TestManualControllerModeBindingEnforced(t *testing.T) {
	ctx := context.Background()
	venue := moexec.NewMockVenue(manualPaperAcc)
	venue.RegisterRealAccount(manualRealAcc, manualUnlock)
	acct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	account := livetrade.NewAccountAdapter(acct)

	liveAcct := domain.NewBrokerAccount("moomoo", domain.EnvReal, manualRealAcc, "live")
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, manualPaperAcc, "paper")

	// A real live-bound executor (full 4-factor activation against the mock venue).
	liveExec, err := moexec.New(ctx, moexec.Config{
		Account:            liveAcct,
		Client:             venue,
		TraderID:           moexec.LiveTraderID,
		ConfirmationPhrase: moexec.LiveConfirmationPhrase,
		UnlockPassword:     manualUnlock,
		Sink:               &fillSink{},
		Book:               account,
	})
	require.NoError(t, err)
	require.True(t, liveExec.IsLive())

	// A PAPER controller (paper Acct) wrapping a LIVE executor is refused.
	_, err = livetrade.NewManualController(livetrade.ManualControllerConfig{
		Acct: paperAcct, Executor: liveExec, Account: account,
		Halt: commands.NewHaltState(nil),
	})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	// A LIVE controller (real Acct) wrapping a PAPER executor is refused.
	paperExec, err := moexec.New(ctx, moexec.Config{
		Account: paperAcct, Client: venue,
		TraderID: "PAPER-X", Sink: &fillSink{}, Book: account,
	})
	require.NoError(t, err)
	_, err = livetrade.NewManualController(livetrade.ManualControllerConfig{
		Acct: liveAcct, Executor: paperExec, Account: account,
		Halt: commands.NewHaltState(nil),
	})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// A live desk requires the EXACT per-order confirmation phrase: a missing/wrong
// phrase returns ErrConfirmationRequired and NO order is placed (the venue sees
// no order). With the phrase the order proceeds to the real account.
func TestLiveDeskRequiresPerOrderConfirm(t *testing.T) {
	ctx := context.Background()
	venue := moexec.NewMockVenue(manualPaperAcc)
	venue.RegisterRealAccount(manualRealAcc, manualUnlock)
	acct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	account := livetrade.NewAccountAdapter(acct)
	account.ObserveBar(bar("AAPL", 2, 100))
	liveAcct := domain.NewBrokerAccount("moomoo", domain.EnvReal, manualRealAcc, "live")
	liveExec, err := moexec.New(ctx, moexec.Config{
		Account:            liveAcct,
		Client:             venue,
		TraderID:           moexec.LiveTraderID,
		ConfirmationPhrase: moexec.LiveConfirmationPhrase,
		UnlockPassword:     manualUnlock,
		Sink:               &fillSink{},
		Book:               account,
	})
	require.NoError(t, err)
	audit := &memAudit{}
	mc, err := livetrade.NewManualController(livetrade.ManualControllerConfig{
		Acct:     liveAcct,
		Executor: liveExec,
		Gate:     manualGateFor(t, 1.0, 0.10),
		Account:  account,
		Halt:     commands.NewHaltState(nil),
		Audit:    audit,
		NAV:      domain.MustMoney("100000.00"),
	})
	require.NoError(t, err)
	require.True(t, mc.IsLive())

	// No confirmation phrase -> rejected, NO order placed.
	_, err = mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "trader", IdempotencyKey: "live1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 1,
	})
	require.ErrorIs(t, err, livetrade.ErrConfirmationRequired)
	orders, _ := venue.GetOrderList(ctx, manualRealAcc, 0)
	assert.Empty(t, orders, "no order may reach the venue without the confirmation phrase")

	// A wrong phrase is also rejected.
	_, err = mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "trader", IdempotencyKey: "live2", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 1, Confirm: "not the phrase",
	})
	require.ErrorIs(t, err, livetrade.ErrConfirmationRequired)

	// The exact phrase -> the order reaches the REAL account.
	res, err := mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "trader", IdempotencyKey: "live3", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 1, Confirm: livetrade.ManualLiveConfirmationPhrase,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
	assert.True(t, strings.HasPrefix(res.ClientOrderID, "MANUAL-LIVE-"))
	orders, _ = venue.GetOrderList(ctx, manualRealAcc, 0)
	assert.Len(t, orders, 1, "the confirmed order reached the real account")
}

// --- risk gate reject + override ---

func TestManualRiskGateRejectAndOverride(t *testing.T) {
	// A tiny 1% budget on a 100k NAV ($1000) makes a 100-share @ $100 ($10k) open
	// exceed the allocator budget — a guaranteed risk-gate rejection.
	h := newPaperManual(t, manualGateFor(t, 0.01, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()

	// Without override: rejected with ErrRiskViolation, NO order placed.
	_, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dan", IdempotencyKey: "r1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 100, Confirm: paperTradePw,
	})
	require.ErrorIs(t, err, livetrade.ErrRiskViolation)
	orders, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	assert.Empty(t, orders, "a risk-rejected order must not reach the venue")
	// A rejection is recorded in risk_events (approved=false).
	require.NotEmpty(t, h.risk.rejections())

	// With override: the order proceeds + the override is recorded (risk_events
	// approved-with-rule + audit override flag).
	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dan", IdempotencyKey: "r2", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 100, Override: true, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
	orders, _ = h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	assert.Len(t, orders, 1, "the overridden order reaches the venue")

	// Audit shows the override + the bypassed rule.
	var overrode bool
	for _, a := range h.audit.byAction("place") {
		if a.Override {
			overrode = true
			assert.NotEmpty(t, a.RiskRule, "override audit must carry the bypassed rule")
		}
	}
	assert.True(t, overrode, "an override audit row is required")

	// An approved override risk_events row exists (RuleName carries "override:").
	var overrideEvent bool
	for _, d := range h.risk.decisions {
		if d.Approved && strings.HasPrefix(d.RuleName, "override:") {
			overrideEvent = true
		}
	}
	assert.True(t, overrideEvent, "an approved override risk_events row is required")
}

// TestManualRiskGateBindsViaBrokerPrice proves the FIX for the inert risk gate
// (finding 3): the gate must bind on a NEVER-FILLED discretionary symbol, priced
// from the broker PriceSource — NOT only from a symbol the manual book has already
// observed via a fill. Without the PriceSource the order priced at 0 notional and
// the budget rule silently approved an arbitrarily large order.
func TestManualRiskGateBindsViaBrokerPrice(t *testing.T) {
	// 1% budget on 100k NAV = $1000. A 100-share @ $100 ($10k) open exceeds it.
	prices := newMemPriceSource()
	prices.set("AAPL", 100) // the broker prices AAPL; the MANUAL book never filled it.
	h := newPaperManualWithPrices(t, manualGateFor(t, 0.01, 0.10), 100000, prices)
	ctx := context.Background()
	// NOTE: deliberately NO h.observe("AAPL", ...) — the manual book's LastPrice is 0.

	_, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dan", IdempotencyKey: "bp1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 100, Confirm: paperTradePw,
	})
	require.ErrorIs(t, err, livetrade.ErrRiskViolation,
		"the gate must bind on the broker-sourced price even though the symbol never filled")
	orders, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	assert.Empty(t, orders, "an over-budget order must not reach the venue")

	// An override still proceeds (the audited operator escape).
	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "dan", IdempotencyKey: "bp2", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 100, Override: true, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
}

// TestManualRiskGateFailsClosedWhenUnpriced proves an opening order the gate cannot
// price AT ALL (no LIMIT price, no broker quote, no fill-observed price) is REJECTED
// (fail-closed), not silently approved at 0 notional — the safety half of finding 3.
// An override is still the audited escape, and a LIMIT order supplies its own price.
func TestManualRiskGateFailsClosedWhenUnpriced(t *testing.T) {
	prices := newMemPriceSource() // knows NO prices
	h := newPaperManualWithPrices(t, manualGateFor(t, 1.0, 0.10), 100000, prices)
	ctx := context.Background()

	_, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "gail", IdempotencyKey: "u1", Symbol: "ZZZZ",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.ErrorIs(t, err, livetrade.ErrRiskViolation,
		"an unpriceable opening order must fail closed, not pass at 0 notional")
	orders, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	assert.Empty(t, orders, "an unpriced order must not reach the venue")

	// A LIMIT order carries its own price, so it is priceable and within budget.
	res, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "gail", IdempotencyKey: "u2", Symbol: "ZZZZ",
		Side: domain.OrderSideBuy, Qty: 10, Type: domain.OrderTypeLimit,
		LimitPrice: domain.MustPrice("50.00"), Confirm: paperTradePw,
	})
	require.NoError(t, err)
	require.True(t, res.Submitted)
}

// A FLAT/closing order bypasses the budget even when the budget is exhausted.
func TestManualCloseBypassesBudget(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()
	// Open long 50 (within the 100% budget).
	open, err := h.mc.PlaceManualOrder(ctx, livetrade.ManualOrderRequest{
		Operator: "eve", IdempotencyKey: "b1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 50, Confirm: paperTradePw,
	})
	require.NoError(t, err)
	h.acceptFill(t, open.ClientOrderID, 100)

	// Now HALT the desk (suppresses opens). A close must still pass.
	h.halt.Halt(commands.HaltManual, "test halt")
	cl, err := h.mc.CloseManualPosition(ctx, "eve", "AAPL", 0, paperTradePw, "")
	require.NoError(t, err)
	require.True(t, cl.Submitted, "a close must pass even while halted")
}

// A halted desk rejects a NEW opening order (the daily-loss / manual halt gate).
func TestManualOpenSuppressedWhileHalted(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	h.halt.Halt(commands.HaltManual, "halt")
	_, err := h.mc.PlaceManualOrder(context.Background(), livetrade.ManualOrderRequest{
		Operator: "f", IdempotencyKey: "h1", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	})
	require.ErrorIs(t, err, livetrade.ErrRiskViolation)
}

// --- idempotent double-submit ---

func TestManualIdempotentDoubleSubmit(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	h.observe("AAPL", 100)
	ctx := context.Background()
	req := livetrade.ManualOrderRequest{
		Operator: "gil", IdempotencyKey: "dup", Symbol: "AAPL",
		Side: domain.OrderSideBuy, Qty: 10, Confirm: paperTradePw,
	}
	r1, err := h.mc.PlaceManualOrder(ctx, req)
	require.NoError(t, err)
	r2, err := h.mc.PlaceManualOrder(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, r1.ClientOrderID, r2.ClientOrderID, "same idempotency key => same coid")

	// Exactly ONE order at the venue despite two submits.
	orders, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	assert.Len(t, orders, 1, "a double-submit must not create a second venue order")

	// Filling once leaves a single net-10 position (no double count).
	h.acceptFill(t, r1.ClientOrderID, 100)
	pos, _ := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	assert.Equal(t, domain.Qty(10), pos.SignedQty)
}

// --- validation ---

func TestManualPlaceValidation(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()
	bad := []livetrade.ManualOrderRequest{
		{IdempotencyKey: "v", Symbol: "AAPL", Side: domain.OrderSideBuy, Qty: 1, Confirm: paperTradePw},        // no operator
		{Operator: "o", Symbol: "AAPL", Side: domain.OrderSideBuy, Qty: 1, Confirm: paperTradePw},              // no key
		{Operator: "o", IdempotencyKey: "v", Side: domain.OrderSideBuy, Qty: 1, Confirm: paperTradePw},         // no symbol
		{Operator: "o", IdempotencyKey: "v", Symbol: "AAPL", Qty: 1, Confirm: paperTradePw},                    // bad side
		{Operator: "o", IdempotencyKey: "v", Symbol: "AAPL", Side: domain.OrderSideBuy, Confirm: paperTradePw}, // qty 0
	}
	for i, req := range bad {
		_, err := h.mc.PlaceManualOrder(ctx, req)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "case %d should be invalid", i)
	}
}

var _ = time.Now
