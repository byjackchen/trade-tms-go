package core

// msgbus.go is the in-process publish/subscribe seam that lets accounting,
// samplers and recorders observe fills and account-state without the loop
// knowing about them. It mirrors the role of Nautilus's MessageBus inside the
// single-threaded backtest: synchronous, ordered, in-goroutine delivery.
//
// Determinism: subscribers receive messages in publish order, and for one
// message in subscription (registration) order. There is no concurrency and no
// map iteration in the delivery path.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// FillObserver is notified of each settled fill, in settlement order.
type FillObserver interface {
	OnFill(domain.Fill)
}

// AccountStateObserver is notified of each emitted account-state, in emission
// order. AccountState carries the post-mutation balance and the causal ts.
type AccountStateObserver interface {
	OnAccountState(AccountState)
}

// BarObserver is notified of each bar as it is dispatched (for recorders /
// samplers that need to see market data alongside the executor).
type BarObserver interface {
	OnBar(domain.Bar)
}

// AccountState is the post-mutation account snapshot emitted on every
// balance-affecting settlement, mirroring Nautilus's AccountState event
// (spec §7.5). Total is the base-currency (USD) balance after the mutation;
// Free equals Total for the zero-margin equity instrument; TS is the causal
// event's timestamp (the fill/bar ts).
type AccountState struct {
	TS       time.Time    // causal event timestamp (UTC)
	Total    domain.Money // base-currency balance after the mutation
	Free     domain.Money // == Total for the zero-margin equity instrument
	Locked   domain.Money
	Realized domain.Money // cumulative realized PnL backing Total
}

// MsgBus is the synchronous in-process bus. Subscribe before running; the loop
// publishes during dispatch. Not safe for concurrent Subscribe during Publish
// (register all observers before run, as the engine does).
type MsgBus struct {
	fillObs []FillObserver
	acctObs []AccountStateObserver
	barObs  []BarObserver
}

// NewMsgBus returns an empty bus.
func NewMsgBus() *MsgBus { return &MsgBus{} }

// SubscribeFills registers a fill observer. Observers are notified in
// registration order.
func (b *MsgBus) SubscribeFills(o FillObserver) { b.fillObs = append(b.fillObs, o) }

// SubscribeAccountState registers an account-state observer.
func (b *MsgBus) SubscribeAccountState(o AccountStateObserver) { b.acctObs = append(b.acctObs, o) }

// SubscribeBars registers a bar observer.
func (b *MsgBus) SubscribeBars(o BarObserver) { b.barObs = append(b.barObs, o) }

// PublishFill delivers f to every fill observer in registration order.
func (b *MsgBus) PublishFill(f domain.Fill) {
	for _, o := range b.fillObs {
		o.OnFill(f)
	}
}

// PublishAccountState delivers s to every account-state observer in order.
func (b *MsgBus) PublishAccountState(s AccountState) {
	for _, o := range b.acctObs {
		o.OnAccountState(s)
	}
}

// PublishBar delivers bar to every bar observer in registration order.
func (b *MsgBus) PublishBar(bar domain.Bar) {
	for _, o := range b.barObs {
		o.OnBar(bar)
	}
}
