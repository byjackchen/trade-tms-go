package accounting

// sampler.go is the equity-curve sampler, the Go port of the Python
// EquityCurveSamplerActor. On each daily bar it records, per strategy, the
// strategy's total PnL (realized + unrealized for open positions; flat
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
// receive a point carrying their realized PnL (matching the Python actor, which
// samples every tracked strategy each bar).
func (s *EquitySampler) Sample(ts time.Time) error {
	// Discover strategies from the account's position book and aggregate.
	perStrat := make(map[string]domain.Money)
	for _, pos := range s.acct.AllPositions() {
		s.strategyIDs[pos.StrategyID] = struct{}{}
	}
	// Aggregate realized + unrealized per strategy deterministically.
	for _, sid := range s.sortedStrategyIDs() {
		var pnl domain.Money
		for _, key := range s.acct.sortedKeys() {
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
	ids := make([]string, 0, len(s.strategyIDs))
	for id := range s.strategyIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
