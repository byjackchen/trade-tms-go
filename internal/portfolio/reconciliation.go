package portfolio

// reconciliation.go ports src/portfolio/reconciliation.py (spec §6
// [MUST-MATCH]): the EOD check that sum(strategy books) == broker net per
// symbol. Pure data module — the caller supplies both sides and consumes the
// report (log / alert / halt); this module never acts on has_issues.
//
// Independent positions (Eng-D3): each strategy keeps its own book; the broker
// truth is the NET position. A drift between the two means a missed fill,
// double-counted close, or manual edit.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Mismatch is one symbol where the strategy-book sum disagrees with broker net
// (reconciliation.py:18-29). diff is broker_net - strategy_books_sum (sign
// matters: books 100 / broker 95 -> diff -5).
type Mismatch struct {
	Symbol           string
	StrategyBooksSum int64 // signed sum across strategies' tracked positions
	BrokerNet        int64 // signed broker view
	Diff             int64 // BrokerNet - StrategyBooksSum
}

// DiffShares returns |Diff| (reconciliation.py:27-29).
func (m Mismatch) DiffShares() int64 { return absInt64(m.Diff) }

// ReconciliationReport is the per-symbol classification of a reconcile run
// (reconciliation.py:32-69). The slices are deterministically ordered: symbols
// are processed in lexicographic ascending order.
type ReconciliationReport struct {
	TS                      time.Time
	Matched                 []string   // symbols within tolerance
	Mismatches              []Mismatch // non-zero diffs beyond tolerance
	SymbolsOnlyInStrategies []string   // strategies claim; broker shows zero
	SymbolsOnlyAtBroker     []string   // broker shows; no strategy claims
}

// HasIssues reports whether any of the three problem lists is non-empty
// (reconciliation.py:40-46).
func (r ReconciliationReport) HasIssues() bool {
	return len(r.Mismatches) > 0 ||
		len(r.SymbolsOnlyInStrategies) > 0 ||
		len(r.SymbolsOnlyAtBroker) > 0
}

// Summary renders the human-facing report text (reconciliation.py:48-69,
// spec §6.1 [MUST-MATCH]). Clean: "Reconciliation OK (N symbols matched)".
// Issues: a header line with the RFC3339-ish timestamp, then the mismatch
// block, then the one-sided blocks; lines joined with "\n". Signed ints use the
// "%+d" format (always-signed).
func (r ReconciliationReport) Summary() string {
	if !r.HasIssues() {
		return fmt.Sprintf("Reconciliation OK (%d symbols matched)", len(r.Matched))
	}
	lines := []string{fmt.Sprintf("Reconciliation report @ %s", isoFormat(r.TS))}
	if len(r.Mismatches) > 0 {
		lines = append(lines, fmt.Sprintf("  Mismatches (%d):", len(r.Mismatches)))
		for _, m := range r.Mismatches {
			lines = append(lines, fmt.Sprintf(
				"    %s: strategies sum %+d, broker %+d, diff %+d",
				m.Symbol, m.StrategyBooksSum, m.BrokerNet, m.Diff))
		}
	}
	if len(r.SymbolsOnlyInStrategies) > 0 {
		lines = append(lines, "  Strategies claim positions, broker shows zero: "+
			strings.Join(r.SymbolsOnlyInStrategies, ", "))
	}
	if len(r.SymbolsOnlyAtBroker) > 0 {
		lines = append(lines, "  Broker shows positions, no strategy claims them: "+
			strings.Join(r.SymbolsOnlyAtBroker, ", "))
	}
	return strings.Join(lines, "\n")
}

// isoFormat renders ts like Python datetime.isoformat() for a UTC-aware
// datetime (e.g. "2024-06-28T21:00:00+00:00"). Only used for the issues header.
func isoFormat(ts time.Time) string {
	u := ts.UTC()
	base := u.Format("2006-01-02T15:04:05")
	if ns := u.Nanosecond(); ns != 0 {
		// Python prints microseconds when sub-second is present.
		base += fmt.Sprintf(".%06d", ns/1000)
	}
	return base + "+00:00"
}

// Reconcile compares aggregated strategy books vs broker net per symbol
// (reconciliation.py:72-131, spec §6.2 [MUST-MATCH]).
//
// strategyBooks[(strategy_id, symbol)] = signed shares; broker[symbol] = signed
// broker net. toleranceShares absorbs tiny diffs (inclusive <=).
//
// Steps:
//  1. Aggregate strategy books per symbol, summing signed shares, SKIPPING
//     entries with qty == 0 (a 0-share entry is not a claimed position).
//  2. Iterate the union of symbols in sorted ascending order.
//  3. Per symbol classify with this priority (first match wins):
//     a. s_sum != 0 && b_net == 0 -> only-in-strategies
//     b. s_sum == 0 && b_net != 0 -> only-at-broker
//     c. |diff| <= tolerance       -> matched (incl. the s_sum==0,b_net==0 flat case)
//     d. else                      -> mismatch
func Reconcile(ts time.Time, strategyBooks map[PositionKey]int64, broker map[string]int64, toleranceShares int64) ReconciliationReport {
	strategySums := make(map[string]int64)
	for k, qty := range strategyBooks {
		if qty == 0 {
			continue
		}
		strategySums[k.Symbol] += qty
	}

	symSet := make(map[string]struct{}, len(strategySums)+len(broker))
	for sym := range strategySums {
		symSet[sym] = struct{}{}
	}
	for sym := range broker {
		symSet[sym] = struct{}{}
	}
	symbols := make([]string, 0, len(symSet))
	for sym := range symSet {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	report := ReconciliationReport{TS: ts}
	for _, sym := range symbols {
		sSum := strategySums[sym] // 0 if absent
		bNet := broker[sym]       // 0 if absent
		diff := bNet - sSum

		switch {
		case sSum != 0 && bNet == 0:
			report.SymbolsOnlyInStrategies = append(report.SymbolsOnlyInStrategies, sym)
		case sSum == 0 && bNet != 0:
			report.SymbolsOnlyAtBroker = append(report.SymbolsOnlyAtBroker, sym)
		case absInt64(diff) <= toleranceShares:
			report.Matched = append(report.Matched, sym)
		default:
			report.Mismatches = append(report.Mismatches, Mismatch{
				Symbol:           sym,
				StrategyBooksSum: sSum,
				BrokerNet:        bNet,
				Diff:             diff,
			})
		}
	}
	return report
}
