package moomoo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// --- test doubles ---

// fakeAccount is a minimal netting account book: it nets fills per (strategy,
// symbol) and tracks realized PnL crudely (enough to assert position math).
type fakeAccount struct {
	mu  sync.Mutex
	pos map[string]*domain.Position // key strategy|symbol
}

func newFakeAccount() *fakeAccount { return &fakeAccount{pos: map[string]*domain.Position{}} }

func key(s, sym string) string { return s + "|" + sym }

func (a *fakeAccount) ApplyFill(f domain.Fill) (domain.Position, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := key(f.StrategyID, f.Symbol)
	p := a.pos[k]
	if p == nil {
		p = &domain.Position{StrategyID: f.StrategyID, Symbol: f.Symbol}
		a.pos[k] = p
	}
	signed := f.Qty
	if f.Side == domain.OrderSideSell {
		signed = -f.Qty
	}
	p.SignedQty += signed
	p.AvgPx = f.Price
	p.UpdatedAt = f.TS
	return *p, nil
}

func (a *fakeAccount) Position(strategyID, symbol string) (domain.Position, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pos[key(strategyID, symbol)]
	if !ok {
		return domain.Position{}, false
	}
	return *p, true
}

// recordSink captures emitted fills.
type recordSink struct {
	mu    sync.Mutex
	fills []domain.Fill
}

func (s *recordSink) EmitFill(f domain.Fill) error {
	s.mu.Lock()
	s.fills = append(s.fills, f)
	s.mu.Unlock()
	return nil
}
func (s *recordSink) all() []domain.Fill {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Fill(nil), s.fills...)
}

// recordPersist captures persistence writes.
type recordPersist struct {
	mu        sync.Mutex
	orders    []domain.Order
	fills     []domain.Fill
	positions []domain.Position
}

func (p *recordPersist) UpsertOrder(_ context.Context, o domain.Order) error {
	p.mu.Lock()
	p.orders = append(p.orders, o)
	p.mu.Unlock()
	return nil
}
func (p *recordPersist) InsertFill(_ context.Context, f domain.Fill) error {
	p.mu.Lock()
	p.fills = append(p.fills, f)
	p.mu.Unlock()
	return nil
}
func (p *recordPersist) UpsertPosition(_ context.Context, pos domain.Position) error {
	p.mu.Lock()
	p.positions = append(p.positions, pos)
	p.mu.Unlock()
	return nil
}

type recordRisk struct {
	mu     sync.Mutex
	events []string
}

func (r *recordRisk) RecordRiskEvent(_ context.Context, _, _, rule, detail string) error {
	r.mu.Lock()
	r.events = append(r.events, rule+":"+detail)
	r.mu.Unlock()
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// mapStrategyResolver is a test StrategyResolver backed by an in-memory
// client-order-id -> strategy-id map (mirrors the live.orders lookup). It also
// records an error to inject for a coid, to exercise the failure path.
type mapStrategyResolver struct {
	mu  sync.Mutex
	m   map[string]string
	err map[string]error
}

func newMapStrategyResolver() *mapStrategyResolver {
	return &mapStrategyResolver{m: map[string]string{}, err: map[string]error{}}
}

func (r *mapStrategyResolver) put(coid, strategyID string) {
	r.mu.Lock()
	r.m[coid] = strategyID
	r.mu.Unlock()
}

func (r *mapStrategyResolver) StrategyForOrder(_ context.Context, coid string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.err[coid]; ok {
		return "", false, e
	}
	sid, ok := r.m[coid]
	return sid, ok && sid != "", nil
}

const paperAcc = uint64(900001)

func newPaperExecutor(t *testing.T) (*MoomooExecutor, *MockVenue, *fakeAccount, *recordSink, *recordPersist) {
	t.Helper()
	venue := NewMockVenue(paperAcc)
	acct := newFakeAccount()
	sink := &recordSink{}
	persist := &recordPersist{}
	e, err := New(context.Background(), Config{
		Mode:     ModePaper,
		Client:   venue,
		AccID:    paperAcc,
		TraderID: "PAPER-SMOKE-001",
		Sink:     sink,
		Account:  acct,
		Persist:  persist,
		Clock:    fixedClock{t: time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("New paper executor: %v", err)
	}
	return e, venue, acct, sink, persist
}

// --- lifecycle tests ---

func TestSubmitAcceptFillUpdatesPositionAndSink(t *testing.T) {
	e, venue, acct, sink, persist := newPaperExecutor(t)
	ts := time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC)

	coid, err := e.SubmitMarket("SEPA-000", "AAPL", domain.OrderSideBuy, 100, "entry", ts)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := venue.Fill(coid, domain.MustPrice("150.00")); err != nil {
		t.Fatal(err)
	}

	// Position settled long 100 @ 150.
	pos, ok := acct.Position("SEPA-000", "AAPL")
	if !ok || pos.SignedQty != 100 {
		t.Fatalf("want long 100, got %+v ok=%v", pos, ok)
	}
	// One fill emitted to the engine sink.
	fills := sink.all()
	if len(fills) != 1 || fills[0].Qty != 100 || fills[0].Price != domain.MustPrice("150.00") {
		t.Fatalf("want 1 fill 100@150, got %+v", fills)
	}
	// Order persisted reaching FILLED.
	if !persistedStatus(persist, coid, domain.OrderStatusFilled) {
		t.Fatalf("order %s never persisted FILLED; orders=%+v", coid, persist.orders)
	}
	if e.FillsEmitted() != 1 {
		t.Fatalf("FillsEmitted=%d want 1", e.FillsEmitted())
	}
}

func TestRejectPathNoPositionNoFill(t *testing.T) {
	e, venue, acct, sink, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, err := e.SubmitMarket("SEPA-000", "BADX", domain.OrderSideBuy, 10, "entry", ts)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := venue.Reject(coid, "insufficient buying power"); err != nil {
		t.Fatal(err)
	}
	if _, ok := acct.Position("SEPA-000", "BADX"); ok {
		t.Fatal("rejected order must not open a position")
	}
	if len(sink.all()) != 0 {
		t.Fatal("rejected order must emit no fill")
	}
}

func TestSubmitTimeRejectSurfacesRiskEvent(t *testing.T) {
	venue := NewMockVenue(paperAcc)
	acct := newFakeAccount()
	risk := &recordRisk{}
	e, err := New(context.Background(), Config{
		Mode: ModePaper, Client: venue, AccID: paperAcc, TraderID: "PAPER-SMOKE-001",
		Sink: &recordSink{}, Account: acct, Risk: risk, Clock: fixedClock{t: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	venue.FailNextPlace(errors.New("bad symbol"))
	_, err = e.SubmitMarket("SEPA-000", "ZZZZ", domain.OrderSideBuy, 5, "entry", time.Now().UTC())
	if err == nil {
		t.Fatal("expected submit error on venue place failure")
	}
	risk.mu.Lock()
	n := len(risk.events)
	risk.mu.Unlock()
	if n == 0 {
		t.Fatal("place failure must record a risk event")
	}
}

func TestPartialFillsAccumulate(t *testing.T) {
	e, venue, acct, sink, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, err := e.SubmitMarket("PAIRS-000", "MSFT", domain.OrderSideBuy, 100, "entry", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	// Two partials then completion: 40 @ 200, then 60 @ 201 (cumulative 100).
	if err := venue.PartialFill(coid, 40, domain.MustPrice("200.00")); err != nil {
		t.Fatal(err)
	}
	if err := venue.PartialFill(coid, 60, domain.MustPrice("201.00")); err != nil {
		t.Fatal(err)
	}
	fills := sink.all()
	if len(fills) != 2 {
		t.Fatalf("want 2 fill deltas, got %d: %+v", len(fills), fills)
	}
	if fills[0].Qty != 40 || fills[0].Price != domain.MustPrice("200.00") {
		t.Fatalf("first delta wrong: %+v", fills[0])
	}
	if fills[1].Qty != 60 || fills[1].Price != domain.MustPrice("201.00") {
		t.Fatalf("second delta wrong: %+v", fills[1])
	}
	pos, _ := acct.Position("PAIRS-000", "MSFT")
	if pos.SignedQty != 100 {
		t.Fatalf("want net 100, got %d", pos.SignedQty)
	}
}

func TestIdempotentDoublePush(t *testing.T) {
	e, venue, acct, sink, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, err := e.SubmitMarket("ORB-000", "SPY", domain.OrderSideBuy, 50, "entry", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := venue.Fill(coid, domain.MustPrice("500.00")); err != nil {
		t.Fatal(err)
	}
	// Replay the SAME terminal fill push twice more — must be no-ops.
	_ = venue.PushRaw(coid)
	_ = venue.PushRaw(coid)
	if got := len(sink.all()); got != 1 {
		t.Fatalf("duplicate pushes must not re-emit fills; got %d", got)
	}
	pos, _ := acct.Position("ORB-000", "SPY")
	if pos.SignedQty != 50 {
		t.Fatalf("duplicate pushes must not double-count; got %d", pos.SignedQty)
	}
}

func TestCancelTerminal(t *testing.T) {
	e, venue, _, sink, persist := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, err := e.SubmitMarket("SEPA-000", "NVDA", domain.OrderSideBuy, 20, "entry", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := venue.Cancel(coid); err != nil {
		t.Fatal(err)
	}
	if !persistedStatus(persist, coid, domain.OrderStatusCanceled) {
		t.Fatalf("order %s never persisted CANCELED", coid)
	}
	// A fill after cancel (broker race) must be ignored — terminal stickiness.
	_ = venue.Fill(coid, domain.MustPrice("100.00"))
	if len(sink.all()) != 0 {
		t.Fatal("fill after cancel must be dropped (terminal stickiness)")
	}
}

func TestIdempotentSubmitNoDoubleOrder(t *testing.T) {
	e, venue, _, _, _ := newPaperExecutor(t)
	ts := time.Now().UTC()
	coid, err := e.SubmitMarket("SEPA-000", "AAPL", domain.OrderSideBuy, 10, "entry", ts)
	if err != nil {
		t.Fatal(err)
	}
	// Re-place the SAME client-order-id via the venue directly (simulating a
	// reconnect retry of an already-submitted order).
	res1, _ := venue.PlaceOrder(context.Background(), mo.PlaceOrderRequest{
		AccID: paperAcc, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: coid,
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, Qty: 10,
	})
	res2, _ := venue.PlaceOrder(context.Background(), mo.PlaceOrderRequest{
		AccID: paperAcc, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: coid,
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, Qty: 10,
	})
	if res1.VenueOrderID != res2.VenueOrderID {
		t.Fatalf("idempotent submit must return same venue id: %s vs %s", res1.VenueOrderID, res2.VenueOrderID)
	}
}

func persistedStatus(p *recordPersist, coid string, status domain.OrderStatus) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.orders {
		if o.ClientOrderID == coid && o.Status == status {
			return true
		}
	}
	return false
}
