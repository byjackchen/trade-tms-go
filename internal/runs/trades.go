package runs

// trades.go derives round-trip trades from the engine's fill stream for the
// research.trades table (DB source of truth). A trade is one open->close cycle
// of a (strategy_id, symbol) position: it opens when the net position leaves
// flat and closes when it returns to flat (NETTING OMS, one position per
// (strategy, instrument) — domain-types-money §7.4). A reversal (e.g. long ->
// short in one fill) closes the current trade at the boundary and opens a new
// one for the residual, so every trade has a single, unambiguous side.
//
// PnL: realized PnL accrues against the running average entry price using the
// same fixed-point money arithmetic the engine uses (round half-to-even at
// 1e-4), so a trade's RealizedPnL matches the account's realized delta over the
// trade's lifetime.

import (
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Trade is one round-trip trade for research.trades.
type Trade struct {
	StrategyID  string
	Symbol      string
	Side        string        // "LONG" | "SHORT"
	Qty         domain.Qty    // absolute peak size of the round trip
	EntryTS     time.Time     // first fill that opened the position
	ExitTS      *time.Time    // nil while still open at run end
	EntryPx     domain.Price  // average entry price
	ExitPx      *domain.Price // average exit price; nil while still open
	RealizedPnL domain.Money  // realized over the trade lifetime
}

// ExtractTrades walks the fills in settlement order and emits round-trip trades
// per (strategy, symbol), ordered by (strategy_id, symbol, entry_ts).
func ExtractTrades(fills []domain.Fill) ([]Trade, error) {
	type key struct{ strat, sym string }
	open := make(map[key]*openTrade)
	var out []Trade
	var openKeys []key // preserve first-seen order for stable open-trade output

	for _, f := range fills {
		k := key{f.StrategyID, f.Symbol}
		signed := f.Qty
		if f.Side == domain.OrderSideSell {
			signed = -signed
		}
		ot := open[k]
		if ot == nil {
			ot = &openTrade{strat: f.StrategyID, sym: f.Symbol}
			open[k] = ot
			openKeys = append(openKeys, k)
		}
		closed, err := ot.apply(signed, f.Price, f.TS)
		if err != nil {
			return nil, fmt.Errorf("runs: extracting trades for %s/%s: %w", f.StrategyID, f.Symbol, err)
		}
		out = append(out, closed...)
		if ot.flat() {
			delete(open, k)
		}
	}
	for _, k := range openKeys {
		if ot, ok := open[k]; ok {
			out = append(out, ot.snapshotOpen())
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StrategyID != out[j].StrategyID {
			return out[i].StrategyID < out[j].StrategyID
		}
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].EntryTS.Before(out[j].EntryTS)
	})
	return out, nil
}

// openTrade is the running state of one (strategy, symbol) round trip.
type openTrade struct {
	strat, sym  string
	signed      domain.Qty   // current signed net position (0 = flat)
	peakAbs     domain.Qty   // peak absolute size reached this round trip
	entryNote   domain.Money // entry notional of the open side (|signed| * avgEntry, exact)
	avgEntry    domain.Price // current average entry price
	openedShort bool         // the round trip opened on the short side
	openNote    domain.Money // cumulative notional of all opening (increasing) fills
	openQty     domain.Qty   // cumulative absolute qty of all opening fills
	entryTS     time.Time    // first fill ts of the round trip
	lastTS      time.Time    // ts of the most recent fill
	realized    domain.Money // realized PnL accrued this round trip
	lastExitPx  domain.Price // last price at which size was reduced (the exit px)
	reduced     bool         // whether any reduction (exit) has occurred
}

func (o *openTrade) flat() bool { return o.signed == 0 }

// apply applies a signed fill delta at price px and ts, returning any trades
// that completed (a round trip returning to flat). A reversal yields one
// completed trade plus a freshly opened residual.
func (o *openTrade) apply(delta domain.Qty, px domain.Price, ts time.Time) ([]Trade, error) {
	if delta == 0 {
		return nil, nil
	}
	var done []Trade
	if o.signed == 0 {
		o.openNew(delta, px, ts)
		return nil, nil
	}
	o.lastTS = ts
	sameDir := (o.signed > 0) == (delta > 0)
	if sameDir {
		// Increase position: blend the average entry price.
		if err := o.increase(delta, px); err != nil {
			return nil, err
		}
		return nil, nil
	}
	// Opposite direction: reduce (and possibly reverse).
	closeQty := absQty(delta)
	curAbs := absQty(o.signed)
	if closeQty <= curAbs {
		if err := o.reduce(closeQty, px); err != nil {
			return nil, err
		}
		if o.signed == 0 {
			done = append(done, o.complete(ts))
			o.reset()
		}
		return done, nil
	}
	// Reversal: close the whole current side, then open residual on the other.
	if err := o.reduce(curAbs, px); err != nil {
		return nil, err
	}
	done = append(done, o.complete(ts))
	residual := closeQty - curAbs
	o.reset()
	signedResidual := residual
	if delta < 0 {
		signedResidual = -residual
	}
	o.openNew(signedResidual, px, ts)
	return done, nil
}

func (o *openTrade) openNew(signed domain.Qty, px domain.Price, ts time.Time) {
	o.signed = signed
	o.avgEntry = px
	o.openedShort = signed < 0
	abs := absQty(signed)
	o.peakAbs = abs
	note, _ := px.MulQty(abs)
	o.entryNote = note
	o.openNote = note
	o.openQty = abs
	o.entryTS = ts
	o.lastTS = ts
	o.realized = 0
	o.reduced = false
}

func (o *openTrade) increase(delta domain.Qty, px domain.Price) error {
	addAbs := absQty(delta)
	addNote, err := px.MulQty(addAbs)
	if err != nil {
		return err
	}
	o.signed += delta
	newAbs := absQty(o.signed)
	if newAbs > o.peakAbs {
		o.peakAbs = newAbs
	}
	o.entryNote, err = o.entryNote.Add(addNote)
	if err != nil {
		return err
	}
	o.openNote, err = o.openNote.Add(addNote)
	if err != nil {
		return err
	}
	o.openQty += addAbs
	o.avgEntry = avgPrice(o.entryNote, newAbs)
	return nil
}

// reduce removes closeAbs shares at price px, realizing PnL against the average
// entry, and shrinks the entry notional proportionally.
func (o *openTrade) reduce(closeAbs domain.Qty, px domain.Price) error {
	wasLong := o.signed > 0
	// realized = (exit - entry) * qty for long; (entry - exit) * qty for short.
	var perShare domain.Price
	var err error
	if wasLong {
		perShare, err = px.Sub(o.avgEntry)
	} else {
		perShare, err = o.avgEntry.Sub(px)
	}
	if err != nil {
		return err
	}
	pnl, err := perShare.MulQty(closeAbs)
	if err != nil {
		return err
	}
	o.realized, err = o.realized.Add(pnl)
	if err != nil {
		return err
	}
	// Shrink entry notional by the closed fraction at the average entry price.
	closedEntryNote, err := o.avgEntry.MulQty(closeAbs)
	if err != nil {
		return err
	}
	o.entryNote, err = o.entryNote.Sub(closedEntryNote)
	if err != nil {
		return err
	}
	if wasLong {
		o.signed -= closeAbs
	} else {
		o.signed += closeAbs
	}
	o.lastExitPx = px
	o.reduced = true
	return nil
}

// complete materializes the finished round trip as a Trade.
func (o *openTrade) complete(ts time.Time) Trade {
	side := "LONG"
	if o.openedShort {
		side = "SHORT"
	}
	exitPx := o.lastExitPx
	exitTS := ts
	return Trade{
		StrategyID:  o.strat,
		Symbol:      o.sym,
		Side:        side,
		Qty:         o.peakAbs,
		EntryTS:     o.entryTS,
		ExitTS:      &exitTS,
		EntryPx:     avgPrice(o.openNote, o.openQty),
		ExitPx:      &exitPx,
		RealizedPnL: o.realized,
	}
}

// snapshotOpen materializes a still-open position at run end (no exit).
func (o *openTrade) snapshotOpen() Trade {
	side := "LONG"
	if o.openedShort {
		side = "SHORT"
	}
	return Trade{
		StrategyID:  o.strat,
		Symbol:      o.sym,
		Side:        side,
		Qty:         o.peakAbs,
		EntryTS:     o.entryTS,
		ExitTS:      nil,
		EntryPx:     avgPrice(o.openNote, o.openQty),
		ExitPx:      nil,
		RealizedPnL: o.realized,
	}
}

func (o *openTrade) reset() {
	o.signed = 0
	o.peakAbs = 0
	o.entryNote = 0
	o.openNote = 0
	o.openQty = 0
	o.avgEntry = 0
	o.openedShort = false
	o.realized = 0
	o.reduced = false
	o.lastExitPx = 0
}

func absQty(q domain.Qty) domain.Qty {
	if q < 0 {
		return -q
	}
	return q
}

// avgPrice returns notional / absQty rounded half-to-even at 1e-4. This
// replicates the engine's Position.AvgEntryPrice (accounting/position.go), so a
// trade's entry/exit price reconciles with the position snapshot.
func avgPrice(notional domain.Money, absQty domain.Qty) domain.Price {
	if absQty == 0 {
		return 0
	}
	return domain.Price(roundHalfEvenDiv(int64(notional), int64(absQty)))
}

// roundHalfEvenDiv divides v by d rounding the remainder half-to-even. Mirrors
// the unexported domain helper of the same name so trade prices match the
// engine's position prices exactly.
func roundHalfEvenDiv(v, d int64) int64 {
	q := v / d
	r := v % d
	if r == 0 {
		return q
	}
	ar := r
	if ar < 0 {
		ar = -ar
	}
	half := d / 2
	if ar > half || (ar == half && q%2 != 0) {
		if v < 0 {
			q--
		} else {
			q++
		}
	}
	return q
}
