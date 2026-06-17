package livengine

// intent.go gives the word "intent" its true home (concept B, design
// intent-to-signal-rename.md §3.2): the IntentRecord the NoopExecutor produces
// for every would-be order in signal mode (side+qty — "a single order the
// strategy WOULD have placed"), and the IntentSink the session flushes them to
// per timestamp, PARALLEL to the SignalSink (§3.2). A SignalRecord is a
// judgment/diagnostic snapshot ("what is the strategy thinking"); an
// IntentRecord is an executable order intent ("the order it would place").
//
// DEFERRED (design §8 Q1 default): this round wires the memory/telemetry layer
// ONLY. The DB table tms.intents and the Redis data.IntentUpdate topic
// (symmetric to tms.signals / data.SignalUpdate) are NOT added here; their
// exposure is deferred to a later round, decided by cockpit need.

import (
	"context"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// IntentRecord is one "would-be, unsubmitted" order intent (a signal-mode
// product). In auto mode the same intent becomes a real domain.Order; in signal
// mode it is only recorded. It is the structural home of concept B: side+qty,
// distinct from a SignalRecord's opaque judgment payload.
type IntentRecord struct {
	// StrategyID is the engine strategy id (allocator key), the same tag a
	// SignalRecord carries.
	StrategyID string
	// AsOf is the bar timestamp the intent was evaluated at (UTC).
	AsOf time.Time
	// Symbol is the instrument the order would target.
	Symbol string
	// Side is the strategy-level direction LONG / SHORT / FLAT (domain.SignalSide,
	// design §8 Q3): FLAT is a close-to-flat intent whose concrete BUY/SELL is
	// resolved at execution time against the live net position, so SignalSide —
	// not OrderSide — is the faithful intent-layer type.
	Side domain.SignalSide
	// Qty is the order quantity the strategy would submit.
	Qty domain.Qty
}

// IntentSink receives the live engine's would-be order intents, PARALLEL to the
// SignalSink. The session calls EmitIntent once per recorded intent during a
// timestamp's flush (after the strategies have run OnBar), in capture order.
// Implementations must be safe to call from the single dispatch goroutine; a
// returned error aborts the run (a persistence failure stops the node rather
// than silently dropping intents). MemSink implements it for the test +
// consistency proof; DiscardSink drops every intent.
type IntentSink interface {
	EmitIntent(ctx context.Context, rec IntentRecord) error
}

// EmitIntent discards rec.
func (DiscardSink) EmitIntent(context.Context, IntentRecord) error { return nil }

// EmitIntent appends rec (in capture order — the consistency proof relies on a
// streaming run and a batch replay recording IDENTICAL intent slices).
func (s *MemSink) EmitIntent(_ context.Context, rec IntentRecord) error {
	s.Intents = append(s.Intents, rec)
	return nil
}

// SortedIntentRecords returns the recorded would-be order intents in a canonical
// order (AsOf, then StrategyID, then Symbol, then Side) so two runs that emit
// at-a-timestamp in a goroutine-independent-but-equal order still compare equal.
// Within a single deterministic run the capture order is already canonical; this
// helper makes cross-run comparison robust (mirrors SortedIntents for signals).
func (s *MemSink) SortedIntentRecords() []IntentRecord {
	out := make([]IntentRecord, len(s.Intents))
	copy(out, s.Intents)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AsOf.Equal(out[j].AsOf) {
			return out[i].AsOf.Before(out[j].AsOf)
		}
		if out[i].StrategyID != out[j].StrategyID {
			return out[i].StrategyID < out[j].StrategyID
		}
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Side < out[j].Side
	})
	return out
}
