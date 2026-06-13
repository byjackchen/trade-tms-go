package params

// document.go layers the document-level fields (display, allocation) on top of
// the searchable parameter spec parsed by internal/hyperopt. The Python
// loader.py intentionally ignores display/allocation (it only reads strategy,
// schema_version, metadata, parameters, constraints), but those fields are part
// of the on-disk JSON and the P0 DB payload (migrations/000003_strategy.up.sql
// stores the FULL document), and downstream consumers (the capital allocator)
// read allocation.capital_pct. Document therefore preserves them verbatim while
// delegating param parsing/validation to hyperopt.ParseStrategyParams.

import (
	"encoding/json"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
)

// Display is the optional human-facing block (display.description).
type Display struct {
	Description string `json:"description,omitempty"`
}

// Allocation is the optional strategy-level capital block. capital_pct feeds
// portfolio.NewAllocator; active gates whether the strategy trades. Pointers
// distinguish "absent" from a zero value (an absent allocation block must not
// be read as capital_pct=0 / active=false).
type Allocation struct {
	CapitalPct *float64 `json:"capital_pct,omitempty"`
	Active     *bool    `json:"active,omitempty"`
}

// Document is a fully-parsed parameter document: the searchable spec (params,
// constraints, schema_version, metadata) plus the document-level display /
// allocation blocks. Raw retains the exact source bytes so the document can be
// re-persisted to param_sets.payload without lossy round-tripping.
type Document struct {
	Params     *hyperopt.StrategyParams
	Display    *Display
	Allocation *Allocation
	Raw        json.RawMessage

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
// display/allocation blocks. Raw is set to the input bytes verbatim.
func ParseDocument(raw []byte, strategy string) (*Document, error) {
	sp, err := hyperopt.ParseStrategyParams(raw, strategy)
	if err != nil {
		return nil, err
	}
	var top struct {
		Display    *Display    `json:"display"`
		Allocation *Allocation `json:"allocation"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("params: %s: bad display/allocation block: %w", strategy, err)
	}
	return &Document{
		Params:     sp,
		Display:    top.Display,
		Allocation: top.Allocation,
		Raw:        append(json.RawMessage(nil), raw...),
	}, nil
}

// Defaults returns {name: default} for every parameter in file order, decoded to
// Go values (float64 for numbers, string, []any for lists) — hyperopt.DefaultsDict.
func (d *Document) Defaults() (map[string]any, error) {
	return hyperopt.DefaultsDict(d.Params)
}

// CapitalPct returns the allocation capital fraction and whether it was present.
func (d *Document) CapitalPct() (float64, bool) {
	if d.Allocation == nil || d.Allocation.CapitalPct == nil {
		return 0, false
	}
	return *d.Allocation.CapitalPct, true
}

// Active reports whether the strategy is marked active. An absent allocation
// block or absent `active` flag is treated as active (the Python baselines that
// omit it — e.g. intraday_breakout — are run normally).
func (d *Document) Active() bool {
	if d.Allocation == nil || d.Allocation.Active == nil {
		return true
	}
	return *d.Allocation.Active
}
