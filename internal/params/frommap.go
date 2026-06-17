package params

// frommap.go exposes the typed-from-map decoders the hyperopt orchestrator needs
// to build a strategy's typed, validated params from a resolved parameter map
// with searched values overlaid. The optimizer resolves the baseline defaults,
// overlays the clamped per-trial overrides for the searched keys, and calls one
// of these to obtain the same typed *Params struct the engine assembly consumes
// — running the same config validation (typed.go) so an out-of-bounds
// suggested value FAILs the trial.
//
// The input map is the merged {name: Go value} map (numbers as float64, lists as
// []any, strings as string) — i.e. Document.Defaults() with searched keys
// replaced by the optimizer's clamped float values.

// SEPAFromMap decodes + validates a merged SEPA param map.
func SEPAFromMap(m map[string]any) (SEPAParams, error) { return sepaFromMap(pmap(m)) }

// PairsFromMap decodes + validates a merged Pairs param map.
func PairsFromMap(m map[string]any) (PairsParams, error) { return pairsFromMap(pmap(m)) }

// SectorRotationFromMap decodes + validates a merged Sector Rotation param map.
func SectorRotationFromMap(m map[string]any) (SectorRotationParams, error) {
	return sectorFromMap(pmap(m))
}

// IntradayBreakoutFromMap decodes + validates a merged Intraday ORB param map.
func IntradayBreakoutFromMap(m map[string]any) (IntradayBreakoutParams, error) {
	return intradayFromMap(pmap(m))
}
