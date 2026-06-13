package engine

// recorder.go collects the engine's observable output — orders, fills,
// position lifecycle, and account-state events — in deterministic order, for
// the run result and the runs/{ts}/*.json artifacts. It subscribes to the
// message bus and records callbacks as they fire on the loop goroutine.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// AccountStatePoint is one recorded account-state event (the equity-curve
// granularity for account.json, §7.4): a USD balance at a causal ts.
type AccountStatePoint struct {
	TS         time.Time
	BalanceUSD domain.Money
}

// Recorder accumulates run output. It implements the core observer interfaces.
type Recorder struct {
	orders        []domain.Order
	fills         []domain.Fill
	accountStates []AccountStatePoint
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// RecordOrder appends a submitted order (the engine calls this on submit).
func (r *Recorder) RecordOrder(o domain.Order) { r.orders = append(r.orders, o) }

// OnFill records a fill (FillObserver).
func (r *Recorder) OnFill(f domain.Fill) { r.fills = append(r.fills, f) }

// OnAccountState records an account-state event (AccountStateObserver).
func (r *Recorder) OnAccountState(s core.AccountState) {
	r.accountStates = append(r.accountStates, AccountStatePoint{TS: s.TS, BalanceUSD: s.Total})
}

// Orders returns the recorded submitted orders in submission order.
func (r *Recorder) Orders() []domain.Order { return r.orders }

// Fills returns the recorded fills in settlement order.
func (r *Recorder) Fills() []domain.Fill { return r.fills }

// AccountStates returns the account-state curve in emission order.
func (r *Recorder) AccountStates() []AccountStatePoint { return r.accountStates }
