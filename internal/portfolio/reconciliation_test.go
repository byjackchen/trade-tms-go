package portfolio

import (
	"strings"
	"testing"
	"time"
)

func recTS() time.Time { return time.Date(2024, 6, 28, 21, 0, 0, 0, time.UTC) }

func books(pairs ...interface{}) map[PositionKey]int64 {
	m := map[PositionKey]int64{}
	for i := 0; i+2 < len(pairs)+1; i += 3 {
		m[PositionKey{pairs[i].(string), pairs[i+1].(string)}] = int64(pairs[i+2].(int))
	}
	return m
}

func brokerMap(pairs ...interface{}) map[string]int64 {
	m := map[string]int64{}
	for i := 0; i+1 < len(pairs)+1; i += 2 {
		m[pairs[i].(string)] = int64(pairs[i+1].(int))
	}
	return m
}

func TestReconcileFullMatch(t *testing.T) {
	rep := Reconcile(recTS(),
		books("SEPA", "AAPL", 100, "Pairs", "KO", 50, "Pairs", "PEP", -40),
		brokerMap("AAPL", 100, "KO", 50, "PEP", -40), 0)
	if rep.HasIssues() {
		t.Fatalf("expected clean, got %+v", rep)
	}
	want := map[string]bool{"AAPL": true, "KO": true, "PEP": true}
	if len(rep.Matched) != 3 {
		t.Fatalf("matched %v", rep.Matched)
	}
	for _, s := range rep.Matched {
		if !want[s] {
			t.Fatalf("unexpected matched %s", s)
		}
	}
	// Deterministic sorted ordering.
	if rep.Matched[0] != "AAPL" || rep.Matched[1] != "KO" || rep.Matched[2] != "PEP" {
		t.Fatalf("matched not sorted: %v", rep.Matched)
	}
}

func TestReconcileMismatchDiffSign(t *testing.T) {
	// books 100, broker 95 -> diff = broker - books = -5.
	rep := Reconcile(recTS(), books("SEPA", "AAPL", 100), brokerMap("AAPL", 95), 0)
	if !rep.HasIssues() || len(rep.Mismatches) != 1 {
		t.Fatalf("expected one mismatch, got %+v", rep)
	}
	m := rep.Mismatches[0]
	if m.Symbol != "AAPL" || m.StrategyBooksSum != 100 || m.BrokerNet != 95 || m.Diff != -5 {
		t.Fatalf("mismatch fields: %+v", m)
	}
	if m.DiffShares() != 5 {
		t.Fatalf("diff_shares: %d want 5", m.DiffShares())
	}
}

func TestReconcilePairsSignedSum(t *testing.T) {
	// +100 KO + (-40) KO -> net 60 == broker 60.
	rep := Reconcile(recTS(),
		books("Pairs1", "KO", 100, "Pairs2", "KO", -40),
		brokerMap("KO", 60), 0)
	if rep.HasIssues() {
		t.Fatalf("pairs net should match: %+v", rep)
	}
	if len(rep.Matched) != 1 || rep.Matched[0] != "KO" {
		t.Fatalf("matched %v", rep.Matched)
	}
}

func TestReconcileOnlyInStrategies(t *testing.T) {
	// AAPL claimed by strategy, broker shows 0; MSFT broker 0 + no claim -> matched.
	rep := Reconcile(recTS(),
		books("SEPA", "AAPL", 100),
		brokerMap("AAPL", 0, "MSFT", 0), 0)
	if len(rep.SymbolsOnlyInStrategies) != 1 || rep.SymbolsOnlyInStrategies[0] != "AAPL" {
		t.Fatalf("only-in-strategies: %v", rep.SymbolsOnlyInStrategies)
	}
	// MSFT: s_sum 0, b_net 0 -> matched (broker-explicit zero counts).
	foundMSFT := false
	for _, s := range rep.Matched {
		if s == "MSFT" {
			foundMSFT = true
		}
	}
	if !foundMSFT {
		t.Fatalf("MSFT (0/0) should be in matched: %v", rep.Matched)
	}
	if !rep.HasIssues() {
		t.Fatal("AAPL-only must flag issues")
	}
}

func TestReconcileOnlyAtBroker(t *testing.T) {
	rep := Reconcile(recTS(), books(), brokerMap("AAPL", 100), 0)
	if len(rep.SymbolsOnlyAtBroker) != 1 || rep.SymbolsOnlyAtBroker[0] != "AAPL" {
		t.Fatalf("only-at-broker: %v", rep.SymbolsOnlyAtBroker)
	}
	if !rep.HasIssues() {
		t.Fatal("broker-only must flag issues")
	}
}

func TestReconcileZeroBookSkipped(t *testing.T) {
	// 0-share book entry is not a claimed position; KO matches cleanly.
	rep := Reconcile(recTS(),
		books("SEPA", "AAPL", 0, "Pairs", "KO", 50),
		brokerMap("KO", 50), 0)
	if rep.HasIssues() {
		t.Fatalf("zero-book entry must be ignored: %+v", rep)
	}
	if len(rep.Matched) != 1 || rep.Matched[0] != "KO" {
		t.Fatalf("matched %v", rep.Matched)
	}
}

func TestReconcileTolerance(t *testing.T) {
	// books 100, broker 99, tol 2 -> |diff|=1 <= 2 -> matched.
	rep := Reconcile(recTS(), books("SEPA", "AAPL", 100), brokerMap("AAPL", 99), 2)
	if rep.HasIssues() {
		t.Fatalf("tolerance should absorb diff 1: %+v", rep)
	}
	if len(rep.Matched) != 1 || rep.Matched[0] != "AAPL" {
		t.Fatalf("matched %v", rep.Matched)
	}
	// Boundary: exactly at tolerance is inclusive.
	rep2 := Reconcile(recTS(), books("SEPA", "AAPL", 100), brokerMap("AAPL", 98), 2)
	if rep2.HasIssues() {
		t.Fatal("|diff|=2 == tol must match (inclusive <=)")
	}
	// One past tolerance -> mismatch.
	rep3 := Reconcile(recTS(), books("SEPA", "AAPL", 100), brokerMap("AAPL", 97), 2)
	if !rep3.HasIssues() || len(rep3.Mismatches) != 1 {
		t.Fatalf("|diff|=3 > tol must mismatch: %+v", rep3)
	}
}

func TestReconcileSummaryClean(t *testing.T) {
	rep := Reconcile(recTS(), books("SEPA", "AAPL", 100), brokerMap("AAPL", 100), 0)
	s := rep.Summary()
	if !strings.Contains(s, "OK") {
		t.Fatalf("clean summary: %q", s)
	}
	if !strings.Contains(s, "1 symbols matched") {
		t.Fatalf("clean count: %q", s)
	}
}

func TestReconcileSummaryIssues(t *testing.T) {
	rep := Reconcile(recTS(),
		books("SEPA", "AAPL", 100, "SEPA", "ONLY", 10),
		brokerMap("AAPL", 95, "BRK", 5), 0)
	s := rep.Summary()
	for _, sub := range []string{"Reconciliation report @ 2024-06-28T21:00:00+00:00", "Mismatches", "AAPL", "diff", "ONLY", "BRK"} {
		if !strings.Contains(s, sub) {
			t.Fatalf("summary missing %q in:\n%s", sub, s)
		}
	}
	// Signed format: strategies sum +100, broker +95, diff -5.
	if !strings.Contains(s, "strategies sum +100, broker +95, diff -5") {
		t.Fatalf("signed line missing:\n%s", s)
	}
}
