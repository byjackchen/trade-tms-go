package moomoo

import (
	"context"
	"testing"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

const realAcc = uint64(700700)

// liveCfg builds a fully-correct live config against a venue that has the real
// account registered + unlock password set.
func liveCfg(venue *MockVenue) Config {
	return Config{
		Account:            domain.NewBrokerAccount("moomoo", domain.EnvReal, realAcc, ""),
		Client:             venue,
		TraderID:           LiveTraderID,
		ConfirmationPhrase: LiveConfirmationPhrase,
		UnlockPassword:     "s3cret",
		Sink:               &recordSink{},
		Book:               newFakeAccount(),
		Clock:              fixedClock{t: time.Now().UTC()},
	}
}

func liveVenue() *MockVenue {
	v := NewMockVenue(paperAcc)
	v.RegisterRealAccount(realAcc, "s3cret")
	return v
}

// PROOF 1: a signal/paper config can NEVER place a live order — a paper executor
// is bound to SIMULATE and the real account, even if named, is unreachable.
func TestPaperExecutorCannotReachLiveAccount(t *testing.T) {
	venue := liveVenue()
	// Build a PAPER executor but (mistakenly) point it at the real acc id.
	e, err := New(context.Background(), Config{
		Account: domain.NewBrokerAccount("moomoo", domain.EnvSimulate, realAcc, ""), Client: venue, TraderID: "PAPER-SMOKE-001",
		Sink: &recordSink{}, Book: newFakeAccount(), Clock: fixedClock{t: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("paper build: %v", err)
	}
	if e.Env() != mo.TrdEnvSimulate {
		t.Fatalf("paper executor must bind SIMULATE, got %s", e.Env())
	}
	if e.IsLive() {
		t.Fatal("paper executor must never report IsLive")
	}
	// Submit: it goes to the SIMULATE env, NOT the real account. The venue never
	// gets unlocked, and the REAL account is untouched.
	coid, err := e.SubmitMarket("S", "AAPL", domain.OrderSideBuy, 1, "x", time.Now().UTC())
	if err != nil {
		t.Fatalf("paper submit: %v", err)
	}
	// The order landed under SIMULATE: a REAL place would have been refused by the
	// venue (REAL order before UnlockTrade). Prove the venue was never unlocked.
	if venue.Unlocked() {
		t.Fatal("paper path must never unlock the real account")
	}
	_ = coid
}

// PROOF 2: a paper config carrying the LIVE trader-id is refused outright (paper
// can never look like live).
func TestPaperWithLiveTraderIDRefused(t *testing.T) {
	venue := liveVenue()
	_, err := New(context.Background(), Config{
		Account: domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAcc, ""), Client: venue, TraderID: LiveTraderID,
		Sink: &recordSink{}, Book: newFakeAccount(), Clock: fixedClock{t: time.Now().UTC()},
	})
	if err == nil {
		t.Fatal("paper executor with the live trader-id must be refused")
	}
}

// PROOF 3: live requires the confirmation phrase.
func TestLiveRequiresConfirmationPhrase(t *testing.T) {
	venue := liveVenue()
	cfg := liveCfg(venue)
	cfg.ConfirmationPhrase = "yes please"
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("live activation without the exact phrase must be refused")
	}
}

// PROOF 4: live requires the LiveTraderID namespace.
func TestLiveRequiresLiveTraderID(t *testing.T) {
	venue := liveVenue()
	cfg := liveCfg(venue)
	cfg.TraderID = "PAPER-SMOKE-001"
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("live activation without the live trader-id must be refused")
	}
}

// PROOF 5: live requires the real acc id to EXIST under REAL env.
func TestLiveRequiresRealAccount(t *testing.T) {
	venue := NewMockVenue(paperAcc) // NO real account registered
	cfg := liveCfg(venue)
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("live activation must be refused when the real acc id is unknown")
	}
	if venue.Unlocked() {
		t.Fatal("must not unlock when activation is refused")
	}
}

// PROOF 6: live requires UnlockTrade to SUCCEED; a wrong password fails
// activation and leaves no usable executor.
func TestLiveRequiresUnlockSuccess(t *testing.T) {
	venue := liveVenue()
	cfg := liveCfg(venue)
	cfg.UnlockPassword = "wrong"
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("live activation must be refused when UnlockTrade fails")
	}
}

// PROOF 7: a correctly-gated live executor activates, unlocks, binds REAL, and
// can place a real order (against the mock real account).
func TestLiveHappyPathActivatesAndUnlocks(t *testing.T) {
	venue := liveVenue()
	e, err := New(context.Background(), liveCfg(venue))
	if err != nil {
		t.Fatalf("live activation should succeed: %v", err)
	}
	if !e.IsLive() || e.Env() != mo.TrdEnvReal {
		t.Fatalf("live executor must bind REAL env")
	}
	if !venue.Unlocked() {
		t.Fatal("live activation must have unlocked the real account")
	}
	// A real order is now reachable (the venue accepts it post-unlock).
	coid, err := e.SubmitMarket("S", "AAPL", domain.OrderSideBuy, 1, "x", time.Now().UTC())
	if err != nil {
		t.Fatalf("live submit after activation: %v", err)
	}
	if err := venue.Accept(coid); err != nil {
		t.Fatal(err)
	}
	if err := venue.Fill(coid, domain.MustPrice("150.00")); err != nil {
		t.Fatal(err)
	}
}

// PROOF 8: the mock venue itself refuses a REAL order before unlock — defence in
// depth proving no real order is reachable without unlock.
func TestVenueRefusesRealOrderBeforeUnlock(t *testing.T) {
	venue := liveVenue()
	_, err := venue.PlaceOrder(context.Background(), mo.PlaceOrderRequest{
		AccID: realAcc, TrdEnv: mo.TrdEnvReal, ClientOrderID: "LIVE-O-0",
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, Qty: 1,
	})
	if err == nil {
		t.Fatal("venue must refuse a REAL order before UnlockTrade")
	}
}
