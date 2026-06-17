package publish

// intent.go normalizes the heterogeneous strategy signal values returned by
// engine.SignalEvaluator.EvaluateSignalJSON into a single, spec-faithful shape
// the persistence + Redis layers consume.
//
// Every strategy adapter now hands publish a canonical DOMAIN signal type (the
// SANCTIONED domain bridge lives in each adapter — modularization-review.md §E3):
//
//   - SEPA  -> domain.SEPASignal        (one per symbol; sepaadapter bridge)
//   - ORB   -> domain.IntradayBreakoutSignal   (one per symbol; orbadapter bridge)
//   - Pairs -> []domain.PairsSignal      (2N legs)
//   - Sector-> []domain.SectorRotationSignal   (one per ETF)
//
// publish therefore switches ONLY on domain signal types and imports no concrete
// strategy package — the local sepa.SignalSnapshot / orb.SignalSnapshot never reach
// here (their local→domain conversion was relocated into sepaadapter/orbadapter,
// the only packages that legitimately import both the zero-domain pure strategy
// package and domain).
//
// We MUST persist the canonical snake_case wire shape (api-ws-redis.md §2.6/§5.9
// — the SignalUnion the cockpit decodes), and we MUST derive the
// tms.signals discriminator columns (strategy_id, symbol, state,
// strength, proximity_to_trigger_pct, generation) so the row's CHECK
// constraints are satisfied and the UI's (symbol, strategy_id) dedup works.

import (
	"encoding/json"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// NormalizedSignal is one strategy signal flattened to the persistence + wire
// contract: the discriminator columns of tms.signals plus the full
// snake_case payload object (signal_json) the cockpit decodes. One
// EvaluateSignalJSON call fans out to N NormalizedSignals (N = 1 for SEPA/ORB
// per symbol, 2 per pair for Pairs, 1 per ETF for Sector).
type NormalizedSignal struct {
	// StrategyID is the LOGICAL strategy id inside the payload
	// (sepa|pairs|sector_rotation|intraday_breakout) — the tms.signals
	// CHECK discriminator, distinct from the engine/allocator id.
	StrategyID string
	// Symbol is the per-name instrument the signal is about.
	Symbol string
	// State is the SignalState (no_setup|forming|buy|hold|exit|stop_hit).
	State domain.SignalState
	// Strength is 0..100.
	Strength float64
	// ProximityToTriggerPct is nil when not applicable.
	ProximityToTriggerPct *float64
	// Generation is the per-generator monotonic counter.
	Generation int64
	// Payload is the spec-faithful snake_case signal object (the unwrapped
	// SignalUnion variant). Marshals to a JSON object.
	Payload any
}

// SignalJSON renders the payload as a JSON object (the tms.signals
// .signal column and the inner signal_json of the Redis SignalUpdate
// envelope, §5.9). It errors if the payload does not marshal to a JSON object
// (a programming error — every variant does).
func (n NormalizedSignal) SignalJSON() (json.RawMessage, error) {
	b, err := json.Marshal(n.Payload)
	if err != nil {
		return nil, fmt.Errorf("publish: marshal signal payload (%s/%s): %w", n.StrategyID, n.Symbol, err)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, fmt.Errorf("publish: signal payload for %s/%s is not a JSON object: %s", n.StrategyID, n.Symbol, b)
	}
	return b, nil
}

// NormalizeSignal flattens one EvaluateSignalJSON result into zero or more
// NormalizedSignals. An unknown concrete type is an error (a new strategy must
// register its conversion here — fail loudly rather than silently drop signals).
func NormalizeSignal(v any) ([]NormalizedSignal, error) {
	switch t := v.(type) {
	case domain.SEPASignal:
		return []NormalizedSignal{normalizeSEPA(t)}, nil
	case *domain.SEPASignal:
		return []NormalizedSignal{normalizeSEPA(*t)}, nil
	case domain.IntradayBreakoutSignal:
		return []NormalizedSignal{normalizeORB(t)}, nil
	case *domain.IntradayBreakoutSignal:
		return []NormalizedSignal{normalizeORB(*t)}, nil
	case domain.PairsSignal:
		return []NormalizedSignal{normalizePairs(t)}, nil
	case []domain.PairsSignal:
		out := make([]NormalizedSignal, 0, len(t))
		for _, it := range t {
			out = append(out, normalizePairs(it))
		}
		return out, nil
	case domain.SectorRotationSignal:
		return []NormalizedSignal{normalizeSector(t)}, nil
	case []domain.SectorRotationSignal:
		out := make([]NormalizedSignal, 0, len(t))
		for _, it := range t {
			out = append(out, normalizeSector(it))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("publish: unsupported signal type %T (register its NormalizeSignal conversion)", v)
	}
}

// normalizeSEPA flattens one already-domain SEPASignal (one per symbol),
// extracting the tms.signals discriminator columns. The local→domain
// field mapping was relocated into sepaadapter (the sanctioned bridge, §E3); this
// only carries the canonical payload + its discriminators.
func normalizeSEPA(d domain.SEPASignal) NormalizedSignal {
	return NormalizedSignal{
		StrategyID:            domain.StrategyIDSEPA,
		Symbol:                d.Symbol,
		State:                 d.State,
		Strength:              d.Strength,
		ProximityToTriggerPct: d.ProximityToTriggerPct,
		Generation:            d.Generation,
		Payload:               d,
	}
}

// normalizeORB flattens one already-domain IntradayBreakoutSignal (one per
// symbol). The local→domain mapping was relocated into orbadapter (§E3).
func normalizeORB(d domain.IntradayBreakoutSignal) NormalizedSignal {
	return NormalizedSignal{
		StrategyID:            domain.StrategyIDIntradayBreakout,
		Symbol:                d.Symbol,
		State:                 d.State,
		Strength:              d.Strength,
		ProximityToTriggerPct: d.ProximityToTriggerPct,
		Generation:            d.Generation,
		Payload:               d,
	}
}

// normalizePairs flattens one already-domain PairsSignal leg. The
// per-name symbol is the leg's own ticker (the long or short leg), so the UI
// dedup key (symbol, strategy_id) addresses each leg distinctly.
func normalizePairs(it domain.PairsSignal) NormalizedSignal {
	return NormalizedSignal{
		StrategyID:            domain.StrategyIDPairs,
		Symbol:                it.Symbol,
		State:                 it.State,
		Strength:              it.Strength,
		ProximityToTriggerPct: it.ProximityToTriggerPct,
		Generation:            it.Generation,
		Payload:               it,
	}
}

// normalizeSector flattens one already-domain SectorRotationSignal (per ETF).
func normalizeSector(it domain.SectorRotationSignal) NormalizedSignal {
	return NormalizedSignal{
		StrategyID:            domain.StrategyIDSectorRotation,
		Symbol:                it.Symbol,
		State:                 it.State,
		Strength:              it.Strength,
		ProximityToTriggerPct: it.ProximityToTriggerPct,
		Generation:            it.Generation,
		Payload:               it,
	}
}
