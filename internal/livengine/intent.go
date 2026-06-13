package livengine

// intent.go defines the live-engine emission seams: the SignalIntent record the
// NoopExecutor produces per (strategy, as-of), the PortfolioHealth snapshot it
// emits alongside, and the IntentSink the session emits both to. Build1 ships an
// in-memory recorder (the test + consistency-proof seam); Build2 implements the
// DB upsert (live.signal_intents, idempotent on (strategy_id, symbol, as_of))
// and the Redis stream publisher behind the same IntentSink.

import (
	"context"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// IntentRecord is one emitted strategy signal: the raw JSON-serializable intent
// payload the strategy's IntentEvaluator produced (a SignalIntent or a slice of
// per-leg/per-name intents), tagged with the engine strategy id and the as-of
// timestamp it was evaluated at. The Payload is exactly what flows to
// live.signal_intents.intent (Build2) and the Redis data.signal_intents stream
// (api-ws-redis.md §5.9); it is held opaque here so livengine does not couple to
// each strategy's intent shape.
type IntentRecord struct {
	// StrategyID is the engine strategy id (e.g. "SEPA-UNIVERSE-001"), the
	// allocator key — distinct from the logical strategy_id inside the payload.
	StrategyID string
	// AsOf is the bar timestamp the intent was evaluated at (UTC), the third key
	// of the live.signal_intents idempotency tuple (strategy_id, symbol, as_of).
	AsOf time.Time
	// Payload is the strategy's evaluate_intent result (opaque, JSON-serializable).
	Payload any
}

// HealthRecord is one PortfolioHealth snapshot emitted after a timestamp's
// intents, mirroring the PortfolioHealthActor's per-cadence publish
// (portfolio-risk.md). In signal mode there are no positions, so DayPnL is 0 and
// DailyLossHalt is always false (informational, decision 6); the field is still
// emitted so the cockpit health panel has continuity.
type HealthRecord struct {
	AsOf     time.Time
	Snapshot portfolio.PortfolioHealthSnapshot
}

// IntentSink receives the live engine's emissions. The session calls EmitIntent
// once per (strategy, as-of) after a bar timestamp's strategies have run, and
// EmitHealth once per timestamp. Implementations must be safe to call from the
// single dispatch goroutine. A returned error aborts the run (so a persistence
// failure stops the node rather than silently dropping signals).
//
// Build1 implementations: MemSink (tests / consistency proof) and the no-op
// DiscardSink. Build2 adds the DB-upsert + Redis-publish sink.
type IntentSink interface {
	EmitIntent(ctx context.Context, rec IntentRecord) error
	EmitHealth(ctx context.Context, rec HealthRecord) error
}

// StateRecord is one strategy's state_summary snapshot emitted alongside its
// intents (the StrategyStateUpdate path, api-ws-redis.md §5.8). EOD decision 4
// requires "evaluate_intent + state_summary"; this is the state_summary half.
type StateRecord struct {
	// StrategyID is the engine strategy id (allocator key).
	StrategyID string
	// AsOf is the bar timestamp the summary was taken at (UTC).
	AsOf time.Time
	// Summary is the strategy's StateSummaryJSON result (opaque, JSON-serializable).
	Summary any
}

// StateEmitter is an OPTIONAL capability of an IntentSink: a sink that also
// wants per-timestamp strategy state_summary snapshots implements it, and the
// session probes for it via a type assertion. Sinks that do not (MemSink,
// DiscardSink) are simply not asked — the intent path is unchanged.
type StateEmitter interface {
	EmitState(ctx context.Context, rec StateRecord) error
}

// DiscardSink drops every emission. It is the default when no sink is wired
// (e.g. a dry-run that only exercises strategy state).
type DiscardSink struct{}

// EmitIntent discards rec.
func (DiscardSink) EmitIntent(context.Context, IntentRecord) error { return nil }

// EmitHealth discards rec.
func (DiscardSink) EmitHealth(context.Context, HealthRecord) error { return nil }

// MemSink records every emission in memory, in emission order. It is the
// deterministic test + consistency-proof seam: a streaming run and a batch
// replay over the same bars must produce equal Intents slices (compared by the
// session's CanonicalIntents helper). Not safe for concurrent use (the live
// loop is single-goroutine).
type MemSink struct {
	Intents []IntentRecord
	Health  []HealthRecord
}

// NewMemSink returns an empty in-memory sink.
func NewMemSink() *MemSink { return &MemSink{} }

// EmitIntent appends rec.
func (s *MemSink) EmitIntent(_ context.Context, rec IntentRecord) error {
	s.Intents = append(s.Intents, rec)
	return nil
}

// EmitHealth appends rec.
func (s *MemSink) EmitHealth(_ context.Context, rec HealthRecord) error {
	s.Health = append(s.Health, rec)
	return nil
}

// SortedIntents returns the recorded intents in a canonical order
// (AsOf, then StrategyID) so two runs that may emit at-a-timestamp in a
// different goroutine-independent-but-equal order still compare equal. Within a
// single deterministic run the emission order is already canonical; this helper
// makes cross-run comparison robust.
func (s *MemSink) SortedIntents() []IntentRecord {
	out := make([]IntentRecord, len(s.Intents))
	copy(out, s.Intents)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AsOf.Equal(out[j].AsOf) {
			return out[i].AsOf.Before(out[j].AsOf)
		}
		return out[i].StrategyID < out[j].StrategyID
	})
	return out
}
