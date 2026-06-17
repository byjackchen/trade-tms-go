package params

// document.go layers the document-level display block on top of the searchable
// parameter spec parsed by internal/hyperopt. The Python loader.py intentionally
// ignores display (it only reads strategy, schema_version, metadata, parameters,
// constraints), but it is part of the on-disk JSON and the P0 DB payload
// (migrations/000003_strategy.up.sql stores the FULL document). Document
// therefore preserves it verbatim while delegating param parsing/validation to
// hyperopt.ParseStrategyParams.
//
// The strategy param payloads may still physically carry an "allocation" block,
// but the Model (internal/model) is the sole owner of capital allocation now, so
// Document neither parses nor exposes it (see docs/concept-alignment.md §3.3).

import (
	"encoding/json"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
)

// Display is the optional human-facing block (display.description).
type Display struct {
	Description string `json:"description,omitempty"`
}

// Document is a fully-parsed parameter document: the searchable spec (params,
// constraints, schema_version, metadata) plus the document-level display block.
// Raw retains the exact source bytes so the document can be re-persisted to
// param_sets.payload without lossy round-tripping.
type Document struct {
	Params  *hyperopt.StrategyParams
	Display *Display
	Raw     json.RawMessage

	// Source records where this document was resolved from, for diagnostics
	// and audit (see Origin* constants).
	Source Origin
}

// Origin tags where a resolved Document came from, mirroring the Python
// resolution order (db active_params / file env-dir / embedded baseline).
type Origin string

const (
	OriginDB       Origin = "db"       // tms.active_params -> tms.param_sets
	OriginFile     Origin = "file"     // TMS_STRATEGY_PARAMS_DIR/<strategy>.json
	OriginBaseline Origin = "baseline" // embedded package default
)

// ParseDocument parses a full parameter document for the requested strategy. It
// runs hyperopt's [MUST-MATCH] validation (strategy field present + matching,
// schema_version allowed, type allowlist, search-only-on-numeric, constraint
// kind allowlist, default required) and additionally decodes the document-level
// display block. Raw is set to the input bytes verbatim.
func ParseDocument(raw []byte, strategy string) (*Document, error) {
	sp, err := hyperopt.ParseStrategyParams(raw, strategy)
	if err != nil {
		return nil, err
	}
	var top struct {
		Display *Display `json:"display"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("params: %s: bad display block: %w", strategy, err)
	}
	return &Document{
		Params:  sp,
		Display: top.Display,
		Raw:     append(json.RawMessage(nil), raw...),
	}, nil
}

// Defaults returns {name: default} for every parameter in file order, decoded to
// Go values (float64 for numbers, string, []any for lists) — hyperopt.DefaultsDict.
func (d *Document) Defaults() (map[string]any, error) {
	return hyperopt.DefaultsDict(d.Params)
}
