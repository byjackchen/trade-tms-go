package runner

// manual_price_source_test.go covers the brokerPriceSource — the manual desk's
// risk-gate price lookup that resolves a symbol's reference price from the
// broker/market-data feed (Qot_RequestHistoryKL's latest daily close). This is the
// fix for the inert risk gate (finding 3): without a price, an opening manual order
// on a never-filled symbol priced at 0 notional and the budget gate was a no-op.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// fakeHistClient is a MoomooClient that only implements RequestHistoryKL (the only
// method brokerPriceSource calls); the rest panic to prove they are never reached.
type fakeHistClient struct {
	bars map[string][]domain.Bar
	err  error
	last struct {
		symbol      string
		begin, end  time.Time
		queryCalled bool
	}
}

func (f *fakeHistClient) RequestHistoryKL(_ context.Context, symbol string, _ qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error) {
	f.last.symbol = symbol
	f.last.begin, f.last.end = begin, end
	f.last.queryCalled = true
	if f.err != nil {
		return nil, f.err
	}
	return f.bars[symbol], nil
}

func (f *fakeHistClient) Start(context.Context)       { panic("unused") }
func (f *fakeHistClient) Ready(context.Context) error { panic("unused") }
func (f *fakeHistClient) Subscribe(context.Context, []string, qotcommon.KLType) error {
	panic("unused")
}
func (f *fakeHistClient) TradeClient() moomoo.TradeClient { panic("unused") }
func (f *fakeHistClient) Close() error                    { panic("unused") }

func bar(sym string, ts time.Time, close float64) domain.Bar {
	px := domain.MustPrice(ftoaPx(close))
	return domain.Bar{Symbol: sym, TS: ts, Open: px, High: px, Low: px, Close: px, Volume: 1}
}

func ftoaPx(v float64) string {
	p, _ := domain.PriceFromFloat64(v)
	return p.String()
}

func TestBrokerPriceSourceLatestClose(t *testing.T) {
	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	fc := &fakeHistClient{bars: map[string][]domain.Bar{
		"AAPL": {
			bar("AAPL", t0, 100),
			bar("AAPL", t0.AddDate(0, 0, 1), 101),
			bar("AAPL", t0.AddDate(0, 0, 2), 195.50), // 195.50 latest
		},
	}}
	ps := newBrokerPriceSource(fc)
	px, ok := ps.LastPrice(context.Background(), "AAPL")
	if !ok {
		t.Fatal("expected a price for AAPL")
	}
	// The LAST (most-recent) bar's close is returned.
	if px.String() != "195.5" {
		t.Fatalf("want latest close 195.5, got %s", px.String())
	}
	// A wide trailing window was queried (≈1 year), robust to stale data.
	if !fc.last.queryCalled {
		t.Fatal("expected RequestHistoryKL to be called")
	}
	if span := fc.last.end.Sub(fc.last.begin); span < 360*24*time.Hour {
		t.Fatalf("expected a wide (>~1y) history window, got %s", span)
	}
}

func TestBrokerPriceSourceUnknownSymbolNotOK(t *testing.T) {
	fc := &fakeHistClient{bars: map[string][]domain.Bar{}}
	ps := newBrokerPriceSource(fc)
	if _, ok := ps.LastPrice(context.Background(), "ZZZZ"); ok {
		t.Fatal("an unknown symbol must return ok=false (no price), so the gate fails closed")
	}
}

func TestBrokerPriceSourceErrorNotOK(t *testing.T) {
	fc := &fakeHistClient{err: errors.New("feed down")}
	ps := newBrokerPriceSource(fc)
	if _, ok := ps.LastPrice(context.Background(), "AAPL"); ok {
		t.Fatal("a feed error must return ok=false, never a phantom 0 price")
	}
}

func TestBrokerPriceSourceNilClient(t *testing.T) {
	if ps := newBrokerPriceSource(nil); ps != nil {
		t.Fatal("a nil client yields a nil (disabled) price source")
	}
	// A nil *brokerPriceSource is still safe to call.
	var ps *brokerPriceSource
	if _, ok := ps.LastPrice(context.Background(), "AAPL"); ok {
		t.Fatal("nil price source must return ok=false")
	}
}
