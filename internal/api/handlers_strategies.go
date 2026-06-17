package api

// handlers_strategies.go serves the strategy registry the UI Strategies section
// renders:
//
//	GET /api/v1/strategies        list the 4 production strategies (metadata +
//	                              active params + allocation + enabled)
//	GET /api/v1/strategies/{id}   one strategy: metadata + active param values +
//	                              full param schema (defaults + search bounds)
//
// The active param document is resolved with the SAME precedence the engine and
// the Python loader use (DB active_params -> file env-dir -> embedded baseline;
// internal/params.Resolver). The param SCHEMA (type / search bounds / order /
// description) comes from the resolved hyperopt.StrategyParams so a promoted
// (tuned) document and the baseline both render with their own bounds.
//
// Strategy id vocabulary: the canonical loader/baseline stems
// (sepa|sector_rotation|pairs|intraday_breakout). Each carries a BacktestID —
// the token POST /api/v1/backtests accepts — which differs only for ORB
// (loader stem "intraday_breakout" -> backtest token "orb"). The UI launches a
// backtest with BacktestID and links the detail page by ID.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// ErrStrategyNotFound is returned by a StrategyReader for an unknown id.
var ErrStrategyNotFound = errors.New("strategy not found")

// strategyDescriptor is the static registry row: the canonical loader id, the
// backtest token, and the display label. Description / allocation / params are
// resolved per-request from the live params document (so a promotion shows up
// immediately without a process restart).
type strategyDescriptor struct {
	ID         string // loader/baseline stem
	BacktestID string // POST /backtests strategy token
	Label      string
}

// strategyRegistry is the fixed display order the UI list renders. It mirrors
// the four engine strategies (assembly.go: sepa|sector_rotation|pairs|orb).
// intraday_breakout (ORB) is included even though it is NOT a hyperopt search
// space (search_spaces.go) — it still has an embedded baseline + schema.
var strategyRegistry = []strategyDescriptor{
	{ID: params.StrategySEPA, BacktestID: "sepa", Label: "SEPA"},
	{ID: params.StrategySectorRotation, BacktestID: "sector_rotation", Label: "Sector Rotation"},
	{ID: params.StrategyPairs, BacktestID: "pairs", Label: "Pairs"},
	{ID: params.StrategyIntradayBreakout, BacktestID: "orb", Label: "ORB"},
}

func descriptorFor(id string) (strategyDescriptor, bool) {
	for _, d := range strategyRegistry {
		if d.ID == id {
			return d, true
		}
	}
	return strategyDescriptor{}, false
}

// ---------------------------------------------------------------------------
// Concrete reader: params.Loader + hyperopt schema.
// ---------------------------------------------------------------------------

// strategyMetaReader resolves StrategyMeta from a params.Loader. It is the
// production StrategyReader; tests inject a stub.
type strategyMetaReader struct {
	loader *params.Loader
}

// NewStrategyReader builds the production StrategyReader. db may be nil
// (file/baseline-only) and dir may be "" (no env override) — the same modes the
// engine's loader supports.
func NewStrategyReader(db params.PayloadReader, dir string) StrategyReader {
	return &strategyMetaReader{loader: params.NewLoader(db, dir)}
}

func (r *strategyMetaReader) ListStrategies(ctx context.Context) ([]StrategyMeta, error) {
	out := make([]StrategyMeta, 0, len(strategyRegistry))
	for _, d := range strategyRegistry {
		m, err := r.resolve(ctx, d)
		if err != nil {
			// Keep the row but mark it: one malformed promoted doc must not
			// blank the entire registry the UI shows.
			out = append(out, StrategyMeta{
				ID:         d.ID,
				BacktestID: d.BacktestID,
				Label:      d.Label,
				Parameters: []ParamSchema{},
				Error:      err.Error(),
			})
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

func (r *strategyMetaReader) GetStrategy(ctx context.Context, id string) (*StrategyMeta, error) {
	d, ok := descriptorFor(id)
	if !ok {
		return nil, ErrStrategyNotFound
	}
	return r.resolve(ctx, d)
}

// resolve loads the active document for one descriptor and projects it to a
// StrategyMeta (metadata + schema + resolved active values).
func (r *strategyMetaReader) resolve(ctx context.Context, d strategyDescriptor) (*StrategyMeta, error) {
	doc, err := r.loader.Resolve(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	values, err := doc.Defaults()
	if err != nil {
		return nil, err
	}
	sp := doc.Params
	schema := make([]ParamSchema, 0, len(sp.Parameters))
	for _, spec := range sp.Parameters {
		ps := ParamSchema{Name: spec.Name, Type: spec.Type}
		var def any
		if err := json.Unmarshal(spec.Default, &def); err == nil {
			ps.Default = def
		}
		if spec.Search != nil {
			low := spec.Search.Low
			high := spec.Search.High
			ps.SearchLow = &low
			ps.SearchHigh = &high
		}
		if spec.Description != nil {
			ps.Description = *spec.Description
		}
		schema = append(schema, ps)
	}

	desc := ""
	if doc.Display != nil {
		desc = doc.Display.Description
	}
	return &StrategyMeta{
		ID:              d.ID,
		BacktestID:      d.BacktestID,
		Label:           d.Label,
		Description:     desc,
		ParamsSource:    string(doc.Source),
		SchemaVersion:   sp.SchemaVersion,
		ParametersCount: len(schema),
		Parameters:      schema,
		ActiveValues:    values,
		RawDoc:          doc.Raw,
	}, nil
}

// Compile-time assertion that the concrete reader satisfies the seam.
var _ StrategyReader = (*strategyMetaReader)(nil)

// hyperopt is referenced to keep the schema-vocabulary documentation honest at
// the import site (the registered search spaces are sepa/sector_rotation/pairs).
var _ = hyperopt.SearchSpaceStrategies

// ---------------------------------------------------------------------------
// GET /api/v1/strategies
// ---------------------------------------------------------------------------

func (s *Server) handleStrategyList(w http.ResponseWriter, r *http.Request) {
	if s.strat == nil {
		writeError(w, http.StatusNotImplemented, CodeInternal, "strategy registry not configured")
		return
	}
	list, err := s.strat.ListStrategies(r.Context())
	if err != nil {
		internalError(w, s.log, "strategy list", err)
		return
	}
	if list == nil {
		list = []StrategyMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"strategies": list})
}

// ---------------------------------------------------------------------------
// GET /api/v1/strategies/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleStrategyGet(w http.ResponseWriter, r *http.Request) {
	if s.strat == nil {
		writeError(w, http.StatusNotImplemented, CodeInternal, "strategy registry not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	m, err := s.strat.GetStrategy(r.Context(), id)
	if errors.Is(err, ErrStrategyNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("strategy %q not found (want sepa|sector_rotation|pairs|intraday_breakout)", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "strategy get", err)
		return
	}
	body := map[string]any{"strategy": m}
	// Emit the canonical params document under "payload" so ground-truth tooling
	// can read the {name:{default,...}} parameters map directly (the inline
	// `parameters` array is the UI's render shape).
	if len(m.RawDoc) > 0 {
		body["payload"] = jsonRaw(m.RawDoc)
	}
	writeJSON(w, http.StatusOK, body)
}
