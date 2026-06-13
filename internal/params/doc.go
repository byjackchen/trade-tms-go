// Package params is the strategy-parameter resolution and typing layer.
//
// It is the Go counterpart of the Python reference's params system
// (src/strategies/params/loader.py + the per-strategy *SignalGeneratorConfig
// dataclasses). Responsibilities, in three layers:
//
//  1. Declaration model. A strategy's parameter document is the JSON shape
//     {strategy, schema_version, display, allocation, metadata, parameters,
//     constraints}. The searchable parameter spec (per-param float/int/str/list
//     with optional numeric search bounds, in-file order, and clamp_high /
//     clamp_low constraint expressions) is parsed by the existing
//     internal/hyperopt loader — this package REUSES hyperopt.StrategyParams /
//     hyperopt.ParseStrategyParams rather than duplicating it, and layers the
//     document-level display/allocation fields on top (Document).
//
//  2. Resolution. Given a strategy id, resolve the active parameter document
//     with the same precedence the Python loader uses (env-dir -> baseline),
//     adapted to the P0 DB schema:
//
//     DB active_params -> param_sets  (the runs/active_params equivalent)
//     -> file dir TMS_STRATEGY_PARAMS_DIR  (only when <strategy>.json exists)
//     -> embedded baseline             (package-shipped defaults)
//
//     Resolution is per-strategy with baseline fallback: a partial promotion
//     (a sepa-only tuned set) still serves sector_rotation/pairs from baseline
//     instead of failing (loader.py:69-96; spec strategy-sepa.md §1.4).
//
//  3. Typing + validation. Each strategy consumes a typed parameter struct
//     (SEPAParams, PairsParams, SectorRotationParams, IntradayBreakoutParams).
//     Load resolves the document, decodes the parameter map into the typed
//     struct, and runs the same runtime validation the Python __post_init__
//     does (bounds such as risk_pct in (0,100], exit_z < entry_z, top_k in
//     [1,len(universe)], eod_exit_time HH:MM, IANA timezone, etc.).
//
// All numeric values are float64 / int64 mirroring Python's float / int, so a
// resolved typed struct matches what the Python SignalGeneratorConfig would
// hold for the same document (locked decision 4).
package params
