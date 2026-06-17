package accounting

// sampler.go is the equity-curve sampler. On each daily bar it records, per
// strategy, the strategy's total PnL (realized + unrealized for open
// positions; flat
// strategies contribute realized only) and the account's total equity. The
// samples back the runs artifacts strategy_equity/{id}.json (per-strategy
// cumulative PnL, §7.7) and account.json (total equity curve, §7.4).

import (
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// EquityPoint is one sampled (ts, value) pair. Value is interpreted by the
// curve it belongs to: USD cumulative PnL for a per-strategy curve, USD account
// equity for the total curve.
type EquityPoint struct {
	TS    time.Time
	Value domain.Money
}

// EquitySampler accumulates per-strategy PnL curves and a total-equity curve.
// Sample once per daily bar timestamp (the engine triggers it on a SampleEvent
// after all bars and fills at that ts have settled). Not safe for concurrent
// use.
type EquitySampler struct {
	acct *Account

	strategyIDs map[string]struct{}      // every strategy seen
	perStrategy map[string][]EquityPoint // strategy_id -> cumulative PnL curve
	total       []EquityPoint            // account equity curve

	// Reusable scratch buffers for the hot per-bar Sample path, so it does not
	// allocate a fresh sorted-key / sorted-id slice every sample (rebuilt in
	// place each call). NOT safe for concurrent use — Sample is single-goroutine.
	keyScratch []domain.StrategySymbol
	idScratch  []string
}

// NewEquitySampler binds a sampler to an account.
func NewEquitySampler(acct *Account) *EquitySampler {
	return &EquitySampler{
		acct:        acct,
		strategyIDs: make(map[string]struct{}),
		perStrategy: make(map[string][]EquityPoint),
	}
}

// Sample records one point per known strategy and one total-equity point at ts.
// Strategy total PnL = realized + (unrealized for open positions). A strategy
// is "known" once any position has been opened under it; flat strategies still
// receive a point carrying their realized PnL (every tracked strategy is
// sampled each bar).
func (s *EquitySampler) Sample(ts time.Time) error {
	// Discover strategies from the account's position book and aggregate.
	perStrat := make(map[string]domain.Money)
	for key := range s.acct.positions {
		s.strategyIDs[key.StrategyID] = struct{}{}
	}
	// Sort the position keys ONCE into the reusable scratch buffer; the
	// per-strategy aggregation below scans this same sorted slice (sorted
	// strategy outer, sorted key inner — the identical deterministic order the
	// previous per-strategy sortedKeys() call produced).
	s.keyScratch = s.acct.sortedKeysInto(s.keyScratch)
	// Aggregate realized + unrealized per strategy deterministically.
	s.idScratch = s.sortedStrategyIDsInto(s.idScratch)
	for _, sid := range s.idScratch {
		var pnl domain.Money
		for _, key := range s.keyScratch {
			if key.StrategyID != sid {
				continue
			}
			p := s.acct.positions[key]
			r := p.RealizedPnL()
			acc, err := pnl.Add(r)
			if err != nil {
				return fmt.Errorf("sampler realized %s: %w", sid, err)
			}
			pnl = acc
			if !p.IsFlat() {
				if last, ok := s.acct.lastPrice[p.Symbol()]; ok {
					u, err := p.UnrealizedPnL(last)
					if err != nil {
						return err
					}
					pnl, err = pnl.Add(u)
					if err != nil {
						return fmt.Errorf("sampler unrealized %s: %w", sid, err)
					}
				}
			}
		}
		perStrat[sid] = pnl
	}
	for sid, pnl := range perStrat {
		s.perStrategy[sid] = append(s.perStrategy[sid], EquityPoint{TS: ts, Value: pnl})
	}

	equity, err := s.acct.Equity()
	if err != nil {
		return err
	}
	s.total = append(s.total, EquityPoint{TS: ts, Value: equity})
	return nil
}

// TotalCurve returns the account-equity curve in sample order.
func (s *EquitySampler) TotalCurve() []EquityPoint {
	out := make([]EquityPoint, len(s.total))
	copy(out, s.total)
	return out
}

// StrategyCurve returns the cumulative-PnL curve for strategyID (nil if none).
func (s *EquitySampler) StrategyCurve(strategyID string) []EquityPoint {
	c := s.perStrategy[strategyID]
	out := make([]EquityPoint, len(c))
	copy(out, c)
	return out
}

// StrategyIDs returns every sampled strategy id, sorted.
func (s *EquitySampler) StrategyIDs() []string { return s.sortedStrategyIDs() }

func (s *EquitySampler) sortedStrategyIDs() []string {
	return s.sortedStrategyIDsInto(make([]string, 0, len(s.strategyIDs)))
}

// sortedStrategyIDsInto fills dst (reset to len 0, capacity reused) with every
// sampled strategy id in sorted order and returns it. The result aliases dst's
// backing array; the hot Sample path passes its reusable scratch buffer to
// avoid a per-sample allocation while keeping the identical sorted order.
func (s *EquitySampler) sortedStrategyIDsInto(dst []string) []string {
	dst = dst[:0]
	for id := range s.strategyIDs {
		dst = append(dst, id)
	}
	sort.Strings(dst)
	return dst
}
