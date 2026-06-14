package params

// resolve.go is the document-resolution layer: given a strategy id, produce the
// active parameter Document with the same precedence the Python loader uses
// (loader.py:64-96), adapted to the P0 DB schema:
//
//	DB active_params -> param_sets      (runs/active_params equivalent)
//	-> file TMS_STRATEGY_PARAMS_DIR/<strategy>.json (env-dir override)
//	-> embedded baseline                (package-shipped default)
//
// Like Python, resolution is per-strategy with baseline fallback: an absent DB
// row OR an absent file falls through to the next source rather than erroring,
// so a partial promotion (a sepa-only tuned set) still serves the rest from
// baseline (spec strategy-sepa.md §1.4 [MUST-MATCH]).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
)

// ErrNoActivePayload is the sentinel a PayloadReader MAY return to signal "no
// active_params row for this strategy" — the "no promotion = baseline" case
// (migrations/000003_strategy.up.sql: "No row = baseline"). It exists so the
// DB-backed reader (internal/params/paramsdb) can surface pgx.ErrNoRows through
// the PayloadReader seam without leaking the pgx package into the params core.
//
// Resolve treats this sentinel identically to a (nil, nil) result: it falls
// through to the file/baseline sources rather than erroring. The canonical
// no-row signal remains (nil, nil); the sentinel is the equivalent error form.
var ErrNoActivePayload = errors.New("params: no active payload")

// PayloadReader reads the active param document payload for a strategy from the
// DB (tms.active_params -> tms.param_sets.payload). It returns (nil, nil) — or
// (nil, ErrNoActivePayload) — when no active_params row exists for the strategy;
// both are the "no promotion = baseline" case and must fall through to the
// file/baseline sources rather than error.
type PayloadReader interface {
	ActivePayload(ctx context.Context, strategy string) (json.RawMessage, error)
}

// Resolver resolves parameter documents. DB and Dir are both optional: a nil DB
// skips the DB tier (embedded/file-only mode, matching a Python install with no
// TMS_STRATEGY_PARAMS_DIR), an empty Dir skips the file tier.
type Resolver struct {
	DB  PayloadReader // optional; nil = skip DB tier
	Dir string        // optional; "" = skip file tier (TMS_STRATEGY_PARAMS_DIR)
}

// Resolve returns the active Document for strategy, applying the precedence
// chain. The strategy id is validated to be one of the known baseline strategies
// so a typo fails fast rather than silently resolving nothing.
func (r *Resolver) Resolve(ctx context.Context, strategy string) (*Document, error) {
	if strategy == "" {
		return nil, fmt.Errorf("params: empty strategy id")
	}

	// 1. DB active_params -> param_sets.payload.
	if r.DB != nil {
		raw, err := r.DB.ActivePayload(ctx, strategy)
		// ErrNoActivePayload is the error form of the (nil, nil) "no promotion"
		// signal — fall through to file/baseline rather than error.
		if err != nil && !errors.Is(err, ErrNoActivePayload) {
			return nil, fmt.Errorf("params: db resolve %q: %w", strategy, err)
		}
		if err == nil && raw != nil {
			doc, err := ParseDocument(raw, strategy)
			if err != nil {
				return nil, fmt.Errorf("params: db payload for %q: %w", strategy, err)
			}
			doc.Source = OriginDB
			return doc, nil
		}
	}

	// 2. File env-dir override: only when <strategy>.json exists there
	//    (loader.py:84-86 — partial dirs fall through per-strategy).
	if r.Dir != "" {
		path := filepath.Join(r.Dir, strategy+".json")
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			doc, err := ParseDocument(raw, strategy)
			if err != nil {
				return nil, fmt.Errorf("params: file %s: %w", path, err)
			}
			doc.Source = OriginFile
			return doc, nil
		case errors.Is(err, os.ErrNotExist):
			// fall through to baseline (per-strategy fallback).
		default:
			return nil, fmt.Errorf("params: read %s: %w", path, err)
		}
	}

	// 3. Embedded baseline (package-shipped). hyperopt owns the embed FS and
	//    the identical baseline JSONs; re-read the raw bytes for the document.
	raw, err := hyperopt.BaselineRaw(strategy)
	if err != nil {
		return nil, err
	}
	doc, err := ParseDocument(raw, strategy)
	if err != nil {
		return nil, fmt.Errorf("params: baseline %q: %w", strategy, err)
	}
	doc.Source = OriginBaseline
	return doc, nil
}
