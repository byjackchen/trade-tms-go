package livengine

// signal.go defines the live-engine emission seams: the Signal record the
// NoopExecutor produces per (strategy, as-of), the PortfolioHealth snapshot it
// emits alongside, and the SignalSink the session emits both to. Build1 ships an
// in-memory recorder (the test + consistency-proof seam); Build2 implements the
// DB upsert (tms.signals, idempotent on (strategy_id, symbol, as_of))
// and the Redis stream publisher behind the same SignalSink.

import (
	"context"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

// SignalRecord is one emitted strategy signal: the raw JSON-serializable signal
// payload the strategy's SignalEvaluator produced (a Signal or a slice of
// per-leg/per-name signals), tagged with the engine strategy id and the as-of
// timestamp it was evaluated at. The Payload is exactly what flows to
// tms.signals.signal (Build2) and the Redis data.SignalUpdate stream
// (api-ws-redis.md §5.9); it is held opaque here so livengine does not couple to
// each strategy's signal shape.
type SignalRecord struct {
	// StrategyID is the engine strategy id (e.g. "SEPA-UNIVERSE-001"), the
	// allocator key — distinct from the logical strategy_id inside the payload.
	StrategyID string
	// AsOf is the bar timestamp the signal was evaluated at (UTC), the third key
	// of the tms.signals idempotency tuple (strategy_id, symbol, as_of).
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
	Snapshot riskgate.PortfolioHealthSnapshot
}

// SignalSink receives the live engine's emissions. The session calls EmitSignal
// once per (strategy, as-of) after a bar timestamp's strategies have run, and
// EmitHealth once per timestamp. Implementations must be safe to call from the
// single dispatch goroutine. A returned error aborts the run (so a persistence
// failure stops the node rather than silently dropping signals).
//
// Build1 implementations: MemSink (tests / consistency proof) and the no-op
// DiscardSink. Build2 adds the DB-upsert + Redis-publish sink.
type SignalSink interface {
	EmitSignal(ctx context.Context, rec SignalRecord) error
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

// StateEmitter is an OPTIONAL capability of an SignalSink: a sink that also
// wants per-timestamp strategy state_summary snapshots implements it, and the
// session probes for it via a type assertion. Sinks that do not (MemSink,
// DiscardSink) are simply not asked — the intent path is unchanged.
type StateEmitter interface {
	EmitState(ctx context.Context, rec StateRecord) error
}

// DiscardSink drops every emission. It is the default when no sink is wired
// (e.g. a dry-run that only exercises strategy state).
type DiscardSink struct{}

// EmitSignal discards rec.
func (DiscardSink) EmitSignal(context.Context, SignalRecord) error { return nil }

// EmitHealth discards rec.
func (DiscardSink) EmitHealth(context.Context, HealthRecord) error { return nil }

// MemSink records every emission in memory, in emission order. It is the
// deterministic test + consistency-proof seam: a streaming run and a batch
// replay over the same bars must produce equal Signals AND equal Intents slices.
// Not safe for concurrent use (the live loop is single-goroutine). It implements
// SignalSink (EmitSignal/EmitHealth) and IntentSink (EmitIntent, in intent.go).
type MemSink struct {
	Signals []SignalRecord
	Health  []HealthRecord
	// Intents records the NoopExecutor's would-be order intents (concept B),
	// captured per timestamp in flush order — see intent.go.
	Intents []IntentRecord
}

// NewMemSink returns an empty in-memory sink.
func NewMemSink() *MemSink { return &MemSink{} }

// EmitSignal appends rec.
func (s *MemSink) EmitSignal(_ context.Context, rec SignalRecord) error {
	s.Signals = append(s.Signals, rec)
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
func (s *MemSink) SortedIntents() []SignalRecord {
	out := make([]SignalRecord, len(s.Signals))
	copy(out, s.Signals)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AsOf.Equal(out[j].AsOf) {
			return out[i].AsOf.Before(out[j].AsOf)
		}
		return out[i].StrategyID < out[j].StrategyID
	})
	return out
}
