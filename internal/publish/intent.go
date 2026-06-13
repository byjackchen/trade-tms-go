package publish

// intent.go normalizes the heterogeneous strategy intent values returned by
// engine.IntentEvaluator.EvaluateIntentJSON into a single, spec-faithful shape
// the persistence + Redis layers consume.
//
// The four strategies return DIFFERENT Go types from EvaluateIntentJSON:
//
//   - SEPA  -> sepa.SignalIntent          (one per symbol; local type, no json tags)
//   - ORB   -> orb.SignalIntent           (one per symbol; local type, no json tags)
//   - Pairs -> []domain.PairsSignalIntent (2N legs; domain type, snake_case tags)
//   - Sector-> []domain.SectorRotationIntent (one per ETF; domain type, tags)
//
// We MUST persist the canonical snake_case wire shape (api-ws-redis.md §2.6/§5.9
// — the SignalIntentUnion the cockpit decodes), and we MUST derive the
// live.signal_intents discriminator columns (strategy_id, symbol, state,
// strength, proximity_to_trigger_pct, generation) so the row's CHECK
// constraints are satisfied and the UI's (symbol, strategy_id) dedup works.
//
// Rather than reflect over un-tagged local structs (fragile), we convert each
// concrete type explicitly to the matching domain.*SignalIntent (which carries
// the byte-identical Python field names + tags), then marshal THAT. This keeps
// the wire output spec-faithful for every strategy and is the only place that
// knows each strategy's intent shape.

import (
	"encoding/json"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// NormalizedIntent is one strategy signal flattened to the persistence + wire
// contract: the discriminator columns of live.signal_intents plus the full
// snake_case payload object (intent_json) the cockpit decodes. One
// EvaluateIntentJSON call fans out to N NormalizedIntents (N = 1 for SEPA/ORB
// per symbol, 2 per pair for Pairs, 1 per ETF for Sector).
type NormalizedIntent struct {
	// StrategyID is the LOGICAL strategy id inside the payload
	// (sepa|pairs|sector_rotation|intraday_breakout) — the live.signal_intents
	// CHECK discriminator, distinct from the engine/allocator id.
	StrategyID string
	// Symbol is the per-name instrument the intent is about.
	Symbol string
	// State is the SignalState (no_setup|forming|buy|hold|exit|stop_hit).
	State domain.SignalState
	// Strength is 0..100.
	Strength float64
	// ProximityToTriggerPct is nil when not applicable.
	ProximityToTriggerPct *float64
	// Generation is the per-generator monotonic counter.
	Generation int64
	// Payload is the spec-faithful snake_case intent object (the unwrapped
	// SignalIntentUnion variant). Marshals to a JSON object.
	Payload any
}

// IntentJSON renders the payload as a JSON object (the live.signal_intents
// .intent column and the inner intent_json of the Redis SignalIntentUpdate
// envelope, §5.9). It errors if the payload does not marshal to a JSON object
// (a programming error — every variant does).
func (n NormalizedIntent) IntentJSON() (json.RawMessage, error) {
	b, err := json.Marshal(n.Payload)
	if err != nil {
		return nil, fmt.Errorf("publish: marshal intent payload (%s/%s): %w", n.StrategyID, n.Symbol, err)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, fmt.Errorf("publish: intent payload for %s/%s is not a JSON object: %s", n.StrategyID, n.Symbol, b)
	}
	return b, nil
}

// NormalizeIntent flattens one EvaluateIntentJSON result into zero or more
// NormalizedIntents. An unknown concrete type is an error (a new strategy must
// register its conversion here — fail loudly rather than silently drop signals).
func NormalizeIntent(v any) ([]NormalizedIntent, error) {
	switch t := v.(type) {
	case sepa.SignalIntent:
		return []NormalizedIntent{normalizeSEPA(t)}, nil
	case *sepa.SignalIntent:
		return []NormalizedIntent{normalizeSEPA(*t)}, nil
	case orb.SignalIntent:
		return []NormalizedIntent{normalizeORB(t)}, nil
	case *orb.SignalIntent:
		return []NormalizedIntent{normalizeORB(*t)}, nil
	case domain.PairsSignalIntent:
		return []NormalizedIntent{normalizePairs(t)}, nil
	case []domain.PairsSignalIntent:
		out := make([]NormalizedIntent, 0, len(t))
		for _, it := range t {
			out = append(out, normalizePairs(it))
		}
		return out, nil
	case domain.SectorRotationIntent:
		return []NormalizedIntent{normalizeSector(t)}, nil
	case []domain.SectorRotationIntent:
		out := make([]NormalizedIntent, 0, len(t))
		for _, it := range t {
			out = append(out, normalizeSector(it))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("publish: unsupported intent type %T (register its NormalizeIntent conversion)", v)
	}
}

// normalizeSEPA converts the local sepa.SignalIntent (no json tags) to the
// domain.SEPASignalIntent (byte-identical Python field tags) and the
// discriminator columns. Decimal price strings ("" == nil) become *domain.Price.
func normalizeSEPA(s sepa.SignalIntent) NormalizedIntent {
	d := domain.NewSEPASignalIntent()
	d.Symbol = s.Symbol
	d.State = domain.SignalState(s.State)
	d.Strength = s.Strength
	d.ProximityToTriggerPct = s.ProximityToTriggerP
	d.UpdatedAt = s.UpdatedAt.UTC()
	d.Generation = int64(s.Generation)
	d.Grade = s.Grade
	d.TrendTemplatePass = s.TrendTemplatePass
	d.BaseAgeDays = s.BaseAgeDays
	d.BaseDepthPct = s.BaseDepthPct
	d.VolumeDryup = s.VolumeDryup
	d.PivotPrice = priceStrPtr(s.PivotPrice)
	d.StopPrice = priceStrPtr(s.StopPrice)
	d.RSRank = s.RSRank
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDSEPA,
		Symbol:                s.Symbol,
		State:                 domain.SignalState(s.State),
		Strength:              s.Strength,
		ProximityToTriggerPct: s.ProximityToTriggerP,
		Generation:            int64(s.Generation),
		Payload:               d,
	}
}

// normalizeORB converts the local orb.SignalIntent to domain.IntradayBreakoutIntent.
func normalizeORB(s orb.SignalIntent) NormalizedIntent {
	d := domain.NewIntradayBreakoutIntent()
	d.Symbol = s.Symbol
	d.State = domain.SignalState(s.State)
	d.Strength = s.Strength
	d.ProximityToTriggerPct = s.ProximityToTriggerPct
	d.UpdatedAt = s.UpdatedAt.UTC()
	d.Generation = int64(s.Generation)
	d.ORBHigh = priceStrPtr(s.ORBHigh)
	d.ORBLow = priceStrPtr(s.ORBLow)
	d.ATRAtOpen = priceStrPtr(s.ATRAtOpen) // always nil (reserved)
	if s.EntryWindowEnd != nil {
		w := s.EntryWindowEnd.UTC()
		d.EntryWindowEnd = &w
	}
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDIntradayBreakout,
		Symbol:                s.Symbol,
		State:                 domain.SignalState(s.State),
		Strength:              s.Strength,
		ProximityToTriggerPct: s.ProximityToTriggerPct,
		Generation:            int64(s.Generation),
		Payload:               d,
	}
}

// normalizePairs flattens one already-domain PairsSignalIntent leg. The
// per-name symbol is the leg's own ticker (the long or short leg), so the UI
// dedup key (symbol, strategy_id) addresses each leg distinctly.
func normalizePairs(it domain.PairsSignalIntent) NormalizedIntent {
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDPairs,
		Symbol:                it.Symbol,
		State:                 it.State,
		Strength:              it.Strength,
		ProximityToTriggerPct: it.ProximityToTriggerPct,
		Generation:            it.Generation,
		Payload:               it,
	}
}

// normalizeSector flattens one already-domain SectorRotationIntent (per ETF).
func normalizeSector(it domain.SectorRotationIntent) NormalizedIntent {
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDSectorRotation,
		Symbol:                it.Symbol,
		State:                 it.State,
		Strength:              it.Strength,
		ProximityToTriggerPct: it.ProximityToTriggerPct,
		Generation:            it.Generation,
		Payload:               it,
	}
}

// priceStrPtr parses a str(Decimal) price ("" == nil) into a *domain.Price.
// A non-empty value that fails to parse is dropped to nil (the reference's
// "" == nil convention treats an unparseable price as absent rather than
// crashing the publish path).
func priceStrPtr(s string) *domain.Price {
	if s == "" {
		return nil
	}
	p, err := domain.ParsePrice(s)
	if err != nil {
		return nil
	}
	return &p
}
