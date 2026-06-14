package publish

// intent.go normalizes the heterogeneous strategy intent values returned by
// engine.IntentEvaluator.EvaluateIntentJSON into a single, spec-faithful shape
// the persistence + Redis layers consume.
//
// Every strategy adapter now hands publish a canonical DOMAIN intent type (the
// SANCTIONED domain bridge lives in each adapter — modularization-review.md §E3):
//
//   - SEPA  -> domain.SEPASignalIntent        (one per symbol; sepaadapter bridge)
//   - ORB   -> domain.IntradayBreakoutIntent   (one per symbol; orbadapter bridge)
//   - Pairs -> []domain.PairsSignalIntent      (2N legs)
//   - Sector-> []domain.SectorRotationIntent   (one per ETF)
//
// publish therefore switches ONLY on domain intent types and imports no concrete
// strategy package — the local sepa.SignalIntent / orb.SignalIntent never reach
// here (their local→domain conversion was relocated into sepaadapter/orbadapter,
// the only packages that legitimately import both the zero-domain pure strategy
// package and domain).
//
// We MUST persist the canonical snake_case wire shape (api-ws-redis.md §2.6/§5.9
// — the SignalIntentUnion the cockpit decodes), and we MUST derive the
// live.signal_intents discriminator columns (strategy_id, symbol, state,
// strength, proximity_to_trigger_pct, generation) so the row's CHECK
// constraints are satisfied and the UI's (symbol, strategy_id) dedup works.

import (
	"encoding/json"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
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
	case domain.SEPASignalIntent:
		return []NormalizedIntent{normalizeSEPA(t)}, nil
	case *domain.SEPASignalIntent:
		return []NormalizedIntent{normalizeSEPA(*t)}, nil
	case domain.IntradayBreakoutIntent:
		return []NormalizedIntent{normalizeORB(t)}, nil
	case *domain.IntradayBreakoutIntent:
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

// normalizeSEPA flattens one already-domain SEPASignalIntent (one per symbol),
// extracting the live.signal_intents discriminator columns. The local→domain
// field mapping was relocated into sepaadapter (the sanctioned bridge, §E3); this
// only carries the canonical payload + its discriminators.
func normalizeSEPA(d domain.SEPASignalIntent) NormalizedIntent {
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDSEPA,
		Symbol:                d.Symbol,
		State:                 d.State,
		Strength:              d.Strength,
		ProximityToTriggerPct: d.ProximityToTriggerPct,
		Generation:            d.Generation,
		Payload:               d,
	}
}

// normalizeORB flattens one already-domain IntradayBreakoutIntent (one per
// symbol). The local→domain mapping was relocated into orbadapter (§E3).
func normalizeORB(d domain.IntradayBreakoutIntent) NormalizedIntent {
	return NormalizedIntent{
		StrategyID:            domain.StrategyIDIntradayBreakout,
		Symbol:                d.Symbol,
		State:                 d.State,
		Strength:              d.Strength,
		ProximityToTriggerPct: d.ProximityToTriggerPct,
		Generation:            d.Generation,
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
